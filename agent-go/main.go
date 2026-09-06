package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// ── Config ────────────────────────────────────────────────────────────────────

type Config struct {
	BackendURL      string
	AccountID       string
	UserID          string
	SessionID       string
	DurationMinutes int
	ProjectsDir     string
}

func loadConfig() Config {
	return Config{
		BackendURL:      envOr("BACKEND_URL", "http://localhost:3000"),
		AccountID:       envOr("ACCOUNT_ID", ""),
		UserID:          envOr("USER_ID", ""),
		SessionID:       envOr("SESSION_ID", ""),
		DurationMinutes: envInt("DURATION_MINUTES", 60),
		ProjectsDir:     "/tmp/apkbuilder-projects",
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// ── Messages ──────────────────────────────────────────────────────────────────

type Message struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"-"`
	// Flat fields for convenience
	Data      string `json:"data,omitempty"`
	Line      string `json:"line,omitempty"`
	BuildID   string `json:"buildId,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	Code      int    `json:"code,omitempty"`
	Error     string `json:"error,omitempty"`
	Message   string `json:"message,omitempty"`
	AgentID   string `json:"agentId,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type BuildRequest struct {
	BuildID  string            `json:"buildId"`
	Files    map[string]string `json:"files"`
	Meta     BuildMeta         `json:"meta"`
	Variant  string            `json:"variant"`
	Format   string            `json:"format"`
	Platform string            `json:"platform"`
}

type BuildMeta struct {
	AppName     string `json:"appName"`
	Framework   string `json:"framework"`
	PackageName string `json:"packageName"`
	Version     string `json:"version"`
}

// ── Agent ─────────────────────────────────────────────────────────────────────

type Agent struct {
	cfg          Config
	conn         *websocket.Conn
	done         chan struct{}
	shells       map[string]*exec.Cmd
	stdinWriters map[string]io.WriteCloser
	vms          map[string]*vmShell
	pending      map[string][]byte
	mu           sync.Mutex
}

func NewAgent(cfg Config) *Agent {
	return &Agent{
		cfg:          cfg,
		done:         make(chan struct{}),
		shells:       make(map[string]*exec.Cmd),
		stdinWriters: make(map[string]io.WriteCloser),
		vms:          make(map[string]*vmShell),
		pending:      make(map[string][]byte),
	}
}

// vmShell is a single docker-backed terminal session scoped to one user session.
type vmShell struct {
	container string
	exec      *exec.Cmd
	stdin     io.WriteCloser
}

func (a *Agent) Run() {
	log.Printf("[agent] Starting... Backend=%s Account=%s User=%s Session=%s Duration=%dm",
		a.cfg.BackendURL, a.cfg.AccountID, a.cfg.UserID, a.cfg.SessionID, a.cfg.DurationMinutes)

	for {
		if err := a.connect(); err != nil {
			log.Printf("[agent] Connection failed: %v, retrying in 5s...", err)
			time.Sleep(5 * time.Second)
			continue
		}
		a.readLoop()
		log.Printf("[agent] Disconnected, reconnecting in 5s...")
		time.Sleep(5 * time.Second)
	}
}

func (a *Agent) connect() error {
	wsURL := strings.Replace(a.cfg.BackendURL, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
	wsURL += "/api/agent"

	log.Printf("[agent] Connecting to %s", wsURL)

	u, err := url.Parse(wsURL)
	if err != nil {
		return err
	}

	c, resp, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("bad handshake (HTTP %d): %w", resp.StatusCode, err)
		}
		return err
	}
	a.conn = c

	// Register
	reg := map[string]string{
		"type":      "register",
		"accountId": a.cfg.AccountID,
		"userId":    a.cfg.UserID,
		"sessionId": a.cfg.SessionID,
	}
	if err := a.send(reg); err != nil {
		c.Close()
		return err
	}

	log.Printf("[agent] Connected and registered")
	return nil
}

func (a *Agent) readLoop() {
	for {
		_, raw, err := a.conn.ReadMessage()
		if err != nil {
			log.Printf("[agent] Read error: %v", err)
			return
		}

		var msg Message
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "registered":
			log.Printf("[agent] Registered: agentId=%s", msg.AgentID)

		case "ping":
			a.send(map[string]string{"type": "pong"})

		case "build_request":
			var req BuildRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				log.Printf("[agent] Bad build_request: %v", err)
				continue
			}
			go a.handleBuild(req)

		case "terminal_data":
			var td struct {
				Type      string `json:"type"`
				SessionID string `json:"sessionId"`
				Data      string `json:"data"`
			}
			if err := json.Unmarshal(raw, &td); err != nil {
				continue
			}
			a.handleTerminal(td.SessionID, td.Data)

		case "terminal_start":
			var ts struct {
				Type       string `json:"type"`
				SessionID  string `json:"sessionId"`
				ProjectDir string `json:"projectDir"`
			}
			if err := json.Unmarshal(raw, &ts); err != nil {
				continue
			}
			// Provision in the background: pulling the base image can take ~1min,
			// which must not block the read loop (ping/pong) or the WS dies.
			go a.startTerminalVM(ts.SessionID, ts.ProjectDir)

		case "terminal_resize":
			// No-op: pipe-backed docker exec shell has a fixed geometry.

		case "shutdown":
			log.Printf("[agent] Shutdown: %s", msg.Reason)
			a.conn.Close()
			os.Exit(0)
		}
	}
}

func (a *Agent) send(v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return a.conn.WriteMessage(websocket.TextMessage, data)
}

// ── Build Handler ─────────────────────────────────────────────────────────────

func (a *Agent) handleBuild(req BuildRequest) {
	log.Printf("[agent] Build request: buildId=%s framework=%s platform=%s", req.BuildID, req.Meta.Framework, req.Platform)

	projectDir := filepath.Join(a.cfg.ProjectsDir, req.BuildID)
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		a.sendBuildFailed(req.BuildID, err.Error())
		return
	}

	// Write project files
	for path, content := range req.Files {
		fullPath := filepath.Join(projectDir, filepath.Clean(path))
		dir := filepath.Dir(fullPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			a.sendBuildFailed(req.BuildID, err.Error())
			return
		}
		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			a.sendBuildFailed(req.BuildID, err.Error())
			return
		}
	}

	a.sendProgress(req.BuildID, fmt.Sprintf("Building %s project for %s...", req.Meta.Framework, req.Platform))
	a.sendProgress(req.BuildID, fmt.Sprintf("Variant: %s, Format: %s", req.Variant, req.Format))

	// Install dependencies
	a.sendProgress(req.BuildID, "Installing dependencies...")
	if out, err := runCmd(projectDir, "npm", "install"); err != nil {
		a.sendProgress(req.BuildID, fmt.Sprintf("npm install failed: %s", out))
		a.sendBuildFailed(req.BuildID, err.Error())
		return
	}
	a.sendProgress(req.BuildID, "Dependencies installed")

	// Build based on framework
	switch req.Meta.Framework {
	case "react-native":
		a.buildReactNative(projectDir, req)
	case "hybrid":
		a.buildHybrid(projectDir, req)
	default:
		a.sendBuildFailed(req.BuildID, fmt.Sprintf("Unknown framework: %s", req.Meta.Framework))
		return
	}

	// Find artifact
	artifact := a.findArtifact(projectDir, req.Format)
	if artifact != "" {
		a.sendProgress(req.BuildID, fmt.Sprintf("Build succeeded: %s", artifact))

		// Upload the artifact to the backend so the user can download it.
		arrURL, err := a.uploadArtifact(req.BuildID, artifact)
		if err != nil {
			a.sendProgress(req.BuildID, fmt.Sprintf("Artifact upload failed: %v", err))
		}

		msg := map[string]interface{}{
			"type":    "build_complete",
			"buildId": req.BuildID,
			"status":  "succeeded",
		}
		if arrURL != "" {
			msg["artifactUrl"] = arrURL
		}
		a.send(msg)
	} else {
		a.sendProgress(req.BuildID, "Build completed but no artifact found")
		a.send(map[string]interface{}{
			"type":    "build_complete",
			"buildId": req.BuildID,
			"status":  "succeeded",
		})
	}
}

// uploadArtifact POSTs the built binary to the backend for storage + serving.
// Returns the public download URL on success.
func (a *Agent) uploadArtifact(buildID, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	uploadURL := strings.TrimSuffix(a.cfg.BackendURL, "/") + "/api/artifacts/" + buildID + "/" + filepath.Base(path)

	req, err := http.NewRequest(http.MethodPost, uploadURL, f)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	client := &http.Client{Timeout: 300 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("backend returned %d: %s", resp.StatusCode, string(body)[:200])
	}

	var out struct {
		ArtifactURL string `json:"artifactUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	log.Printf("[agent] Artifact uploaded for %s -> %s", buildID, out.ArtifactURL)
	return out.ArtifactURL, nil
}

func (a *Agent) buildReactNative(projectDir string, req BuildRequest) {
	gradlew := filepath.Join(projectDir, "android", "gradlew")

	if _, err := os.Stat(gradlew); err != nil {
		// The shipped android/ dir is authored source (no gradlew wrapper binary).
		// Regenerate a runnable native project from app.json.
		a.sendProgress(req.BuildID, "Running expo prebuild (clean)...")
		if out, err := runCmd(projectDir, "npx", "expo", "prebuild", "--platform", "android", "--no-install", "--clean"); err != nil {
			a.sendProgress(req.BuildID, "Prebuild warnings, continuing...")
			_ = out
		}
	} else {
		a.sendProgress(req.BuildID, "Running expo prebuild...")
		runCmd(projectDir, "npx", "expo", "prebuild", "--platform", "android", "--no-install")
	}

	if req.Platform == "web" {
		a.sendProgress(req.BuildID, "Building web bundle...")
		if out, err := runCmd(projectDir, "npx", "expo", "export", "--platform", "web"); err != nil {
			a.sendBuildFailed(req.BuildID, out)
			return
		}
		return
	}

	gradleTask := "assembleDebug"
	if req.Variant == "release" {
		gradleTask = "bundleRelease"
	}

	if _, err := os.Stat(gradlew); err != nil {
		a.sendBuildFailed(req.BuildID, "No gradlew found after prebuild; cannot run gradle build")
		return
	}

	a.sendProgress(req.BuildID, fmt.Sprintf("Running gradle %s...", gradleTask))
	runCmd(projectDir, "chmod", "+x", gradlew)
	if out, err := runCmd(filepath.Join(projectDir, "android"), gradlew, gradleTask, "--no-daemon"); err != nil {
		a.sendBuildFailed(req.BuildID, out)
		return
	}
}

func (a *Agent) buildHybrid(projectDir string, req BuildRequest) {
	a.sendProgress(req.BuildID, "Building Vite bundle...")
	if out, err := runCmd(projectDir, "npx", "vite", "build"); err != nil {
		a.sendBuildFailed(req.BuildID, out)
		return
	}

	if req.Platform == "android" || req.Platform == "ios" {
		a.sendProgress(req.BuildID, fmt.Sprintf("Adding %s platform...", req.Platform))
		runCmd(projectDir, "npx", "cap", "add", req.Platform)
		a.sendProgress(req.BuildID, "Syncing platform...")
		runCmd(projectDir, "npx", "cap", "sync", req.Platform)
	}
}

func (a *Agent) findArtifact(projectDir, format string) string {
	candidates := []string{}
	if format == "aab" {
		candidates = []string{
			"android/app/build/outputs/bundle/release/app-release.aab",
			"android/app/build/outputs/bundle/debug/app-debug.aab",
		}
	} else {
		candidates = []string{
			"android/app/build/outputs/apk/debug/app-debug.apk",
			"android/app/build/outputs/apk/release/app-release.apk",
		}
	}
	for _, c := range candidates {
		full := filepath.Join(projectDir, c)
		if _, err := os.Stat(full); err == nil {
			return full
		}
	}
	return ""
}

func (a *Agent) sendProgress(buildID, line string) {
	log.Printf("[build:%s] %s", buildID, line)
	a.send(map[string]interface{}{
		"type":    "build_progress",
		"buildId": buildID,
		"line":    line,
	})
}

func (a *Agent) sendBuildFailed(buildID, errMsg string) {
	log.Printf("[build:%s] FAILED: %s", buildID, errMsg)
	a.send(map[string]interface{}{
		"type":    "build_complete",
		"buildId": buildID,
		"status":  "failed",
		"error":   errMsg,
	})
}

// ── Terminal Handler ──────────────────────────────────────────────────────────

// startTerminalVM provisions (once) a docker-backed sandbox per session, copies
// the project source into /workspace, and starts an interactive shell attached
// to the session. Falls back to a host /bin/bash if docker is unavailable.
func (a *Agent) startTerminalVM(sessionID, projectDir string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.vms[sessionID]; ok {
		return
	}

	container := "apkbuilder-" + strings.ReplaceAll(sessionID, "-", "")
	if len(container) > 24 {
		container = container[:24]
	}
	log.Printf("[terminal] Provisioning VM for session %s (docker=%s)", sessionID, container)

	// Create the container (idempotent).
	out, err := runCmd("", "docker", "run", "-d", "-it", "--name", container, "node:20-alpine", "sleep", "infinity")
	if err != nil {
		log.Printf("[terminal] docker run failed (%v): %s", err, out)
		// Fallback: host bash shell
		a.startHostShell(sessionID)
		return
	}

	// Copy project source into the VM, if we have a project dir.
	var projLines string
	if projectDir != "" {
		if _, err := os.Stat(projectDir); err == nil {
			runCmd("", "docker", "cp", projectDir+"/.", container+":/workspace")
			projLines = "\r\n\x1b[1;32mProject source copied to /workspace\x1b[0m\r\n"
		}
	}
	runCmd("", "docker", "exec", container, "sh", "-c", "mkdir -p /workspace && cd /workspace && echo $(ls | wc -l) files")

	// Start a shell attached to the container (cwd = /workspace inside VM).
	cmd := exec.Command("docker", "exec", "-i", "-w", "/workspace", container, "/bin/sh")
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Printf("[terminal] stdin pipe: %v", err)
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("[terminal] stdout pipe: %v", err)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Printf("[terminal] stderr pipe: %v", err)
		return
	}

	if err := cmd.Start(); err != nil {
		log.Printf("[terminal] Failed to start container shell: %v", err)
		return
	}

	vm := &vmShell{container: container, exec: cmd, stdin: stdin}
	a.vms[sessionID] = vm

	// Flush any input that arrived while the VM was provisioning.
	if buf, ok := a.pending[sessionID]; ok {
		vm.stdin.Write(buf)
		delete(a.pending, sessionID)
	}

	pipeOutput := func(r io.Reader) {
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				a.sendTerminalOutput(sessionID, string(buf[:n]))
			}
			if err != nil {
				break
			}
		}
	}
	go pipeOutput(stdout)
	go pipeOutput(stderr)

	go func() {
		cmd.Wait()
		a.sendTerminalOutput(sessionID, "\r\n\x1b[33m[Shell exited]\x1b[0m\r\n")
		a.mu.Lock()
		delete(a.vms, sessionID)
		a.mu.Unlock()
	}()

	if projLines != "" {
		a.sendTerminalOutput(sessionID, projLines)
	}
}

// startHostShell is the non-docker fallback.
func (a *Agent) startHostShell(sessionID string) {
	cmd := exec.Command("/bin/bash")
	cmd.Dir = "/tmp"
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "HOME=/root")

	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		log.Printf("[terminal] Failed to start shell: %v", err)
		return
	}
	a.shells[sessionID] = cmd

	stdoutReader := func(r io.Reader) {
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				a.sendTerminalOutput(sessionID, string(buf[:n]))
			}
			if err != nil {
				break
			}
		}
	}
	go stdoutReader(stdout)
	go stdoutReader(stderr)

	go func() {
		cmd.Wait()
		a.sendTerminalOutput(sessionID, "\r\n\x1b[33m[Shell exited]\x1b[0m\r\n")
		delete(a.shells, sessionID)
	}()
	a.stdinWriters[sessionID] = stdin
}

func (a *Agent) handleTerminal(sessionID, data string) {
	a.mu.Lock()
	if vm, ok := a.vms[sessionID]; ok {
		vm.stdin.Write([]byte(data))
		a.mu.Unlock()
		return
	}
	if w, ok := a.stdinWriters[sessionID]; ok {
		w.Write([]byte(data))
		a.mu.Unlock()
		return
	}
	// VM still provisioning — queue the keystrokes.
	a.pending[sessionID] = append(a.pending[sessionID], []byte(data)...)
	a.mu.Unlock()
}

func (a *Agent) sendTerminalOutput(sessionID, data string) {
	a.send(map[string]interface{}{
		"type":      "terminal_output",
		"sessionId": sessionID,
		"data":      data,
	})
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func runCmd(dir string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	cfg := loadConfig()
	agent := NewAgent(cfg)

	// Handle signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		log.Println("[agent] Received signal, cleaning up...")
		for _, cmd := range agent.shells {
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
		}
		if agent.conn != nil {
			agent.conn.Close()
		}
		os.Exit(0)
	}()

	agent.Run()
}
