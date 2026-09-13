package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	cfg := loadConfig()
	agent := NewAgent(cfg)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		log.Println("[agent] Received signal, cleaning up...")
		agent.KillBuild()
		if agent.conn != nil {
			agent.conn.Close()
		}
		os.Exit(0)
	}()

	agent.Run()
}
