package terminal

import (
	"io"
	"log"
	"os"
	"os/exec"
	"strings"

	"apkbuilder-agent/core"
)

type vmShell struct {
	Container string
	Exec      *exec.Cmd
	Stdin     io.WriteCloser
}

type Handler struct {
	a core.AgentInterface

	// Terminal state
	vms     map[string]*vmShell
	shells  map[string]*exec.Cmd
	stdinW  map[string]io.WriteCloser
	pending map[string][]byte
}

func NewHandler(a core.AgentInterface) *Handler {
	return &Handler{
		a:       a,
		vms:     make(map[string]*vmShell),
		shells:  make(map[string]*exec.Cmd),
		stdinW:  make(map[string]io.WriteCloser),
		pending: make(map[string][]byte),
	}
}

func (h *Handler) StartTerminalVM(sessionID, projectDir string) {
	if _, ok := h.vms[sessionID]; ok {
		return
	}

	container := "apkbuilder-" + strings.ReplaceAll(sessionID, "-", "")
	if len(container) > 24 {
		container = container[:24]
	}
	log.Printf("[terminal] Provisioning VM for session %s (docker=%s)", sessionID, container)

	if _, err := ensureContainer(container, "node:20-alpine"); err != nil {
		log.Printf("[terminal] VM provision failed (%v): %v", container, err)
		h.startHostShell(sessionID)
		return
	}

	runCmd("", "docker", "exec", container, "sh", "-c", "mkdir -p /workspace")

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

	vm := &vmShell{Container: container, Exec: cmd, Stdin: stdin}
	h.vms[sessionID] = vm

	if buf, ok := h.pending[sessionID]; ok {
		vm.Stdin.Write(buf)
		delete(h.pending, sessionID)
	}

	pipeOutput := func(r io.Reader) {
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				h.a.SendTerminalOutput(sessionID, string(buf[:n]))
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
		h.a.SendTerminalOutput(sessionID, "\r\n\x1b[33m[Shell exited]\x1b[0m\r\n")
		delete(h.vms, sessionID)
		runCmd("", "docker", "rm", "-f", container)
	}()

	h.a.SendTerminalOutput(sessionID, "\r\n\x1b[1;32mShell ready\x1b[0m\r\n")

	if projectDir != "" {
		if _, err := os.Stat(projectDir); err == nil {
			go func() {
				runCmd("", "docker", "cp", projectDir+"/.", container+":/workspace")
				h.a.SendTerminalOutput(sessionID, "\r\n\x1b[1;32mProject source copied to /workspace\x1b[0m\r\n")
			}()
		}
	}
}

func (h *Handler) startHostShell(sessionID string) {
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
	h.shells[sessionID] = cmd

	stdoutReader := func(r io.Reader) {
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				h.a.SendTerminalOutput(sessionID, string(buf[:n]))
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
		h.a.SendTerminalOutput(sessionID, "\r\n\x1b[33m[Shell exited]\x1b[0m\r\n")
		delete(h.shells, sessionID)
	}()
	h.stdinW[sessionID] = stdin
}

func (h *Handler) HandleTerminal(sessionID, data string) {
	if vm, ok := h.vms[sessionID]; ok {
		vm.Stdin.Write([]byte(data))
		return
	}
	if w, ok := h.stdinW[sessionID]; ok {
		w.Write([]byte(data))
		return
	}
	h.pending[sessionID] = append(h.pending[sessionID], []byte(data)...)
}

// Containers returns running containers (for killBuild cleanup).
func (h *Handler) Containers() []string {
	containers := make([]string, 0, len(h.vms))
	for _, vm := range h.vms {
		containers = append(containers, vm.Container)
	}
	return containers
}

func runCmd(dir string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func ensureContainer(name, image string) (string, error) {
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
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "no such") {
		runCmd("", "docker", "rm", "-f", name)
	}
	out, err = runCmd("", "docker", "run", "-d", "-it", "--name", name, image, "sleep", "infinity")
	if err != nil {
		if _, e := runCmd("", "docker", "start", name); e == nil {
			return "reused-race", nil
		}
		return out, err
	}
	return "created", nil
}
