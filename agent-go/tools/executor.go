package tools

import (
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strings"
)

// Executor runs tool calls and returns results.
type Executor struct {
	WorkDir string
}

// NewExecutor creates a new tool executor.
func NewExecutor(workDir string) *Executor {
	return &Executor{WorkDir: workDir}
}

// resolvePath resolves a file path relative to the work directory.
// If the path is already absolute, it's returned as-is.
// If the path starts with "./" or doesn't start with "/", it's resolved relative to WorkDir.
func (e *Executor) resolvePath(filePath string) string {
	if filepath.IsAbs(filePath) {
		return filePath
	}
	return filepath.Join(e.WorkDir, filePath)
}

// ExecuteToolCall runs a single tool call and returns the result.
func (e *Executor) ExecuteToolCall(call ToolCall) ToolResult {
	log.Printf("[tools] Executing: %s", call.Name)

	var output string
	var err error

	args, parseErr := ParseToolArgs(call.Args)
	if parseErr != nil {
		return ToolResult{
			ToolCallID: call.ID,
			Name:       call.Name,
			Error:      fmt.Sprintf("invalid arguments: %v", parseErr),
		}
	}

	switch call.Name {
	case "read":
		filePath := GetStringArg(args, "filePath")
		offset := GetIntArg(args, "offset")
		limit := GetIntArg(args, "limit")
		if filePath == "" {
			output, err = "", fmt.Errorf("filePath is required")
		} else {
			output, err = ReadFile(e.resolvePath(filePath), offset, limit)
		}

	case "write":
		filePath := GetStringArg(args, "filePath")
		content := GetStringArg(args, "content")
		if filePath == "" || content == "" {
			output, err = "", fmt.Errorf("filePath and content are required")
		} else {
			output, err = WriteFile(e.resolvePath(filePath), content)
		}

	case "edit":
		filePath := GetStringArg(args, "filePath")
		oldString := GetStringArg(args, "oldString")
		newString := GetStringArg(args, "newString")
		replaceAll := GetBoolArg(args, "replaceAll")
		if filePath == "" || oldString == "" {
			output, err = "", fmt.Errorf("filePath and oldString are required")
		} else {
			output, err = EditFile(e.resolvePath(filePath), oldString, newString, replaceAll)
		}

	case "bash":
		command := GetStringArg(args, "command")
		workdir := GetStringArg(args, "workdir")
		timeout := GetIntArg(args, "timeout")
		if command == "" {
			output, err = "", fmt.Errorf("command is required")
		} else {
			if workdir == "" || workdir == "/path/to/project/root" || workdir == "." {
				workdir = e.WorkDir
			} else if !filepath.IsAbs(workdir) {
				workdir = filepath.Join(e.WorkDir, workdir)
			}
			output, err = BashExec(command, workdir, timeout)
		}

	default:
		output, err = "", fmt.Errorf("unknown tool: %s", call.Name)
	}

	if err != nil {
		return ToolResult{
			ToolCallID: call.ID,
			Name:       call.Name,
			Error:      err.Error(),
		}
	}

	output, _ = TruncateOutput(output, 2000, 100*1024)
	return ToolResult{
		ToolCallID: call.ID,
		Name:       call.Name,
		Output:     output,
	}
}

// FormatToolResults formats tool results for the AI conversation.
func FormatToolResults(results []ToolResult) string {
	var parts []string
	for _, r := range results {
		if r.Error != "" {
			parts = append(parts, fmt.Sprintf("<tool_result tool=\"%s\" id=\"%s\">\nError: %s\n</tool_result>", r.Name, r.ToolCallID, r.Error))
		} else {
			parts = append(parts, fmt.Sprintf("<tool_result tool=\"%s\" id=\"%s\">\n%s\n</tool_result>", r.Name, r.ToolCallID, r.Output))
		}
	}
	return strings.Join(parts, "\n\n")
}

// ParseToolCalls extracts tool calls from an AI response.
// This handles both OpenAI-style and raw JSON tool calls.
func ParseToolCalls(content string) []ToolCall {
	var toolCalls []ToolCall

	// Try to parse as OpenAI-style response with tool_calls
	var response struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID   string `json:"id"`
					Type string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := json.Unmarshal([]byte(content), &response); err == nil {
		if len(response.Choices) > 0 {
			for _, tc := range response.Choices[0].Message.ToolCalls {
				toolCalls = append(toolCalls, ToolCall{
					ID:   tc.ID,
					Name: tc.Function.Name,
					Args: json.RawMessage(tc.Function.Arguments),
				})
			}
			return toolCalls
		}
	}

	// Try to parse as a single tool call JSON object: {"name": "write", "content": "...", ...}
	var singleTool struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(content), &singleTool); err == nil && singleTool.Name != "" {
		// Re-marshal the whole object as the arguments
		toolCalls = append(toolCalls, ToolCall{
			ID:   fmt.Sprintf("tool-%d", len(toolCalls)+1),
			Name: singleTool.Name,
			Args: json.RawMessage(content),
		})
		return toolCalls
	}

	return toolCalls
}
