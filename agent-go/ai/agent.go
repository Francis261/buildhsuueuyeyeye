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
)

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

	// Build system message.
	systemPrompt := req.AgentConfig.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = "You are an expert mobile app code assistant embedded in an APK builder. Help the user write, fix, or explain code. Return only code or a concise answer relevant to the given file context. Use markdown code blocks for code snippets."
	}

	// Build file context.
	fileContext := ""
	if req.File != nil {
		fileContext = fmt.Sprintf("\n\nCURRENT FILE (%s, %s):\n```\n%s\n```",
			req.File.Path, req.File.Language, truncate(req.File.Content, 6000))
	}

	// Build messages array.
	messages := make([]map[string]string, 0, len(req.Messages)+1)
	messages = append(messages, map[string]string{
		"role":    "system",
		"content": systemPrompt,
	})
	for _, m := range req.Messages {
		content := m.Content
		// Append file context to the last user message.
		if m.Role == "user" && fileContext != "" {
			content += fileContext
		}
		messages = append(messages, map[string]string{
			"role":    m.Role,
			"content": content,
		})
	}

	// Try models in order.
	modelsToTry := append([]string{model}, req.AgentConfig.Models...)
	// Deduplicate.
	seen := make(map[string]bool)
	unique := make([]string, 0, len(modelsToTry))
	for _, m := range modelsToTry {
		if !seen[m] && m != "" {
			seen[m] = true
			unique = append(unique, m)
		}
	}

	temperature := req.AgentConfig.Temperature
	if temperature == 0 {
		temperature = 0.4
	}
	maxTokens := req.AgentConfig.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}

	var lastError string
	for _, tryModel := range unique {
		result, err := h.callAPI(baseURL, req.AgentConfig.APIKey, tryModel, messages, temperature, maxTokens)
		if err != nil {
			lastError = fmt.Sprintf("Model %s error: %v", tryModel, err)
			log.Printf("[ai] %s", lastError)
			continue
		}
		if result != "" {
			h.a.Send(map[string]interface{}{
				"type":      "ai_response",
				"requestId": req.RequestID,
				"content":   result,
				"done":      true,
			})
			return
		}
		lastError = fmt.Sprintf("Model %s returned empty response", tryModel)
	}

	h.sendError(req.RequestID, fmt.Sprintf("All models failed: %s", lastError))
}

func (h *Handler) callAPI(baseURL, apiKey, model string, messages []map[string]string, temperature float64, maxTokens int) (string, error) {
	body := map[string]interface{}{
		"model":       model,
		"temperature": temperature,
		"max_tokens":  maxTokens,
		"messages":    messages,
	}

	data, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	url := baseURL + "/chat/completions"
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("API returned %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("bad response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", nil
	}
	return strings.TrimSpace(result.Choices[0].Message.Content), nil
}

func (h *Handler) sendError(requestID, msg string) {
	h.a.Send(map[string]interface{}{
		"type":      "ai_response",
		"requestId": requestID,
		"error":     msg,
		"done":      true,
	})
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
