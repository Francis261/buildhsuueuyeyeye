package tools

import (
	"os"
	"testing"
)

func TestReadFile(t *testing.T) {
	// Create a test file
	content := "Hello, World!\nLine 2\nLine 3"
	tmpFile := t.TempDir() + "/test.txt"
	os.WriteFile(tmpFile, []byte(content), 0644)

	// Test reading the file
	result, err := ReadFile(tmpFile, 0, 0)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if result == "" {
		t.Fatal("ReadFile returned empty result")
	}
	t.Logf("ReadFile result:\n%s", result)
}

func TestWriteFile(t *testing.T) {
	tmpFile := t.TempDir() + "/test.txt"
	
	result, err := WriteFile(tmpFile, "Hello, World!")
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	t.Log("WriteFile result:", result)
	
	// Verify the file was written
	data, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatalf("Failed to read written file: %v", err)
	}
	if string(data) != "Hello, World!" {
		t.Fatalf("File content mismatch: got %q", string(data))
	}
}

func TestEditFile(t *testing.T) {
	tmpFile := t.TempDir() + "/test.txt"
	os.WriteFile(tmpFile, []byte("Hello, World!"), 0644)

	result, err := EditFile(tmpFile, "World", "Go", false)
	if err != nil {
		t.Fatalf("EditFile failed: %v", err)
	}
	t.Log("EditFile result:", result)
	
	// Verify the edit
	data, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatalf("Failed to read edited file: %v", err)
	}
	if string(data) != "Hello, Go!" {
		t.Fatalf("File content mismatch: got %q", string(data))
	}
}

func TestBashExec(t *testing.T) {
	result, err := BashExec("echo hello", ".", 5000)
	if err != nil {
		t.Fatalf("BashExec failed: %v", err)
	}
	if result != "hello\n" {
		t.Fatalf("BashExec result mismatch: got %q", result)
	}
}

func TestGetToolDefinitions(t *testing.T) {
	defs := GetToolDefinitions()
	if len(defs) != 4 {
		t.Fatalf("Expected 4 tools, got %d", len(defs))
	}
	for _, d := range defs {
		t.Logf("Tool: %s - %s", d.Name, d.Description)
	}
}
