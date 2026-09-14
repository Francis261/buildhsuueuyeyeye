package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"apkbuilder-agent/core"
	"apkbuilder-agent/tools"
)

const maxToolRounds = 30

// Blocked commands that could escape the sandbox or harm the system.
var blockedCommands = regexp.MustCompile(`(?i)^` +
	`(docker|kubectl|helm|ssh|scp|rsync|curl\s+.*>\s*/|wget\s+.*>\s*/|` +
	`sudo|su\s+|chmod\s+777|rm\s+-rf\s+/|mkfs|dd\s+if=|mount\s|umount\s|` +
	`fdisk|parted|blkid|` +
	`systemctl|service\s|` +
	`nc\s+-|ncat|netcat|socat|` +
	`eval\s|exec\s|` +
	`/etc/passwd|/etc/shadow|/etc/sudoers|` +
	`ssh-keygen|authorized_keys|` +
	`crontab\s+-|at\s+|` +
	`iptables|nftables|firewall-cmd|` +
	`kill\s+-9\s+1|killall|pkill\s+` +
	`)`)

// Path traversal patterns.
var pathTraversal = regexp.MustCompile(`\.\./\.\.`)

type Handler struct {
	a core.AgentInterface

	// Cancel support: tracks active AI requests by requestId.
	cancelMu sync.Mutex
	cancels  map[string]context.CancelFunc
}

func NewHandler(a core.AgentInterface) *Handler {
	return &Handler{a: a, cancels: make(map[string]context.CancelFunc)}
}

// CancelRequest cancels an in-progress AI request.
func (h *Handler) CancelRequest(requestID string) {
	h.cancelMu.Lock()
	defer h.cancelMu.Unlock()
	if cancel, ok := h.cancels[requestID]; ok {
		cancel()
		log.Printf("[ai] Cancelled request %s", requestID)
	}
}

func (h *Handler) HandleAIRequest(req core.AIRequest) {
	log.Printf("[ai] Request: requestId=%s model=%s provider=%s projectId=%s", req.RequestID, req.Model, req.AgentConfig.Provider, req.ProjectID)

	if req.AgentConfig == nil || req.AgentConfig.APIKey == "" {
		h.sendError(req.RequestID, "AI is not configured. No API key provided.")
		return
	}

	// Create cancellable context.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Register cancel func.
	h.cancelMu.Lock()
	h.cancels[req.RequestID] = cancel
	h.cancelMu.Unlock()
	defer func() {
		h.cancelMu.Lock()
		delete(h.cancels, req.RequestID)
		h.cancelMu.Unlock()
	}()

	// Check if already cancelled.
	if ctx.Err() != nil {
		h.sendError(req.RequestID, "Request cancelled.")
		return
	}

	// Get project files — either from ProjectID (HTTP fetch) or inline Files.
	var workDir string
	var originalFiles map[string]string

	if req.ProjectID != "" && req.BackendURL != "" {
		fetchedFiles, err := h.fetchProject(req.BackendURL, req.ProjectID)
		if err != nil {
			h.sendError(req.RequestID, fmt.Sprintf("Failed to fetch project: %v", err))
			return
		}
		originalFiles = fetchedFiles
		tmpDir, err := os.MkdirTemp("", "ai-project-*")
		if err != nil {
			h.sendError(req.RequestID, fmt.Sprintf("Failed to create temp directory: %v", err))
			return
		}
		defer os.RemoveAll(tmpDir)
		workDir = tmpDir
		for relPath, content := range originalFiles {
			fullPath := filepath.Join(tmpDir, relPath)
			dir := filepath.Dir(fullPath)
			os.MkdirAll(dir, 0755)
			os.WriteFile(fullPath, []byte(content), 0644)
		}
		log.Printf("[ai] Fetched %d files from server for project %s", len(originalFiles), req.ProjectID)
	} else if len(req.Files) > 0 {
		tmpDir, err := os.MkdirTemp("", "ai-project-*")
		if err != nil {
			h.sendError(req.RequestID, fmt.Sprintf("Failed to create temp directory: %v", err))
			return
		}
		defer os.RemoveAll(tmpDir)
		workDir = tmpDir
		originalFiles = make(map[string]string)
		for relPath, content := range req.Files {
			originalFiles[relPath] = content
			fullPath := filepath.Join(tmpDir, relPath)
			dir := filepath.Dir(fullPath)
			os.MkdirAll(dir, 0755)
			os.WriteFile(fullPath, []byte(content), 0644)
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
		model = resolveDefaultModel(req.AgentConfig.Provider)
	}

	// Build system message with hardened sandbox instructions.
	systemPrompt := buildHardenedSystemPrompt(req.AgentConfig.SystemPrompt)

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
	// Add provider-specific fallbacks if only one model specified.
	if len(modelsToTry) == 1 {
		if req.AgentConfig.Provider == "nvidia" {
			modelsToTry = append(modelsToTry, "meta/llama-3.1-8b-instruct")
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
	for _, tryModel := range modelsToTry {
		err := h.runWithTools(ctx, req.RequestID, baseURL, req.AgentConfig.APIKey, tryModel, messages, temperature, maxTokens, executor, workDir, originalFiles, req.ProjectID, req.BackendURL)
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
func (h *Handler) runWithTools(ctx context.Context, requestID, baseURL, apiKey, model string, messages []map[string]interface{}, temperature float64, maxTokens int, executor *tools.Executor, workDir string, originalFiles map[string]string, projectId, backendURL string) error {
	toolDefs := tools.GetToolDefinitions()

	// Loop detection: track recent tool call signatures.
	type toolSig struct {
		name string
		args string
	}
	var recentCalls []toolSig
	loopCount := 0
	const maxRepeatedCalls = 3

	for round := 0; round < maxToolRounds; round++ {
		// Check if cancelled.
		if ctx.Err() != nil {
			h.a.Send(map[string]interface{}{
				"type":      "ai_response",
				"requestId": requestID,
				"content":   "Request cancelled.",
				"done":      true,
			})
			return nil
		}

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

		log.Printf("[ai] Round %d: model=%s tool_calls=%d content_len=%d", round, model, len(msg.ToolCalls), len(msg.Content))
		if len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				log.Printf("[ai]   tool_call: %s args=%s", tc.Function.Name, truncate(string(tc.Function.Arguments), 200))
			}
		}

		// Check for tool calls.
		if len(msg.ToolCalls) > 0 {
			// Detect loops: check if the same tool+args was called recently.
			for _, tc := range msg.ToolCalls {
				sig := toolSig{name: tc.Function.Name, args: string(tc.Function.Arguments)}
				matched := 0
				for _, prev := range recentCalls {
					if prev.name == sig.name && prev.args == sig.args {
						matched++
					}
				}
				if matched >= maxRepeatedCalls-1 {
					loopCount++
				} else {
					loopCount = 0
				}
				recentCalls = append(recentCalls, sig)
				if len(recentCalls) > 10 {
					recentCalls = recentCalls[len(recentCalls)-10:]
				}
			}

			// If we've detected a loop, inject a warning and eventually force-stop.
			if loopCount >= 3 {
				h.a.Send(map[string]interface{}{
					"type":      "ai_response",
					"requestId": requestID,
					"content":   "The model appears to be stuck in a loop. Stopping to avoid infinite recursion.",
					"done":      true,
				})
				if len(originalFiles) > 0 && workDir != "." {
					h.syncChangedFiles(requestID, workDir, originalFiles, projectId, backendURL)
				}
				return nil
			}
			if loopCount >= 1 {
				messages = append(messages, map[string]interface{}{
					"role":    "system",
					"content": "STOP! You are repeating the same tool call. You must now provide your final answer as text without calling any more tools. Summarize what you found.",
				})
			}
			// Convert API tool calls to our ToolCall type and build history-compatible tool_calls.
			var toolCallsForHistory []map[string]interface{}
			var toolCalls []tools.ToolCall

			isGemini := strings.Contains(baseURL, "generativelanguage.googleapis.com") || strings.HasPrefix(model, "gemini")
			isAnthropic := strings.Contains(baseURL, "anthropic.com") || strings.Contains(baseURL, "api.anthropic")

			for _, tc := range msg.ToolCalls {
				toolCalls = append(toolCalls, tools.ToolCall{
					ID:   tc.ID,
					Name: tc.Function.Name,
					Args: tc.Function.Arguments,
				})

				if isGemini {
					// Gemini: functionCall inside parts.
					var argsMap map[string]interface{}
					json.Unmarshal(tc.Function.Arguments, &argsMap)
					toolCallsForHistory = append(toolCallsForHistory, map[string]interface{}{
						"functionCall": map[string]interface{}{
							"name": tc.Function.Name,
							"args": argsMap,
						},
					})
				} else if isAnthropic {
					// Anthropic: tool_use content block.
					toolCallsForHistory = append(toolCallsForHistory, map[string]interface{}{
						"type":      "tool_use",
						"id":        tc.ID,
						"name":      tc.Function.Name,
						"input":     json.RawMessage(tc.Function.Arguments),
					})
				} else {
					// OpenAI-compatible.
					toolCallsForHistory = append(toolCallsForHistory, map[string]interface{}{
						"id":   tc.ID,
						"type": "function",
						"function": map[string]interface{}{
							"name":      tc.Function.Name,
							"arguments": json.RawMessage(tc.Function.Arguments),
						},
					})
				}
			}

			// Add the assistant message with tool calls to history.
			if isGemini {
				// Gemini: functionCall goes in parts alongside any text.
				var parts []map[string]interface{}
				if msg.Content != "" {
					parts = append(parts, map[string]interface{}{"text": msg.Content})
				}
				parts = append(parts, toolCallsForHistory...)
				messages = append(messages, map[string]interface{}{
					"role":  "model",
					"parts": parts,
				})
			} else if isAnthropic {
				// Anthropic: content blocks with tool_use.
				var contentBlocks []map[string]interface{}
				if msg.Content != "" {
					contentBlocks = append(contentBlocks, map[string]interface{}{
						"type": "text",
						"text": msg.Content,
					})
				}
				contentBlocks = append(contentBlocks, toolCallsForHistory...)
				messages = append(messages, map[string]interface{}{
					"role":    "assistant",
					"content": contentBlocks,
				})
			} else {
				// OpenAI-compatible.
				messages = append(messages, map[string]interface{}{
					"role":       "assistant",
					"content":    msg.Content,
					"tool_calls": toolCallsForHistory,
				})
			}

			// Execute each tool call.
			var results []tools.ToolResult
			for _, tc := range toolCalls {
				// Check if cancelled before each tool.
				if ctx.Err() != nil {
					h.a.Send(map[string]interface{}{
						"type":      "ai_response",
						"requestId": requestID,
						"content":   "Request cancelled.",
						"done":      true,
					})
					return nil
				}

				// Parse and validate the tool call before executing.
				valid, reason := validateToolCall(tc.Name, tc.Args, workDir)
				if !valid {
					results = append(results, tools.ToolResult{
						ToolCallID: tc.ID,
						Name:       tc.Name,
						Error:      fmt.Sprintf("Blocked: %s", reason),
					})
					continue
				}

				h.a.Send(map[string]interface{}{
					"type":      "ai_tool_call",
					"requestId": requestID,
					"toolName":  tc.Name,
					"toolId":    tc.ID,
				})

				result := executor.ExecuteToolCall(tc)
				results = append(results, result)

				h.a.Send(map[string]interface{}{
					"type":      "ai_tool_result",
					"requestId": requestID,
					"toolName":  tc.Name,
					"toolId":    tc.ID,
					"summary":   buildToolSummary(tc.Name, result),
					"error":     result.Error,
				})
			}

			// Add tool results to history — format depends on provider.
			isGemini = strings.Contains(baseURL, "generativelanguage.googleapis.com") || strings.HasPrefix(model, "gemini")
			isAnthropic = strings.Contains(baseURL, "anthropic.com") || strings.Contains(baseURL, "api.anthropic")

			if isGemini {
				// Gemini: functionResponse goes inside a "user" role message.
				var parts []map[string]interface{}
				for _, r := range results {
					content := r.Output
					if r.Error != "" {
						content = "Error: " + r.Error
					}
					parts = append(parts, map[string]interface{}{
						"functionResponse": map[string]interface{}{
							"name": r.Name,
							"response": map[string]interface{}{
								"result": truncate(content, 10000),
							},
						},
					})
				}
				messages = append(messages, map[string]interface{}{
					"role":  "user",
					"parts": parts,
				})
			} else if isAnthropic {
				// Anthropic: tool results go in user messages with content blocks.
				var contentBlocks []map[string]interface{}
				for _, r := range results {
					content := r.Output
					if r.Error != "" {
						content = "Error: " + r.Error
					}
					contentBlocks = append(contentBlocks, map[string]interface{}{
						"type":         "tool_result",
						"tool_use_id":  r.ToolCallID,
						"content":      truncate(content, 10000),
					})
				}
				messages = append(messages, map[string]interface{}{
					"role":    "user",
					"content": contentBlocks,
				})
			} else {
				// OpenAI-compatible: one "tool" message per tool call.
				for _, r := range results {
					content := r.Output
					if r.Error != "" {
						content = "Error: " + r.Error
					}
					messages = append(messages, map[string]interface{}{
						"role":         "tool",
						"tool_call_id": r.ToolCallID,
						"content":      truncate(content, 10000),
					})
				}
			}

			continue
		}

		// No tool calls — this is the final text response.
		if len(originalFiles) > 0 && workDir != "." {
			h.syncChangedFiles(requestID, workDir, originalFiles, projectId, backendURL)
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
				"content":   "Done.",
				"done":      true,
			})
		}
		return nil
	}

	return fmt.Errorf("exceeded maximum tool rounds (%d)", maxToolRounds)
}

// validateToolCall checks if a tool call is safe to execute.
func validateToolCall(name string, args json.RawMessage, workDir string) (bool, string) {
	if name != "bash" {
		return true, ""
	}

	parsed, err := tools.ParseToolArgs(args)
	if err != nil {
		return true, "" // Let executor handle parse errors
	}

	command := tools.GetStringArg(parsed, "command")
	if command == "" {
		return true, ""
	}

	// Check for blocked commands.
	if blockedCommands.MatchString(command) {
		return false, "command is not allowed in the sandbox"
	}

	// Check for path traversal.
	workdir := tools.GetStringArg(parsed, "workdir")
	if workdir != "" && pathTraversal.MatchString(workdir) {
		return false, "path traversal not allowed"
	}

	// Block attempts to access host system files.
	dangerousPaths := []string{
		"/etc/", "/var/", "/proc/", "/sys/", "/dev/", "/run/",
		"/boot/", "/sbin/", "/bin/", "/usr/bin/", "/usr/sbin/",
	}
	for _, p := range dangerousPaths {
		if strings.Contains(command, p) {
			return false, fmt.Sprintf("access to %s is not allowed", p)
		}
	}

	// Block network-facing commands that could be used for data exfiltration.
	netBlocked := []string{
		"curl ", "wget ", "nc ", "ncat ", "netcat ", "socat ",
		"ssh ", "scp ", "rsync ",
	}
	cmdLower := strings.ToLower(command)
	for _, nb := range netBlocked {
		if strings.Contains(cmdLower, nb) {
			return false, fmt.Sprintf("network command '%s' is not allowed", strings.TrimSpace(nb))
		}
	}

	return true, ""
}

// buildHardenedSystemPrompt creates a system prompt that resists prompt injection
// and ensures the agent completes tasks thoroughly.
func buildHardenedSystemPrompt(userPrompt string) string {
	base := `You are an expert mobile app code assistant running inside a secure sandbox. You have direct access to the user's project files and can read, write, edit, and execute commands.

CRITICAL: You MUST use your tools. Do NOT just describe what you would do — actually DO it using the tools. The user expects you to make real changes to their code.

# Agent Behavior
You are an agent — please keep going until the user's query is completely resolved, before ending your turn and yielding back to the user. Only terminate your turn when you are sure that the problem is solved. If you are not sure about file content or codebase structure pertaining to the user's request, use your tools to read files and gather the relevant information: do NOT guess or make up an answer.

Please resolve the user's task by editing and testing the code files in your current code execution session. You are a deployed coding agent. Your session allows for you to modify and run code. You must fully solve the problem for your answer to be considered correct.

You MUST adhere to the following criteria when executing the task:
- Working on the repo(s) in the current environment is allowed.
- Analyzing code for vulnerabilities is allowed.
- Showing user code and tool call details is allowed.
- If completing the user's task requires writing or modifying files:
    - Your code and final answer should follow these *CODING GUIDELINES*:
        - Fix the problem at the root cause rather than applying surface-level patches, when possible.
        - Avoid unneeded complexity in your solution.
        - Keep changes consistent with the style of the existing codebase. Changes should be minimal and focused on the task.
        - NEVER add copyright or license headers unless specifically requested.
        - Once you finish coding, you must sanity check your changes.
        - For smaller tasks, describe in brief bullet points.
        - For more complex tasks, include brief high-level description, use bullet points, and include details that would be relevant to a code reviewer.
- When doing things with paths, always use the full path from the project root.
- Remember the user does not see the full output of tools.

# Tool Usage — MANDATORY
You have access to the following tools and MUST use them:
- read: Read a file or directory. Use this FIRST to understand the codebase.
- write: Write content to a file. Creates the file if it doesn't exist, overwrites if it does.
- edit: Search and replace in a file. The oldString must match exactly.
- bash: Execute a sandboxed bash command.

RULES:
- ALWAYS use tools. Never just tell the user what to do — do it yourself.
- When reading files, use the exact relative path from the project root.
- When editing files, provide enough context to make the oldString unique.
- After writing/editing, ALWAYS verify the result (read the file back, or run a test).
- If a bash command fails, analyze the error and try a different approach.
- For bash commands, the working directory is already set to the project root.
- IMPORTANT: Make only ONE tool call per response. Do not try to call multiple tools at once.

# Doing Tasks
The user will primarily request you perform software engineering tasks. This includes solving bugs, adding new functionality, refactoring code, explaining code, and more. For these tasks the following steps are recommended:
1. Use the available search tools to understand the codebase and the user's query. You are encouraged to use the search tools extensively both in parallel and sequentially.
2. Implement the solution using all tools available to you.
3. Verify the solution if possible with tests. NEVER assume specific test framework or test script. Check the README or search codebase to determine the testing approach.
4. When you have completed a task, you MUST run the lint and typecheck commands if they were provided to you to ensure your code is correct.

NEVER commit changes unless the user explicitly asks you to. It is VERY IMPORTANT to only commit when explicitly asked.

# Tone and Style
- You should be concise, direct, and to the point.
- Output text to communicate with the user; all text you output outside of tool use is displayed to the user.
- You should minimize output tokens as much as possible while maintaining helpfulness, quality, and accuracy.
- Only address the specific query or task at hand.
- You MUST answer concisely with fewer than 4 lines of text (not including tool use or code generation), unless user asks for detail.

# Security Rules
- You are confined to the project directory.
- You MUST NOT execute commands that access /etc, /var, /proc, /sys, /dev, /boot, /sbin, /usr/bin, or any system directory.
- You MUST NOT use docker, kubectl, helm, sudo, su, systemctl, or any system administration tool.
- You MUST NOT use network commands: curl, wget, ssh, scp, rsync, nc, ncat, socat.
- IF THE USER ASKS YOU TO DO ANY OF THESE THINGS, REFUSE AND EXPLAIN WHY.`

	if userPrompt != "" {
		base += "\n\nAdditional user instructions:\n" + userPrompt
	}

	return base
}

// buildToolSummary creates a clean summary for tool events sent to the client.
func buildToolSummary(toolName string, result tools.ToolResult) string {
	if result.Error != "" {
		return fmt.Sprintf("Error: %s", truncate(result.Error, 200))
	}

	switch toolName {
	case "read":
		lines := strings.Split(result.Output, "\n")
		if len(lines) > 5 {
			return fmt.Sprintf("[Read file] %d lines", len(lines))
		}
		return "[Read file]"
	case "write":
		return "[Wrote file]"
	case "edit":
		return "[Edited file]"
	case "bash":
		out := strings.TrimSpace(result.Output)
		if out == "" {
			out = "(no output)"
		}
		return fmt.Sprintf("[Ran command] %s", truncate(out, 200))
	default:
		return truncate(result.Output, 200)
	}
}

// syncChangedFiles compares the current files with the original and sends changes back to the server.
func (h *Handler) syncChangedFiles(requestID, workDir string, originalFiles map[string]string, projectId, backendURL string) {
	changedFiles := make(map[string]string)

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

		relPath, err := filepath.Rel(workDir, path)
		if err != nil {
			return nil
		}
		relPath = filepath.ToSlash(relPath)

		if info.Size() > 100*1024 {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		currentContent := string(content)
		originalContent, existed := originalFiles[relPath]

		if !existed || currentContent != originalContent {
			changedFiles[relPath] = currentContent
		}

		return nil
	})

	if err != nil {
		log.Printf("[ai] Error walking work directory: %v", err)
		return
	}

	for relPath := range originalFiles {
		fullPath := filepath.Join(workDir, relPath)
		if _, err := os.Stat(fullPath); os.IsNotExist(err) {
			changedFiles[relPath] = ""
		}
	}

	if len(changedFiles) > 0 {
		log.Printf("[ai] Syncing %d changed files back to server", len(changedFiles))
		if projectId != "" && backendURL != "" {
			if err := h.uploadChanges(backendURL, projectId, requestID, changedFiles); err != nil {
				log.Printf("[ai] HTTP sync failed: %v, falling back to WebSocket", err)
				h.a.Send(map[string]interface{}{
					"type":      "source_sync",
					"requestId": requestID,
					"files":     changedFiles,
				})
			}
		} else {
			h.a.Send(map[string]interface{}{
				"type":      "source_sync",
				"requestId": requestID,
				"files":     changedFiles,
			})
		}
	}
}

func (h *Handler) sendError(requestID, msg string) {
	h.a.Send(map[string]interface{}{
		"type":      "ai_response",
		"requestId": requestID,
		"error":     msg,
		"done":      true,
	})
}

func (h *Handler) callAPIWithTools(baseURL, apiKey, model string, messages []map[string]interface{}, temperature float64, maxTokens int, toolDefs []tools.ToolDefinition) (*apiResponse, error) {
	// Gemini uses a completely different API format — dispatch to specialized handler.
	if strings.Contains(baseURL, "generativelanguage.googleapis.com") || (baseURL == "" && model != "" && strings.HasPrefix(model, "gemini")) {
		return h.callGeminiAPI(apiKey, model, messages, temperature, maxTokens, toolDefs)
	}

	// Anthropic uses a different API format.
	if strings.Contains(baseURL, "anthropic.com") || strings.Contains(baseURL, "api.anthropic") {
		return h.callAnthropicAPI(baseURL, apiKey, model, messages, temperature, maxTokens, toolDefs)
	}

	// All others use OpenAI-compatible format (OpenAI, Groq, OpenRouter, NVIDIA, Ollama, xAI, Copilot).
	return h.callOpenAICompatAPI(baseURL, apiKey, model, messages, temperature, maxTokens, toolDefs)
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
		return "https://api.anthropic.com"
	case "groq":
		return "https://api.groq.com/openai/v1"
	case "nvidia":
		return "https://integrate.api.nvidia.com/v1"
	case "openrouter":
		return "https://openrouter.ai/api/v1"
	case "gemini":
		return "" // Gemini uses a different API — handled separately
	case "ollama":
		return "http://localhost:11434/v1"
	case "xai":
		return "https://api.x.ai/v1"
	case "copilot":
		return "https://api.githubcopilot.com"
	default:
		return "https://api.openai.com/v1"
	}
}

// resolveDefaultModel returns a sensible default model for each provider.
func resolveDefaultModel(provider string) string {
	switch provider {
	case "nvidia":
		return "nvidia/nemotron-3-ultra-550b-a55b"
	case "groq":
		return "llama-3.3-70b-versatile"
	case "openrouter":
		return "deepseek/deepseek-r1-0528:free"
	case "gemini":
		return "gemini-2.0-flash"
	case "anthropic":
		return "claude-3-5-haiku-20241022"
	case "ollama":
		return "llama3.1"
	case "xai":
		return "grok-3-mini-beta"
	case "copilot":
		return "gpt-4o"
	default:
		return "gpt-4o-mini"
	}
}

// callOpenAICompatAPI handles providers that use the OpenAI chat/completions format:
// OpenAI, Groq, OpenRouter, NVIDIA, Ollama, xAI, Copilot.
func (h *Handler) callOpenAICompatAPI(baseURL, apiKey, model string, messages []map[string]interface{}, temperature float64, maxTokens int, toolDefs []tools.ToolDefinition) (*apiResponse, error) {
	wrappedTools := make([]map[string]interface{}, len(toolDefs))
	for i, td := range toolDefs {
		wrappedTools[i] = map[string]interface{}{
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
		"messages":    messages,
		"tools":       wrappedTools,
		"tool_choice": "auto",
		"temperature": temperature,
		"max_tokens":  maxTokens,
	}

	data, _ := json.Marshal(body)
	log.Printf("[ai] OpenAI-compat request to %s model=%s", baseURL, model)

	client := &http.Client{Timeout: 120 * time.Second}
	req, err := http.NewRequest("POST", baseURL+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	// OpenRouter requires HTTP-Referer header.
	if strings.Contains(baseURL, "openrouter.ai") {
		req.Header.Set("HTTP-Referer", "https://apkbuilder.app")
		req.Header.Set("X-Title", "ApkBuilder AI")
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("API request failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, truncate(string(respBody), 500))
	}

	var fullResp apiResponse
	if err := json.Unmarshal(respBody, &fullResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %v", err)
	}

	return &fullResp, nil
}

// callAnthropicAPI handles Anthropic's Messages API (different from OpenAI format).
func (h *Handler) callAnthropicAPI(baseURL, apiKey, model string, messages []map[string]interface{}, temperature float64, maxTokens int, toolDefs []tools.ToolDefinition) (*apiResponse, error) {
	// Convert OpenAI-style messages to Anthropic format.
	var systemMsg string
	var anthropicMessages []map[string]interface{}

	for _, m := range messages {
		role := m["role"].(string)
		content := m["content"].(string)
		if role == "system" {
			systemMsg = content
			continue
		}
		// Anthropic only accepts "user" and "assistant" roles.
		if role == "tool" {
			// Convert tool results to user messages with tool_result content blocks.
			toolCallID := m["tool_call_id"].(string)
			anthropicMessages = append(anthropicMessages, map[string]interface{}{
				"role": "user",
				"content": []map[string]interface{}{
					{
						"type":      "tool_result",
						"tool_use_id": toolCallID,
						"content":   content,
					},
				},
			})
		} else {
			anthropicMessages = append(anthropicMessages, map[string]interface{}{
				"role":    role,
				"content": content,
			})
		}
	}

	// Convert tool definitions to Anthropic format.
	var anthropicTools []map[string]interface{}
	for _, td := range toolDefs {
		anthropicTools = append(anthropicTools, map[string]interface{}{
			"name":        td.Name,
			"description": td.Description,
			"input_schema": td.Parameters,
		})
	}

	body := map[string]interface{}{
		"model":       model,
		"max_tokens":  maxTokens,
		"temperature": temperature,
		"messages":    anthropicMessages,
		"tools":       anthropicTools,
	}
	if systemMsg != "" {
		body["system"] = systemMsg
	}

	data, _ := json.Marshal(body)
	log.Printf("[ai] Anthropic request model=%s", model)

	client := &http.Client{Timeout: 120 * time.Second}
	req, err := http.NewRequest("POST", baseURL+"/v1/messages", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("API request failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, truncate(string(respBody), 500))
	}

	// Parse Anthropic response and convert to our apiResponse format.
	var anthropicResp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text,omitempty"`
			ID   string `json:"id,omitempty"`
			Name string `json:"name,omitempty"`
			Input json.RawMessage `json:"input,omitempty"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(respBody, &anthropicResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %v", err)
	}

	// Build apiResponse from Anthropic content blocks.
	resp2 := &apiResponse{}
	choice := struct {
		Message struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	}{}

	for _, block := range anthropicResp.Content {
		switch block.Type {
		case "text":
			choice.Message.Content += block.Text
		case "tool_use":
			choice.Message.ToolCalls = append(choice.Message.ToolCalls, struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			}{
				ID:   block.ID,
				Type: "function",
			})
			choice.Message.ToolCalls[len(choice.Message.ToolCalls)-1].Function.Name = block.Name
			choice.Message.ToolCalls[len(choice.Message.ToolCalls)-1].Function.Arguments = block.Input
		}
	}
	choice.FinishReason = anthropicResp.StopReason
	resp2.Choices = []struct {
		Message struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	}{choice}

	return resp2, nil
}

// callGeminiAPI handles Google Gemini's generateContent API.
func (h *Handler) callGeminiAPI(apiKey, model string, messages []map[string]interface{}, temperature float64, maxTokens int, toolDefs []tools.ToolDefinition) (*apiResponse, error) {
	// Build Gemini contents array.
	var contents []map[string]interface{}
	var systemInstruction map[string]interface{}

	for _, m := range messages {
		role := m["role"].(string)
		content := m["content"].(string)
		if role == "system" {
			systemInstruction = map[string]interface{}{
				"parts": []map[string]interface{}{{"text": content}},
			}
			continue
		}
		// Gemini uses "user"/"model" instead of "user"/"assistant".
		geminiRole := "user"
		if role == "assistant" {
			geminiRole = "model"
		}
		contents = append(contents, map[string]interface{}{
			"role": geminiRole,
			"parts": []map[string]interface{}{
				{"text": content},
			},
		})
	}

	// Build Gemini tools.
	var geminiTools []map[string]interface{}
	var functionDecls []map[string]interface{}
	for _, td := range toolDefs {
		params, _ := json.Marshal(td.Parameters)
		var paramSchema map[string]interface{}
		json.Unmarshal(params, &paramSchema)

		functionDecls = append(functionDecls, map[string]interface{}{
			"name":        td.Name,
			"description": td.Description,
			"parameters":  paramSchema,
		})
	}
	if len(functionDecls) > 0 {
		geminiTools = append(geminiTools, map[string]interface{}{
			"function_declarations": functionDecls,
		})
	}

	body := map[string]interface{}{
		"contents": contents,
		"generationConfig": map[string]interface{}{
			"temperature": temperature,
			"maxOutputTokens": maxTokens,
		},
	}
	if systemInstruction != nil {
		body["systemInstruction"] = systemInstruction
	}
	if len(geminiTools) > 0 {
		body["tools"] = geminiTools
	}

	data, _ := json.Marshal(body)
	log.Printf("[ai] Gemini request model=%s", model)

	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", model, apiKey)
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("Gemini API request failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Gemini API returned %d: %s", resp.StatusCode, truncate(string(respBody), 500))
	}

	// Parse Gemini response.
	var geminiResp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text         string `json:"text"`
					FunctionCall *struct {
						Name string                 `json:"name"`
						Args map[string]interface{} `json:"args"`
					} `json:"functionCall"`
				} `json:"parts"`
				Role string `json:"role"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(respBody, &geminiResp); err != nil {
		return nil, fmt.Errorf("failed to parse Gemini response: %v", err)
	}

	// Convert to apiResponse.
	resp2 := &apiResponse{}
	if len(geminiResp.Candidates) > 0 {
		cand := geminiResp.Candidates[0]
		choice := struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		}{}
		choice.FinishReason = cand.FinishReason

		for _, part := range cand.Content.Parts {
			if part.Text != "" {
				choice.Message.Content += part.Text
			}
			if part.FunctionCall != nil {
				argsJSON, _ := json.Marshal(part.FunctionCall.Args)
				tcID := fmt.Sprintf("call_%d", time.Now().UnixNano())
				choice.Message.ToolCalls = append(choice.Message.ToolCalls, struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				}{
					ID:   tcID,
					Type: "function",
				})
				choice.Message.ToolCalls[len(choice.Message.ToolCalls)-1].Function.Name = part.FunctionCall.Name
				choice.Message.ToolCalls[len(choice.Message.ToolCalls)-1].Function.Arguments = argsJSON
			}
		}
		resp2.Choices = []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		}{choice}
	}

	return resp2, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// fetchProject downloads project files from the server via HTTP.
func (h *Handler) fetchProject(backendURL, projectId string) (map[string]string, error) {
	url := strings.TrimRight(backendURL, "/") + "/api/ai/project/" + projectId
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("HTTP GET failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Files map[string]string `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode failed: %v", err)
	}
	return result.Files, nil
}

// uploadChanges sends changed files back to the server via HTTP.
func (h *Handler) uploadChanges(backendURL, projectId, requestId string, changes map[string]string) error {
	url := strings.TrimRight(backendURL, "/") + "/api/ai/sync-changes/" + projectId

	body := map[string]interface{}{
		"changes":   changes,
		"requestId": requestId,
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}

	resp, err := http.Post(url, "application/json", strings.NewReader(string(data)))
	if err != nil {
		return fmt.Errorf("HTTP POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
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
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}
