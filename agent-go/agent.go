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
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"apkbuilder-agent/ai"
	"apkbuilder-agent/builder"
	"apkbuilder-agent/core"
	"apkbuilder-agent/terminal"

	"github.com/gorilla/websocket"
)

type Agent struct {
	cfg      Config
	conn     *websocket.Conn
	done     chan struct{}
	writeMu  sync.Mutex
	artifactAcks sync.Map

	buildPgids map[int]bool
	buildOpsMu sync.Mutex
	cancelling int32

	builder  *builder.Handler
	terminal *terminal.Handler
	ai       *ai.Handler
}

func NewAgent(cfg Config) *Agent {
	a := &Agent{
		cfg:        cfg,
		done:       make(chan struct{}),
		buildPgids: make(map[int]bool),
	}
	a.builder = builder.NewHandler(a, core.BuildConfig{ProjectsDir: cfg.ProjectsDir})
	a.terminal = terminal.NewHandler(a)
	a.ai = ai.NewHandler(a)
	return a
}

func (a *Agent) Run() {
	log.Printf("[agent] Starting... Backend=%s Account=%s User=%s Session=%s Duration=%dm",
		a.cfg.BackendURL, a.cfg.AccountID, a.cfg.UserID, a.cfg.SessionID, a.cfg.DurationMinutes)

	go func() {
		time.Sleep(time.Duration(a.cfg.DurationMinutes) * time.Minute)
		log.Printf("[agent] Duration budget (%dm) elapsed — terminating", a.cfg.DurationMinutes)
		a.KillBuild()
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

	header := make(map[string][]string)
	header["Origin"] = []string{a.cfg.BackendURL}
	header["User-Agent"] = []string{"apkbuilder-agent/1.0"}
	c, resp, err := websocket.DefaultDialer.Dial(u.String(), header)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("bad handshake (HTTP %d): %w", resp.StatusCode, err)
		}
		return err
	}
	a.conn = c

	reg := map[string]string{
		"type":      "register",
		"accountId": a.cfg.AccountID,
		"userId":    a.cfg.UserID,
		"sessionId": a.cfg.SessionID,
	}
	if err := a.Send(reg); err != nil {
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

		var msg core.Message
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "registered":
			log.Printf("[agent] Registered: agentId=%s", msg.AgentID)

		case "ping":
			a.Send(map[string]string{"type": "pong"})

		case "build_request":
			var req core.BuildRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				log.Printf("[agent] Bad build_request: %v", err)
				continue
			}
			req.ProjectsDir = a.cfg.ProjectsDir
			go a.builder.HandleBuild(req)

		case "build_cancel":
			var bc struct {
				Type    string `json:"type"`
				BuildID string `json:"buildId"`
			}
			if err := json.Unmarshal(raw, &bc); err != nil {
				continue
			}
			log.Printf("[agent] Cancel requested for build %s", bc.BuildID)
			atomic.StoreInt32(&a.cancelling, 1)
			a.KillBuild()

		case "terminal_data":
			var td struct {
				Type      string `json:"type"`
				SessionID string `json:"sessionId"`
				Data      string `json:"data"`
			}
			if err := json.Unmarshal(raw, &td); err != nil {
				continue
			}
			a.terminal.HandleTerminal(td.SessionID, td.Data)

		case "terminal_start":
			var ts struct {
				Type       string `json:"type"`
				SessionID  string `json:"sessionId"`
				ProjectDir string `json:"projectDir"`
			}
			if err := json.Unmarshal(raw, &ts); err != nil {
				continue
			}
			go a.terminal.StartTerminalVM(ts.SessionID, ts.ProjectDir)

		case "terminal_resize":
			// No-op

		case "ai_request":
			var req core.AIRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				log.Printf("[agent] Bad ai_request: %v", err)
				continue
			}
			go a.ai.HandleAIRequest(req)

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
			a.KillBuild()
			a.conn.Close()
			os.Exit(0)
		}
	}
}

// ─── AgentInterface implementation ────────────────────────────────

func (a *Agent) Send(v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	return a.conn.WriteMessage(websocket.TextMessage, data)
}

func (a *Agent) SendProgress(buildID, line string) {
	log.Printf("[build:%s] %s", buildID, line)
	a.Send(map[string]interface{}{
		"type":    "build_progress",
		"buildId": buildID,
		"line":    line,
	})
}

func (a *Agent) SendBuildFailed(buildID, errMsg string) {
	log.Printf("[build:%s] FAILED: %s", buildID, errMsg)
	a.Send(map[string]interface{}{
		"type":    "build_complete",
		"buildId": buildID,
		"status":  "failed",
		"error":   errMsg,
	})
}

func (a *Agent) SendBuildCancelled(buildID string) {
	log.Printf("[build:%s] CANCELLED", buildID)
	a.Send(map[string]interface{}{
		"type":    "build_complete",
		"buildId": buildID,
		"status":  "cancelled",
	})
}

func (a *Agent) SendTerminalOutput(sessionID, data string) {
	a.Send(map[string]interface{}{
		"type":      "terminal_output",
		"sessionId": sessionID,
		"data":      data,
	})
}

func (a *Agent) Cancelled() bool {
	return atomic.LoadInt32(&a.cancelling) == 1
}

func (a *Agent) KillBuild() {
	a.buildOpsMu.Lock()
	pgids := make([]int, 0, len(a.buildPgids))
	for pgid := range a.buildPgids {
		pgids = append(pgids, pgid)
	}
	a.buildOpsMu.Unlock()

	for _, pgid := range pgids {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}

	for _, c := range a.terminal.Containers() {
		if _, err := runCmd("", "docker", "rm", "-f", c); err != nil {
			log.Printf("[agent] cleanup container %s: %v", c, err)
		}
	}
}

func (a *Agent) RunBuildCmd(dir string, name string, args ...string) (string, error) {
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

func (a *Agent) ArtifactAcks() *sync.Map {
	return &a.artifactAcks
}

func (a *Agent) SendArtifactOverWS(buildID, path string) (string, error) {
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
	if err := a.Send(map[string]interface{}{
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
			if err := a.Send(map[string]interface{}{
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

	if err := a.Send(map[string]interface{}{"type": "artifact_finish", "buildId": buildID}); err != nil {
		return "", err
	}

	select {
	case url := <-respCh:
		return url, nil
	case <-time.After(120 * time.Second):
		return "", fmt.Errorf("timed out waiting for artifact_ack")
	}
}

func runCmd(dir string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
