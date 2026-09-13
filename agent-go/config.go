package main

import (
	"os"
	"strconv"
)

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
