package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ReadFile reads a file or lists a directory.
func ReadFile(filePath string, offset, limit int) (string, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return "", fmt.Errorf("file not found: %s", filePath)
	}

	if info.IsDir() {
		return readDirectory(filePath, offset, limit)
	}
	return readFileContent(filePath, offset, limit)
}

func readDirectory(dirPath string, offset, limit int) (string, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return "", fmt.Errorf("cannot read directory: %v", err)
	}

	if limit <= 0 {
		limit = 2000
	}
	if offset <= 0 {
		offset = 1
	}

	start := offset - 1
	var items []string
	for i := start; i < len(entries) && i < start+limit; i++ {
		name := entries[i].Name()
		if entries[i].IsDir() {
			name += "/"
		}
		items = append(items, name)
	}

	output := fmt.Sprintf("<path>%s</path>\n<type>directory</type>\n<entries>\n%s\n", dirPath, strings.Join(items, "\n"))
	if start+len(entries) > start+limit {
		output += fmt.Sprintf("\n(Showing %d of %d entries. Use offset=%d to continue.)", len(items), len(entries), start+len(items)+1)
	} else {
		output += fmt.Sprintf("\n(%d entries)", len(entries))
	}
	output += "\n</entries>"
	return output, nil
}

func readFileContent(filePath string, offset, limit int) (string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("cannot read file: %v", err)
	}

	lines := strings.Split(string(data), "\n")
	if limit <= 0 {
		limit = 2000
	}
	if offset <= 0 {
		offset = 1
	}

	start := offset - 1
	if start >= len(lines) {
		return "", fmt.Errorf("offset %d is out of range for this file (%d lines)", offset, len(lines))
	}

	end := start + limit
	if end > len(lines) {
		end = len(lines)
	}

	output := fmt.Sprintf("<path>%s</path>\n<type>file</type>\n<content>\n", filePath)
	for i := start; i < end; i++ {
		output += fmt.Sprintf("%d: %s\n", i+1, lines[i])
	}

	totalLines := len(lines)
	if end < totalLines {
		output += fmt.Sprintf("\n(Showing lines %d-%d of %d. Use offset=%d to continue.)", start+1, end, totalLines, end+1)
	} else {
		output += fmt.Sprintf("\n(End of file - total %d lines)", totalLines)
	}
	output += "\n</content>"
	return output, nil
}

// WriteFile writes content to a file, creating directories as needed.
func WriteFile(filePath, content string) (string, error) {
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("cannot create directory: %v", err)
	}

	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		return "", fmt.Errorf("cannot write file: %v", err)
	}

	return fmt.Sprintf("Wrote file successfully: %s", filePath), nil
}

// EditFile performs a search-and-replace edit.
func EditFile(filePath, oldString, newString string, replaceAll bool) (string, error) {
	if oldString == newString {
		return "", fmt.Errorf("oldString and newString are identical")
	}
	if oldString == "" {
		return "", fmt.Errorf("oldString cannot be empty")
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("cannot read file: %v", err)
	}

	content := string(data)

	if replaceAll {
		if !strings.Contains(content, oldString) {
			return "", fmt.Errorf("oldString not found in file")
		}
		content = strings.ReplaceAll(content, oldString, newString)
	} else {
		count := strings.Count(content, oldString)
		if count == 0 {
			return "", fmt.Errorf("oldString not found in file")
		}
		if count > 1 {
			return "", fmt.Errorf("found %d matches for oldString. Provide more context to make it unique", count)
		}
		content = strings.Replace(content, oldString, newString, 1)
	}

	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		return "", fmt.Errorf("cannot write file: %v", err)
	}

	return "Edit applied successfully.", nil
}
