package builder

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"apkbuilder-agent/core"
)

type Handler struct {
	a core.AgentInterface
	cfg core.BuildConfig
}

func NewHandler(a core.AgentInterface, cfg core.BuildConfig) *Handler {
	return &Handler{a: a, cfg: cfg}
}

func (h *Handler) HandleBuild(req core.BuildRequest) {
	log.Printf("[builder] Build request: buildId=%s framework=%s platform=%s", req.BuildID, req.Meta.Framework, req.Platform)

	projectDir := filepath.Join(h.cfg.ProjectsDir, req.BuildID)
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		h.a.SendBuildFailed(req.BuildID, err.Error())
		return
	}

	for path, content := range req.Files {
		fullPath := filepath.Join(projectDir, filepath.Clean(path))
		dir := filepath.Dir(fullPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			h.a.SendBuildFailed(req.BuildID, err.Error())
			return
		}
		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			h.a.SendBuildFailed(req.BuildID, err.Error())
			return
		}
	}

	h.a.SendProgress(req.BuildID, fmt.Sprintf("Building %s project for %s...", req.Meta.Framework, req.Platform))
	h.a.SendProgress(req.BuildID, fmt.Sprintf("Variant: %s, Format: %s", req.Variant, req.Format))

	h.a.SendProgress(req.BuildID, "Installing dependencies...")
	if h.a.Cancelled() {
		h.a.SendBuildCancelled(req.BuildID)
		return
	}
	if out, err := h.a.RunBuildCmd(projectDir, "npm", "install"); err != nil {
		if h.a.Cancelled() {
			h.a.SendBuildCancelled(req.BuildID)
			return
		}
		h.a.SendProgress(req.BuildID, fmt.Sprintf("npm install failed: %s", out))
		h.a.SendBuildFailed(req.BuildID, err.Error())
		return
	}
	if h.a.Cancelled() {
		h.a.SendBuildCancelled(req.BuildID)
		return
	}
	h.a.SendProgress(req.BuildID, "Dependencies installed")

	switch req.Meta.Framework {
	case "react-native":
		h.buildReactNative(projectDir, req)
	case "hybrid":
		h.buildHybrid(projectDir, req)
	default:
		h.a.SendBuildFailed(req.BuildID, fmt.Sprintf("Unknown framework: %s", req.Meta.Framework))
		return
	}

	if h.a.Cancelled() {
		h.a.SendBuildCancelled(req.BuildID)
		return
	}

	artifact := h.findArtifact(projectDir, req.Format)
	if artifact != "" {
		h.a.SendProgress(req.BuildID, fmt.Sprintf("Build succeeded: %s", artifact))

		arrURL, err := h.a.SendArtifactOverWS(req.BuildID, artifact)
		if err != nil {
			h.a.SendProgress(req.BuildID, fmt.Sprintf("Artifact upload failed: %v", err))
		}

		msg := map[string]interface{}{
			"type":    "build_complete",
			"buildId": req.BuildID,
			"status":  "succeeded",
		}
		if arrURL != "" {
			msg["artifactUrl"] = arrURL
		}
		h.a.Send(msg)
	} else {
		h.a.SendProgress(req.BuildID, "Build completed but no artifact found")
		h.a.Send(map[string]interface{}{
			"type":    "build_complete",
			"buildId": req.BuildID,
			"status":  "succeeded",
		})
	}
}

func (h *Handler) buildReactNative(projectDir string, req core.BuildRequest) {
	gradlew := filepath.Join(projectDir, "android", "gradlew")

	if _, err := os.Stat(gradlew); err != nil {
		h.a.SendProgress(req.BuildID, "Running expo prebuild (clean)...")
		if out, err := h.a.RunBuildCmd(projectDir, "npx", "expo", "prebuild", "--platform", "android", "--no-install", "--clean"); err != nil {
			if h.a.Cancelled() {
				h.a.SendBuildCancelled(req.BuildID)
				return
			}
			h.a.SendProgress(req.BuildID, "Prebuild warnings, continuing...")
			_ = out
		}
	} else {
		h.a.SendProgress(req.BuildID, "Running expo prebuild...")
		h.a.RunBuildCmd(projectDir, "npx", "expo", "prebuild", "--platform", "android", "--no-install")
	}

	if h.a.Cancelled() {
		h.a.SendBuildCancelled(req.BuildID)
		return
	}

	if req.Platform == "web" {
		h.a.SendProgress(req.BuildID, "Building web bundle...")
		if out, err := h.a.RunBuildCmd(projectDir, "npx", "expo", "export", "--platform", "web"); err != nil {
			if h.a.Cancelled() {
				h.a.SendBuildCancelled(req.BuildID)
				return
			}
			h.a.SendBuildFailed(req.BuildID, out)
			return
		}
		return
	}

	gradleTask := "assembleDebug"
	if req.Variant == "release" {
		gradleTask = "bundleRelease"
	}

	if _, err := os.Stat(gradlew); err != nil {
		h.a.SendBuildFailed(req.BuildID, "No gradlew found after prebuild; cannot run gradle build")
		return
	}

	h.a.SendProgress(req.BuildID, fmt.Sprintf("Running gradle %s...", gradleTask))
	h.a.RunBuildCmd(projectDir, "chmod", "+x", gradlew)
	if out, err := h.a.RunBuildCmd(filepath.Join(projectDir, "android"), gradlew, gradleTask, "--no-daemon"); err != nil {
		if h.a.Cancelled() {
			h.a.SendBuildCancelled(req.BuildID)
			return
		}
		h.a.SendBuildFailed(req.BuildID, out)
		return
	}
}

func (h *Handler) buildHybrid(projectDir string, req core.BuildRequest) {
	h.a.SendProgress(req.BuildID, "Building Vite bundle...")
	if out, err := h.a.RunBuildCmd(projectDir, "npx", "vite", "build"); err != nil {
		if h.a.Cancelled() {
			h.a.SendBuildCancelled(req.BuildID)
			return
		}
		h.a.SendBuildFailed(req.BuildID, out)
		return
	}

	if req.Platform == "android" || req.Platform == "ios" {
		h.a.SendProgress(req.BuildID, fmt.Sprintf("Adding %s platform...", req.Platform))
		h.a.RunBuildCmd(projectDir, "npx", "cap", "add", req.Platform)
		h.a.SendProgress(req.BuildID, "Syncing platform...")
		h.a.RunBuildCmd(projectDir, "npx", "cap", "sync", req.Platform)
	}
}

func (h *Handler) findArtifact(projectDir, format string) string {
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
