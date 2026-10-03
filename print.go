package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/eodomo/nextpage/internal/agent"
	"github.com/eodomo/nextpage/internal/llm"
)

// runPrint answers one prompt without the TUI. Tool calls that would need
// approval are refused (use --allowedTools or --permission-mode to grant
// them), questions get a "no user available" answer.
func runPrint(ag *agent.Agent, prompt string, opts options) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	enc := json.NewEncoder(os.Stdout)
	streamJSON := opts.outputFormat == "stream-json"

	emit := func(ev agent.Event) {
		switch e := ev.(type) {
		case agent.PermissionRequest:
			e.Reply <- agent.PermissionReply{
				Feedback: "This tool call needs approval, and no user is available in non-interactive mode. Continue without it if you can, otherwise explain what needs to be run.",
			}
		case agent.QuestionRequest:
			e.Reply <- "(no user is available in non-interactive mode; make a reasonable assumption and state it)"
		case agent.PlanRequest:
			e.Reply <- false
		case agent.Notice:
			fmt.Fprintln(os.Stderr, e.Text)
		case agent.ToolStart:
			if opts.verbose {
				fmt.Fprintf(os.Stderr, "● %s(%s)\n", e.Name, e.Subject)
			}
		case agent.ToolEnd:
			if opts.verbose && e.Result.IsError {
				fmt.Fprintf(os.Stderr, "  ⎿ error: %s\n", firstLine(e.Result.Output))
			}
		}
		if streamJSON {
			_ = enc.Encode(map[string]any{"type": fmt.Sprintf("%T", ev)[len("agent."):], "event": ev})
		}
	}

	err := ag.Run(ctx, prompt, emit)
	result := lastAssistantText(ag.History())
	usage, _ := ag.Usage()
	sessionID := ""
	if ag.Session != nil {
		sessionID = ag.Session.ID
	}

	switch opts.outputFormat {
	case "json", "stream-json":
		out := map[string]any{
			"type":       "result",
			"is_error":   err != nil,
			"result":     result,
			"session_id": sessionID,
			"usage":      usage,
		}
		if err != nil {
			out["error"] = err.Error()
		}
		_ = enc.Encode(out)
	default:
		if result != "" {
			fmt.Println(result)
		}
	}
	return err
}

func lastAssistantText(h []llm.Message) string {
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Role == llm.RoleAssistant && strings.TrimSpace(h[i].Content) != "" {
			return strings.TrimSpace(h[i].Content)
		}
	}
	return ""
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}
