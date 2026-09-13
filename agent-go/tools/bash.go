package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// BashExec executes a bash command and returns the output.
func BashExec(command, workdir string, timeout int) (string, error) {
	if timeout <= 0 {
		timeout = 120000
	}
	if workdir == "" {
		workdir = "."
	}

	cmd := exec.Command("bash", "-c", command)
	cmd.Dir = workdir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Start()
	if err != nil {
		return "", fmt.Errorf("failed to start command: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		output := stdout.String()
		if stderr.Len() > 0 {
			if output != "" {
				output += "\n"
			}
			output += stderr.String()
		}
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				return output, fmt.Errorf("command exited with code %d", exitErr.ExitCode())
			}
			return output, fmt.Errorf("command failed: %v", err)
		}
		if output == "" {
			output = "(no output)"
		}
		return output, nil

	case <-time.After(time.Duration(timeout) * time.Millisecond):
		_ = cmd.Process.Kill()
		return "", fmt.Errorf("command timed out after %dms", timeout)
	}
}

// ParseToolArgs parses JSON tool call arguments into a map.
func ParseToolArgs(args []byte) (map[string]interface{}, error) {
	var result map[string]interface{}
	if err := json.Unmarshal(args, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetStringArg extracts a string argument from parsed args.
func GetStringArg(args map[string]interface{}, key string) string {
	if val, ok := args[key]; ok {
		if s, ok := val.(string); ok {
			return s
		}
	}
	return ""
}

// GetIntArg extracts an integer argument from parsed args.
func GetIntArg(args map[string]interface{}, key string) int {
	if val, ok := args[key]; ok {
		switch v := val.(type) {
		case float64:
			return int(v)
		case int:
			return v
		}
	}
	return 0
}

// GetBoolArg extracts a boolean argument from parsed args.
func GetBoolArg(args map[string]interface{}, key string) bool {
	if val, ok := args[key]; ok {
		if b, ok := val.(bool); ok {
			return b
		}
	}
	return false
}

// TruncateOutput truncates output to a maximum number of lines and bytes.
func TruncateOutput(output string, maxLines, maxBytes int) (string, bool) {
	if maxLines <= 0 {
		maxLines = 2000
	}
	if maxBytes <= 0 {
		maxBytes = 100 * 1024
	}

	lines := strings.Split(output, "\n")
	truncated := false

	if len(lines) > maxLines {
		lines = lines[:maxLines]
		truncated = true
	}

	result := strings.Join(lines, "\n")
	if len(result) > maxBytes {
		result = result[:maxBytes]
		truncated = true
	}

	if truncated {
		result += "\n\n... (output truncated)"
	}

	return result, truncated
}
