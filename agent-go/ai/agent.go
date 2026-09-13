package ai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"apkbuilder-agent/core"
	"apkbuilder-agent/tools"
)

const maxToolRounds = 20

type Handler struct {
	a core.AgentInterface
}

func NewHandler(a core.AgentInterface) *Handler {
	return &Handler{a: a}
}

func (h *Handler) HandleAIRequest(req core.AIRequest) {
	log.Printf("[ai] Request: requestId=%s model=%s provider=%s", req.RequestID, req.Model, req.AgentConfig.Provider)

	if req.AgentConfig == nil || req.AgentConfig.APIKey == "" {
		h.sendError(req.RequestID, "AI is not configured. No API key provided.")
		return
	}

	// Resolve base URL.
	baseURL := req.AgentConfig.BaseURL
	if baseURL == "" {
		baseURL = resolveProviderBaseURL(req.AgentConfig.Provider)
	}
	baseURL = strings.TrimRight(baseURL, "/")

	// Resolve model.
	model := req.Model
	if model == "" && len(req.AgentConfig.Models) > 0 {
		model = req.AgentConfig.Models[0]
	}
	if model == "" && req.AgentConfig.CustomModel != "" {
		model = req.AgentConfig.CustomModel
	}
	if model == "" {
		model = "gpt-4o-mini"
	}

	// Build system message with tool instructions.
	systemPrompt := req.AgentConfig.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = "You are an expert mobile app code assistant. You have access to tools to read, write, edit files and run bash commands. Use tools to help the user with their coding tasks."
	}
	systemPrompt += "\n\nYou have access to the following tools:\n- read: Read a file or directory\n- write: Write content to a file\n- edit: Search and replace in a file\n- bash: Execute a bash command\n\nWhen you need to use a tool, respond with tool calls in the format specified by the API. After receiving tool results, continue helping the user."

	// Build file context.
	fileContext := ""
	if req.File != nil {
		fileContext = fmt.Sprintf("\n\nCURRENT FILE (%s, %s):\n```\n%s\n```",
			req.File.Path, req.File.Language, truncate(req.File.Content, 6000))
	}

	// Build messages array.
	messages := []map[string]interface{}{
		{
			"role":    "system",
			"content": systemPrompt,
		},
	}
	for _, m := range req.Messages {
		content := m.Content
		if m.Role == "user" && fileContext != "" {
			content += fileContext
		}
		messages = append(messages, map[string]interface{}{
			"role":    m.Role,
			"content": content,
		})
	}

	// Create tool executor.
	executor := tools.NewExecutor(".")

	// Try models in order.
	modelsToTry := dedupModels(append([]string{model}, req.AgentConfig.Models...))

	temperature := req.AgentConfig.Temperature
	if temperature == 0 {
		temperature = 0.4
	}
	maxTokens := req.AgentConfig.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}

	var lastError string
	for _, tryModel := range modelsToTry {
		err := h.runWithTools(req.RequestID, baseURL, req.AgentConfig.APIKey, tryModel, messages, temperature, maxTokens, executor)
		if err != nil {
			lastError = fmt.Sprintf("Model %s error: %v", tryModel, err)
			log.Printf("[ai] %s", lastError)
			continue
		}
		return
	}

	h.sendError(req.RequestID, fmt.Sprintf("All models failed: %s", lastError))
}

// runWithTools executes the AI loop with tool support.
func (h *Handler) runWithTools(requestID, baseURL, apiKey, model string, messages []map[string]interface{}, temperature float64, maxTokens int, executor *tools.Executor) error {
	toolDefs := tools.GetToolDefinitions()

	for round := 0; round < maxToolRounds; round++ {
		// Call the API.
		fullResp, err := h.callAPIWithTools(baseURL, apiKey, model, messages, temperature, maxTokens, toolDefs)
		if err != nil {
			return err
		}

		// Parse the response.
		choice := fullResp.Choices
		if len(choice) == 0 {
			return fmt.Errorf("no choices in response")
		}

		msg := choice[0].Message

		// Check for tool calls.
		if len(msg.ToolCalls) > 0 {
			// Add the assistant message with tool calls to history.
			messages = append(messages, map[string]interface{}{
				"role":      "assistant",
				"content":   msg.Content,
				"tool_calls": msg.ToolCalls,
			})

			// Send tool call status to client.
			for _, tc := range msg.ToolCalls {
				h.a.Send(map[string]interface{}{
					"type":      "ai_tool_call",
					"requestId": requestID,
					"toolName":  tc.Function.Name,
					"toolId":    tc.ID,
				})
			}

			// Execute each tool call.
			for _, tc := range msg.ToolCalls {
				toolCall := tools.ToolCall{
					ID:   tc.ID,
					Name: tc.Function.Name,
					Args: json.RawMessage(tc.Function.Arguments),
				}

				result := executor.ExecuteToolCall(toolCall)

				// Send tool result to client.
				h.a.Send(map[string]interface{}{
					"type":      "ai_tool_result",
					"requestId": requestID,
					"toolName":  result.Name,
					"toolId":    result.ToolCallID,
					"output":    result.Output,
					"error":     result.Error,
				})

				// Add tool result to messages.
				toolResult := result.Output
				if result.Error != "" {
					toolResult = "Error: " + result.Error
				}
				messages = append(messages, map[string]interface{}{
					"role":       "tool",
					"tool_call_id": tc.ID,
					"content":    toolResult,
				})
			}

			// Continue the loop to get the next response.
			continue
		}

		// No tool calls — this is the final text response.
		if msg.Content != "" {
			h.a.Send(map[string]interface{}{
				"type":      "ai_response",
				"requestId": requestID,
				"content":   msg.Content,
				"done":      true,
			})
		} else {
			h.a.Send(map[string]interface{}{
				"type":      "ai_response",
				"requestId": requestID,
				"content":   "(no response)",
				"done":      true,
			})
		}
		return nil
	}

	return fmt.Errorf("exceeded maximum tool rounds (%d)", maxToolRounds)
}

// API response types.
type apiResponse struct {
	Choices []struct {
		Message struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (h *Handler) callAPIWithTools(baseURL, apiKey, model string, messages []map[string]interface{}, temperature float64, maxTokens int, toolDefs []tools.ToolDefinition) (*apiResponse, error) {
	// Convert tool definitions to OpenAI format.
	toolsParam := make([]map[string]interface{}, len(toolDefs))
	for i, td := range toolDefs {
		toolsParam[i] = map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        td.Name,
				"description": td.Description,
				"parameters":  td.Parameters,
			},
		}
	}

	body := map[string]interface{}{
		"model":       model,
		"temperature": temperature,
		"max_tokens":  maxTokens,
		"messages":    messages,
		"tools":       toolsParam,
	}

	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	url := baseURL + "/chat/completions"
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}

	var result apiResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("bad response: %w", err)
	}

	if result.Error != nil {
		return nil, fmt.Errorf("API error: %s", result.Error.Message)
	}

	return &result, nil
}

func (h *Handler) sendError(requestID, msg string) {
	h.a.Send(map[string]interface{}{
		"type":      "ai_response",
		"requestId": requestID,
		"error":     msg,
		"done":      true,
	})
}

func dedupModels(models []string) []string {
	seen := make(map[string]bool)
	unique := make([]string, 0, len(models))
	for _, m := range models {
		if !seen[m] && m != "" {
			seen[m] = true
			unique = append(unique, m)
		}
	}
	return unique
}

func resolveProviderBaseURL(provider string) string {
	switch provider {
	case "openai":
		return "https://api.openai.com/v1"
	case "anthropic":
		return "https://api.anthropic.com/v1"
	case "groq":
		return "https://api.groq.com/openai/v1"
	case "nvidia":
		return "https://integrate.api.nvidia.com/v1"
	case "openrouter":
		return "https://openrouter.ai/api/v1"
	default:
		return "https://api.openai.com/v1"
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
