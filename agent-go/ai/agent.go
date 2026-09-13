package ai

import (
	"fmt"
	"log"
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
	log.Printf("[ai] Request: requestId=%s model=%s", req.RequestID, req.Model)

	// Placeholder for AI inference.
	// Real implementation would call OpenAI / NVIDIA / etc. via HTTP.
	// For now, send a mock response.
	time.Sleep(500 * time.Millisecond)

	h.a.Send(map[string]interface{}{
		"type":      "ai_response",
		"requestId": req.RequestID,
		"content":   fmt.Sprintf("AI agent is not yet implemented for model %s. This is a placeholder response.", req.Model),
		"done":      true,
	})
}
