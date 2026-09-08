package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	artifactAcks sync.Map // buildId -> chan string (artifact_ack url)
	writeMu      sync.Mutex
	mu           sync.Mutex

	buildPgids map[int]bool // process groups of running build commands
	buildOpsMu sync.Mutex
	cancelling int32 // atomic: 1 when a user-cancel is in progress (vs budget kill)
}

func NewAgent(cfg Config) *Agent {
	return &Agent{
		cfg:          cfg,
		done:         make(chan struct{}),
		shells:       make(map[string]*exec.Cmd),
		stdinWriters: make(map[string]io.WriteCloser),
		vms:          make(map[string]*vmShell),
		pending:      make(map[string][]byte),
		buildPgids:   make(map[int]bool),
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

	// Enforce the paid time budget locally: when it elapses, kill any running
	// build/terminal and exit. This guarantees we never build past what the
	// user paid for, even if the backend dies before sending shutdown.
	go func() {
		time.Sleep(time.Duration(a.cfg.DurationMinutes) * time.Minute)
		log.Printf("[agent] Duration budget (%dm) elapsed — terminating", a.cfg.DurationMinutes)
		a.killBuild()
		os.Exit(0)
	}()

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

		case "build_cancel":
			var bc struct {
				Type    string `json:"type"`
				BuildID string `json:"buildId"`
			}
			if err := json.Unmarshal(raw, &bc); err != nil {
				continue
			}
			log.Printf("[agent] Cancel requested for build %s (agent keeps running)", bc.BuildID)
			atomic.StoreInt32(&a.cancelling, 1)
			a.killBuild()

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

		case "artifact_ack":
			var ack struct {
				Type        string `json:"type"`
				BuildID     string `json:"buildId"`
				ArtifactURL string `json:"artifactUrl"`
			}
			if err := json.Unmarshal(raw, &ack); err != nil {
				continue
			}
			if ch, ok := a.artifactAcks.LoadAndDelete(ack.BuildID); ok {
				ch.(chan string) <- ack.ArtifactURL
			}

		case "shutdown":
			log.Printf("[agent] Shutdown: %s", msg.Reason)
			a.killBuild()
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
	// gorilla/websocket is not safe for concurrent writers: the artifact
	// stream (build goroutine) and the ping/pong loop can both call send().
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
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
	atomic.StoreInt32(&a.cancelling, 0)
	if a.cancelled() {
		a.sendBuildCancelled(req.BuildID)
		return
	}
	if out, err := a.runBuildCmd(projectDir, "npm", "install"); err != nil {
		if a.cancelled() {
			a.sendBuildCancelled(req.BuildID)
			return
		}
		a.sendProgress(req.BuildID, fmt.Sprintf("npm install failed: %s", out))
		a.sendBuildFailed(req.BuildID, err.Error())
		return
	}
	if a.cancelled() {
		a.sendBuildCancelled(req.BuildID)
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

	if a.cancelled() {
		a.sendBuildCancelled(req.BuildID)
		return
	}

	// Find artifact
	artifact := a.findArtifact(projectDir, req.Format)
	if artifact != "" {
		a.sendProgress(req.BuildID, fmt.Sprintf("Build succeeded: %s", artifact))

		// Upload the artifact to the backend over the existing WebSocket
		// (the Cloudflare tunnel throttles large HTTP uploads).
		arrURL, err := a.sendArtifactOverWS(req.BuildID, artifact)
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

// sendArtifactOverWS streams a file to the backend in base64 chunks over the
// agent's WebSocket connection and waits for the backend's artifact_ack.
func (a *Agent) sendArtifactOverWS(buildID, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", err
	}

	filename := filepath.Base(path)
	if err := a.send(map[string]interface{}{
		"type":     "artifact_start",
		"buildId":  buildID,
		"filename": filename,
		"size":     info.Size(),
	}); err != nil {
		return "", err
	}

	const chunkSize = 256 * 1024
	buf := make([]byte, chunkSize)
	index := 0
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if err := a.send(map[string]interface{}{
				"type":    "artifact_chunk",
				"buildId": buildID,
				"index":   index,
				"data":    base64.StdEncoding.EncodeToString(buf[:n]),
			}); err != nil {
				return "", err
			}
			index++
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}

	respCh := make(chan string, 1)
	a.artifactAcks.Store(buildID, respCh)
	defer a.artifactAcks.Delete(buildID)

	if err := a.send(map[string]interface{}{"type": "artifact_finish", "buildId": buildID}); err != nil {
		return "", err
	}

	select {
	case url := <-respCh:
		return url, nil
	case <-time.After(120 * time.Second):
		return "", fmt.Errorf("timed out waiting for artifact_ack")
	}
}

func (a *Agent) buildReactNative(projectDir string, req BuildRequest) {
	gradlew := filepath.Join(projectDir, "android", "gradlew")

	if _, err := os.Stat(gradlew); err != nil {
		// The shipped android/ dir is authored source (no gradlew wrapper binary).
		// Regenerate a runnable native project from app.json.
		a.sendProgress(req.BuildID, "Running expo prebuild (clean)...")
		if out, err := a.runBuildCmd(projectDir, "npx", "expo", "prebuild", "--platform", "android", "--no-install", "--clean"); err != nil {
			if a.cancelled() {
				a.sendBuildCancelled(req.BuildID)
				return
			}
			a.sendProgress(req.BuildID, "Prebuild warnings, continuing...")
			_ = out
		}
	} else {
		a.sendProgress(req.BuildID, "Running expo prebuild...")
		a.runBuildCmd(projectDir, "npx", "expo", "prebuild", "--platform", "android", "--no-install")
	}

	if a.cancelled() {
		a.sendBuildCancelled(req.BuildID)
		return
	}

	if req.Platform == "web" {
		a.sendProgress(req.BuildID, "Building web bundle...")
		if out, err := a.runBuildCmd(projectDir, "npx", "expo", "export", "--platform", "web"); err != nil {
			if a.cancelled() {
				a.sendBuildCancelled(req.BuildID)
				return
			}
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
	a.runBuildCmd(projectDir, "chmod", "+x", gradlew)
	if out, err := a.runBuildCmd(filepath.Join(projectDir, "android"), gradlew, gradleTask, "--no-daemon"); err != nil {
		if a.cancelled() {
			a.sendBuildCancelled(req.BuildID)
			return
		}
		a.sendBuildFailed(req.BuildID, out)
		return
	}
}

func (a *Agent) buildHybrid(projectDir string, req BuildRequest) {
	a.sendProgress(req.BuildID, "Building Vite bundle...")
	if out, err := a.runBuildCmd(projectDir, "npx", "vite", "build"); err != nil {
		if a.cancelled() {
			a.sendBuildCancelled(req.BuildID)
			return
		}
		a.sendBuildFailed(req.BuildID, out)
		return
	}

	if req.Platform == "android" || req.Platform == "ios" {
		a.sendProgress(req.BuildID, fmt.Sprintf("Adding %s platform...", req.Platform))
		a.runBuildCmd(projectDir, "npx", "cap", "add", req.Platform)
		a.sendProgress(req.BuildID, "Syncing platform...")
		a.runBuildCmd(projectDir, "npx", "cap", "sync", req.Platform)
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

// cancelled reports whether a user requested cancellation of the current build.
func (a *Agent) cancelled() bool {
	return atomic.LoadInt32(&a.cancelling) == 1
}

func (a *Agent) sendBuildCancelled(buildID string) {
	log.Printf("[build:%s] CANCELLED", buildID)
	a.send(map[string]interface{}{
		"type":    "build_complete",
		"buildId": buildID,
		"status":  "cancelled",
	})
}

// ── Terminal Handler ──────────────────────────────────────────────────────────

// startTerminalVM provisions (once) a docker-backed sandbox per session, copies
// the project source into /workspace, and starts an interactive shell attached
// to the session. Falls back to a host /bin/bash if docker is unavailable.
func (a *Agent) startTerminalVM(sessionID, projectDir string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Reattach: the shell from a previous connection is still running for this
	// session. Emit a short banner and tap Enter so the shell redraws its
	// prompt on the fresh client buffer.
	if vm, ok := a.vms[sessionID]; ok {
		log.Printf("[terminal] Reattaching existing shell for session %s", sessionID)
		a.sendTerminalOutput(sessionID, "\r\n\x1b[1;36m[Reconnected]\x1b[0m\r\n")
		vm.stdin.Write([]byte("\r"))
		return
	}
	if _, ok := a.shells[sessionID]; ok {
		log.Printf("[terminal] Reattaching existing host shell for session %s", sessionID)
		a.sendTerminalOutput(sessionID, "\r\n\x1b[1;36m[Reconnected]\x1b[0m\r\n")
		if w, ok := a.stdinWriters[sessionID]; ok {
			w.Write([]byte("\r"))
		}
		return
	}

	container := "apkbuilder-" + strings.ReplaceAll(sessionID, "-", "")
	if len(container) > 24 {
		container = container[:24]
	}
	log.Printf("[terminal] Provisioning VM for session %s (docker=%s)", sessionID, container)

	// Create (or reuse) the container idempotently: a previous shell may have
	// exited and left the container running, and a later `docker run` with the
	// same --name would otherwise fail with a name-conflict (exit 125).
	if _, err := ensureContainer(container, "node:20-alpine"); err != nil {
		log.Printf("[terminal] VM provision failed (%v): %v", container, err)
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

	// Start an interactive shell attached to the container (cwd = /workspace
	// inside the VM). docker exec -it requires a client-side TTY or the shell
	// runs non-interactively (no prompt, no input echo); `script` allocates
	// that PTY and forwards stdin/stdout across the pipe.
	cmd := exec.Command("script", "-qefc",
		fmt.Sprintf("docker exec -it -w /workspace %s /bin/sh", container), "/dev/null")
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
		// Best-effort: free the container so a future provision can recreate it.
		runCmd("", "docker", "rm", "-f", container)
	}()

	if projLines != "" {
		a.sendTerminalOutput(sessionID, projLines)
	}
}

// startHostShell is the non-docker fallback.
func (a *Agent) startHostShell(sessionID string) {
	// `script` allocates a PTY so the shell is interactive (prompt + echo),
	// otherwise a pipe-backed bash runs non-interactively.
	cmd := exec.Command("script", "-qefc", "/bin/bash", "/dev/null")
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

// runBuildCmd runs a build step in its own process group and records the group
// so killBuild can terminate it when the paid agent budget elapses.
func (a *Agent) runBuildCmd(dir string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb

	a.buildOpsMu.Lock()
	if err := cmd.Start(); err != nil {
		a.buildOpsMu.Unlock()
		return "", err
	}
	a.buildPgids[cmd.Process.Pid] = true
	a.buildOpsMu.Unlock()

	err := cmd.Wait()

	a.buildOpsMu.Lock()
	delete(a.buildPgids, cmd.Process.Pid)
	a.buildOpsMu.Unlock()

	if errb.Len() > 0 {
		out.Write(errb.Bytes())
	}
	return out.String(), err
}

// killBuild hard-stops any in-flight build command (and terminal VMs) — used
// when the time budget elapses or the backend orders a shutdown.
func (a *Agent) killBuild() {
	a.buildOpsMu.Lock()
	pgids := make([]int, 0, len(a.buildPgids))
	for pgid := range a.buildPgids {
		pgids = append(pgids, pgid)
	}
	a.buildOpsMu.Unlock()

	for _, pgid := range pgids {
		// Negative pid targets the whole process group (Setpgid above).
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}

	a.mu.Lock()
	containers := make([]string, 0, len(a.vms))
	for _, vm := range a.vms {
		containers = append(containers, vm.container)
	}
	a.mu.Unlock()
	for _, c := range containers {
		if _, err := runCmd("", "docker", "rm", "-f", c); err != nil {
			log.Printf("[agent] cleanup container %s: %v", c, err)
		}
	}
}

// ensureContainer makes sure a docker container with the given name exists and
// is running, creating it when absent or reusing/restarting it otherwise. This
// keeps repeated terminal sessions idempotent (consistently shared agents reconnect
// to the same VM instead of tripping over the container-name conflict).
func ensureContainer(name, image string) (string, error) {
	// Already running?
	out, err := runCmd("", "docker", "inspect", "-f", "{{.State.Running}}", name)
	if err == nil && strings.TrimSpace(out) == "true" {
		return "reused-running", nil
	}
	if err == nil && strings.TrimSpace(out) == "false" {
		if o, e := runCmd("", "docker", "start", name); e != nil {
			return o, e
		}
		return "reused-stopped", nil
	}
	// Not present (or a generic error) -> recreate it.
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "no such") {
		runCmd("", "docker", "rm", "-f", name) // best-effort cleanup of a broken state
	}
	out, err = runCmd("", "docker", "run", "-d", "-it", "--name", name, image, "sleep", "infinity")
	if err != nil {
		// Race with another provisioner: if it just appeared, start it.
		if _, e := runCmd("", "docker", "start", name); e == nil {
			return "reused-race", nil
		}
		return out, err
	}
	return "created", nil
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
