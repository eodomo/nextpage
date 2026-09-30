package main

import (
	"fmt"
	"log"
	"os"

	tea "charm.land/bubbletea/v2"
)

type streamEvent struct {
	chunk string
	err   error
}

type chunkMsg string
type requestErrorMsg struct{ err error }
type requestDoneMsg struct{ err error }

func runRequest(prompt string, events chan<- streamEvent) tea.Cmd {
	return func() tea.Msg {
		defer close(events)

		err := SendRequest(prompt, func(chunk string) error {
			events <- streamEvent{chunk: chunk}
			return nil
		})
		if err != nil {
			events <- streamEvent{err: err}
		}
		return nil
	}
}

func waitForEvent(events <-chan streamEvent) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-events
		if !ok {
			return requestDoneMsg{}
		}
		if event.err != nil {
			return requestErrorMsg{event.err}
		}
		return chunkMsg(event.chunk)
	}
}

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
