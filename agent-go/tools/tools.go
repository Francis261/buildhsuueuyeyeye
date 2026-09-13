package tools

import (
	"encoding/json"
)

// ToolDefinition defines a tool that the AI can call.
type ToolDefinition struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

// ToolCall represents a tool call from the AI.
type ToolCall struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Args     json.RawMessage `json:"args"`
}

// ToolResult represents the result of executing a tool.
type ToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	Name       string `json:"name"`
	Output     string `json:"output"`
	Error      string `json:"error,omitempty"`
}

// GetToolDefinitions returns the definitions of all available tools.
func GetToolDefinitions() []ToolDefinition {
	return []ToolDefinition{
		{
			Name:        "read",
			Description: "Read a file or directory. For files, returns content with line numbers. For directories, lists entries.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"filePath": map[string]interface{}{
						"type":        "string",
						"description": "The absolute path to the file or directory to read",
					},
					"offset": map[string]interface{}{
						"type":        "integer",
						"description": "The line number to start reading from (1-indexed)",
					},
					"limit": map[string]interface{}{
						"type":        "integer",
						"description": "The maximum number of lines to read (defaults to 2000)",
					},
				},
				"required": []string{"filePath"},
			},
		},
		{
			Name:        "write",
			Description: "Write content to a file. Creates the file if it doesn't exist, overwrites if it does.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"filePath": map[string]interface{}{
						"type":        "string",
						"description": "The absolute path to the file to write",
					},
					"content": map[string]interface{}{
						"type":        "string",
						"description": "The content to write to the file",
					},
				},
				"required": []string{"filePath", "content"},
			},
		},
		{
			Name:        "edit",
			Description: "Apply a search-and-replace edit to a file. The oldString must match exactly.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"filePath": map[string]interface{}{
						"type":        "string",
						"description": "The absolute path to the file to modify",
					},
					"oldString": map[string]interface{}{
						"type":        "string",
						"description": "The text to replace",
					},
					"newString": map[string]interface{}{
						"type":        "string",
						"description": "The text to replace it with (must be different from oldString)",
					},
					"replaceAll": map[string]interface{}{
						"type":        "boolean",
						"description": "Replace all occurrences of oldString (default false)",
					},
				},
				"required": []string{"filePath", "oldString", "newString"},
			},
		},
		{
			Name:        "bash",
			Description: "Execute a bash command with optional timeout. Returns stdout and stderr.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"command": map[string]interface{}{
						"type":        "string",
						"description": "The command to execute",
					},
					"workdir": map[string]interface{}{
						"type":        "string",
						"description": "Working directory (defaults to project root)",
					},
					"timeout": map[string]interface{}{
						"type":        "integer",
						"description": "Timeout in milliseconds (default 120000)",
					},
				},
				"required": []string{"command"},
			},
		},
	}
}
