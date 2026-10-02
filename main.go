package main

import (
	"fmt"
	"log"
	"os"

	tea "charm.land/bubbletea/v2"
)

func main() {
	logPath := os.Getenv("LOGPATH")
	if logPath == "" {
		log.Fatal("set LOGPATH")
	}
	logFile, err := os.OpenFile(os.Getenv("LOGPATH"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
	if err != nil {
		log.Fatal(err)
	}
	defer logFile.Close()
	log.SetOutput(logFile)

	p := tea.NewProgram(initialModel())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Oof: %v\n", err)
	}
}
