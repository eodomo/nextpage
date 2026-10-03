package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/eodomo/nextpage/internal/ext"
	"github.com/eodomo/nextpage/internal/tools"
)

// Tools a sub-agent never gets: it can't spawn further agents, talk to the
// user, or touch the parent's todo list and plan state.
var subagentExcluded = []string{"Task", "TodoWrite", "AskUserQuestion", "ExitPlanMode"}

const generalPurposePrompt = `You are a sub-agent handling one self-contained task for a parent agent. Use the available tools to complete it, then reply with a concise final report containing exactly what the parent needs (findings, file paths with line numbers, what you changed). The parent only sees your final message.`

func (a *Agent) spawn(ctx context.Context, agentType, description, prompt string) (string, error) {
	var def *ext.AgentDef
	if agentType != "" && agentType != "general-purpose" {
		for i := range a.Env.Agents {
			if a.Env.Agents[i].Name == agentType {
				def = &a.Env.Agents[i]
			}
		}
		if def == nil {
			return "", fmt.Errorf("unknown subagent_type %q", agentType)
		}
	}

	reg := a.Tools.Filter(func(t tools.Tool) bool {
		if slices.Contains(subagentExcluded, t.Name()) {
			return false
		}
		return def == nil || len(def.Tools) == 0 || slices.Contains(def.Tools, t.Name())
	})

	cfg := a.Config
	cfg.Tools = reg
	cfg.Session = nil
	cfg.AutoCompact = false
	parentPrompt := a.SystemPrompt
	instructions := generalPurposePrompt
	if def != nil {
		instructions = def.Prompt
		if def.Model != "" && def.Model != "inherit" {
			cfg.Model = def.Model
		}
	}
	cfg.SystemPrompt = func() string {
		return instructions + "\n\n" + parentPrompt()
	}

	child := &Agent{Config: cfg, name: description}
	if child.name == "" {
		child.name = "sub-agent"
	}
	a.mu.Lock()
	parentEmit := a.emitFunc
	a.mu.Unlock()
	if err := child.Run(ctx, prompt, parentEmit); err != nil {
		return "", err
	}

	h := child.History()
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Role == "assistant" && strings.TrimSpace(h[i].Content) != "" {
			usage, _ := child.Usage()
			a.mu.Lock()
			a.usage.Add(usage)
			a.mu.Unlock()
			return h[i].Content, nil
		}
	}
	return "(the sub-agent finished without a report)", nil
}
