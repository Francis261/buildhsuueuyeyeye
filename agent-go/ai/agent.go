package ai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
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
	log.Printf("[ai] Request: requestId=%s model=%s provider=%s fileCount=%d", req.RequestID, req.Model, req.AgentConfig.Provider, len(req.Files))

	if req.AgentConfig == nil || req.AgentConfig.APIKey == "" {
		h.sendError(req.RequestID, "AI is not configured. No API key provided.")
		return
	}

	// Extract project files to a temp directory if provided.
	var workDir string
	var originalFiles map[string]string
	if len(req.Files) > 0 {
		tmpDir, err := os.MkdirTemp("", "ai-project-*")
		if err != nil {
			h.sendError(req.RequestID, fmt.Sprintf("Failed to create temp directory: %v", err))
			return
		}
		defer os.RemoveAll(tmpDir)
		workDir = tmpDir

		// Extract files
		originalFiles = make(map[string]string)
		for relPath, content := range req.Files {
			originalFiles[relPath] = content
			fullPath := filepath.Join(tmpDir, relPath)
			dir := filepath.Dir(fullPath)
			if err := os.MkdirAll(dir, 0755); err != nil {
				log.Printf("[ai] Failed to create directory %s: %v", dir, err)
				continue
			}
			if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
				log.Printf("[ai] Failed to write file %s: %v", relPath, err)
				continue
			}
		}
		log.Printf("[ai] Extracted %d files to %s", len(req.Files), tmpDir)
	} else {
		workDir = "."
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
	systemPrompt += "\n\nYou have access to the following tools:\n- read: Read a file or directory\n- write: Write content to a file\n- edit: Search and replace in a file\n- bash: Execute a bash command\n\nIMPORTANT RULES:\n- Use the EXACT file paths as they appear in the project (e.g. src/screens/HomeScreen.tsx). Do NOT use placeholder paths like /path/to/project/root.\n- For bash commands, do NOT specify a workdir unless needed. The working directory is already set to the project root.\n- When reading files, use the exact relative path from the project root.\n- When editing files, provide enough context to make the oldString unique."

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
	executor := tools.NewExecutor(workDir)

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
		err := h.runWithTools(req.RequestID, baseURL, req.AgentConfig.APIKey, tryModel, messages, temperature, maxTokens, executor, workDir, originalFiles)
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
func (h *Handler) runWithTools(requestID, baseURL, apiKey, model string, messages []map[string]interface{}, temperature float64, maxTokens int, executor *tools.Executor, workDir string, originalFiles map[string]string) error {
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
		// But some models (like llama) return tool calls as JSON in the text content.
		// Try to detect and parse that.
		if msg.Content != "" && len(msg.ToolCalls) == 0 {
			if extracted := tools.ParseToolCalls(msg.Content); len(extracted) > 0 {
				// Model returned tool calls as text — execute them.
				messages = append(messages, map[string]interface{}{
					"role":    "assistant",
					"content": msg.Content,
				})
				for _, tc := range extracted {
					h.a.Send(map[string]interface{}{
						"type":      "ai_tool_call",
						"requestId": requestID,
						"toolName":  tc.Name,
						"toolId":    tc.ID,
					})
					result := executor.ExecuteToolCall(tc)
					h.a.Send(map[string]interface{}{
						"type":      "ai_tool_result",
						"requestId": requestID,
						"toolName":  result.Name,
						"toolId":    result.ToolCallID,
						"output":    result.Output,
						"error":     result.Error,
					})
					toolResult := result.Output
					if result.Error != "" {
						toolResult = "Error: " + result.Error
					}
					messages = append(messages, map[string]interface{}{
						"role":         "tool",
						"tool_call_id": tc.ID,
						"content":      toolResult,
					})
				}
				continue
			}
		}

		// No tool calls — this is the final text response.
		// Before sending the response, sync any changed files back to the server.
		if len(originalFiles) > 0 && workDir != "." {
			h.syncChangedFiles(requestID, workDir, originalFiles)
		}

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

// syncChangedFiles compares the current files with the original and sends changes back to the server.
func (h *Handler) syncChangedFiles(requestID, workDir string, originalFiles map[string]string) {
	changedFiles := make(map[string]string)

	// Directories to exclude from sync
	excludeDirs := map[string]bool{
		"node_modules": true,
		".git":         true,
		".expo":        true,
		"build":        true,
		".gradle":      true,
		"android":      true,
		"ios":          true,
		".next":        true,
		"dist":         true,
		".cache":       true,
		"__pycache__":  true,
		".idea":        true,
		".vscode":      true,
	}

	// Walk the work directory and compare with original files
	err := filepath.Walk(workDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if excludeDirs[info.Name()] && path != workDir {
				return filepath.SkipDir
			}
			return nil
		}

		// Get relative path from workDir
		relPath, err := filepath.Rel(workDir, path)
		if err != nil {
			return nil
		}
		// Normalize path separators
		relPath = filepath.ToSlash(relPath)

		// Skip binary and large files
		if info.Size() > 100*1024 {
			return nil
		}

		// Read current content
		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		currentContent := string(content)
		originalContent, existed := originalFiles[relPath]

		// File is new or changed
		if !existed || currentContent != originalContent {
			changedFiles[relPath] = currentContent
		}

		return nil
	})

	if err != nil {
		log.Printf("[ai] Error walking work directory: %v", err)
		return
	}

	// Check for deleted files
	for relPath := range originalFiles {
		fullPath := filepath.Join(workDir, relPath)
		if _, err := os.Stat(fullPath); os.IsNotExist(err) {
			// File was deleted - send empty content to signal deletion
			changedFiles[relPath] = ""
		}
	}

	if len(changedFiles) > 0 {
		log.Printf("[ai] Syncing %d changed files back to server", len(changedFiles))
		h.a.Send(map[string]interface{}{
			"type":      "source_sync",
			"requestId": requestID,
			"files":     changedFiles,
		})
	}
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

	client := &http.Client{Timeout: 45 * time.Second}
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
