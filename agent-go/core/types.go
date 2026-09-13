package core

import "sync"

// AgentInterface is what sub-packages (builder, terminal, ai) use to
// communicate back to the main Agent without importing the main package.
type AgentInterface interface {
	Send(v interface{}) error
	SendProgress(buildID, line string)
	SendBuildFailed(buildID, errMsg string)
	SendBuildCancelled(buildID string)
	SendTerminalOutput(sessionID, data string)
	Cancelled() bool

	// Build control
	KillBuild()
	RunBuildCmd(dir string, name string, args ...string) (string, error)

	// Artifact
	ArtifactAcks() *sync.Map
	SendArtifactOverWS(buildID, path string) (string, error)
}

// BuildConfig holds configuration needed by the builder.
type BuildConfig struct {
	ProjectsDir string
}

// Message types shared between packages.
type Message struct {
	Type    string `json:"type"`
	AgentID string `json:"agentId,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

type BuildRequest struct {
	Type       string            `json:"type"`
	BuildID    string            `json:"buildId"`
	Platform   string            `json:"platform"`
	Variant    string            `json:"variant"`
	Format     string            `json:"format"`
	Files      map[string]string `json:"files"`
	ProjectsDir string           `json:"-"`
	Meta       BuildMeta         `json:"meta"`
}

type BuildMeta struct {
	Framework string `json:"framework"`
	Version   string `json:"version,omitempty"`
}

type AIRequest struct {
	Type        string            `json:"type"`
	RequestID   string            `json:"requestId"`
	Model       string            `json:"model"`
	Messages    []AIMessage       `json:"messages"`
	File        *AIFileContext     `json:"file,omitempty"`
	AgentConfig *AIAgentConfig    `json:"agentConfig,omitempty"`
}

type AIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type AIFileContext struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Language string `json:"language"`
}

type AIAgentConfig struct {
	Provider    string   `json:"provider"`
	APIKey      string   `json:"apiKey"`
	BaseURL     string   `json:"baseUrl,omitempty"`
	Models      []string `json:"models,omitempty"`
	CustomModel string   `json:"customModel,omitempty"`
	Temperature float64  `json:"temperature,omitempty"`
	MaxTokens   int      `json:"maxTokens,omitempty"`
	SystemPrompt string `json:"systemPrompt,omitempty"`
}
