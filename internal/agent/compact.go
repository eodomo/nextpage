package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/eodomo/nextpage/internal/hooks"
	"github.com/eodomo/nextpage/internal/llm"
)

const compactPrompt = `Your task is to create a detailed summary of the conversation so far. It will replace the conversation history, so it must preserve everything needed to continue the work without losing context.

Include:
1. The user's requests and intent, in order.
2. Key technical decisions, concepts and constraints.
3. Files examined or modified, with the important details of each change.
4. Errors encountered and how they were fixed.
5. Pending tasks and exactly what was being worked on most recently.
6. The next step, if one was in progress.

Respond with the summary only. Do not call any tools.`

// Compact replaces the conversation history with a model-written summary.
func (a *Agent) Compact(ctx context.Context, instructions string, emit func(Event)) error {
	a.setEmit(emit)
	defer a.setEmit(nil)
	return a.compact(ctx, instructions, "manual")
}

func (a *Agent) compact(ctx context.Context, instructions string, trigger string) error {
	if len(a.History()) == 0 {
		return fmt.Errorf("nothing to compact")
	}
	in := a.hookInput(hooks.PreCompact)
	in.Trigger = trigger
	a.reportHookErrors(a.Hooks.Run(ctx, in))

	prompt := compactPrompt
	if instructions != "" {
		prompt += "\n\nAdditional instructions from the user: " + instructions
	}
	a.mu.Lock()
	a.history = append(a.history, llm.Message{Role: llm.RoleUser, Content: prompt})
	a.mu.Unlock()

	resp, err := a.complete(ctx, nil, false)

	a.mu.Lock()
	a.history = a.history[:len(a.history)-1]
	a.mu.Unlock()
	if err != nil {
		return err
	}
	summary := strings.TrimSpace(resp.Message.Content)
	if summary == "" {
		return fmt.Errorf("model returned an empty summary")
	}

	_ = a.Session.MarkCompact()
	a.mu.Lock()
	a.history = nil
	a.ctxToks = 0
	a.mu.Unlock()
	a.appendMsg(llm.Message{
		Role:    llm.RoleUser,
		Harness: true,
		Content: "This session is being continued from a previous conversation that was compacted. Summary of the earlier conversation:\n\n" +
			summary + "\n\nContinue from where things left off without asking the user to repeat anything.",
	})
	return nil
}

func (a *Agent) maybeAutoCompact(ctx context.Context) error {
	if !a.AutoCompact || a.NumCtx <= 0 {
		return nil
	}
	_, used := a.Usage()
	if used < a.NumCtx*85/100 {
		return nil
	}
	a.emit(Notice{Text: fmt.Sprintf("Context %d/%d tokens; compacting conversation...", used, a.NumCtx)})
	if err := a.compact(ctx, "", "auto"); err != nil {
		a.emit(Notice{Level: NoticeWarn, Text: "auto-compact failed: " + err.Error()})
	}
	return nil
}
