// Package agent implements the agentic loop: send the conversation to the
// model, run any tool calls it makes (subject to permissions and hooks),
// feed the results back, and repeat until the model answers without tools.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/eodomo/nextpage/internal/config"
	"github.com/eodomo/nextpage/internal/hooks"
	"github.com/eodomo/nextpage/internal/llm"
	"github.com/eodomo/nextpage/internal/permission"
	"github.com/eodomo/nextpage/internal/session"
	"github.com/eodomo/nextpage/internal/tools"
)

type Config struct {
	Provider llm.Provider
	Model    string
	Tools    *tools.Registry
	Perms    *permission.Checker
	Hooks    *hooks.Runner
	Env      *tools.Env
	Session  *session.Store // nil disables persistence
	// SystemPrompt is rebuilt every turn so it can reflect the current mode.
	SystemPrompt func() string

	Think       bool
	NumCtx      int
	Temperature *float64
	MaxTurns    int
	AutoCompact bool
	// LocalSettings is where "always allow" rules are saved.
	LocalSettings string
}

type Agent struct {
	Config
	name string // "" for the main agent

	mu       sync.Mutex
	history  []llm.Message
	usage    llm.Usage
	ctxToks  int
	emitFunc func(Event)
}

// ErrBlocked is returned when a UserPromptSubmit hook rejects the prompt.
var ErrBlocked = errors.New("prompt blocked by hook")

// errStopTurn ends the loop after the user rejects a tool call without
// giving feedback, handing control back to them.
var errStopTurn = errors.New("stopped by user")

func New(cfg Config) *Agent {
	if cfg.MaxTurns == 0 {
		cfg.MaxTurns = 100
	}
	a := &Agent{Config: cfg}
	if cfg.Env != nil {
		cfg.Env.Spawn = a.spawn
		cfg.Env.Ask = a.ask
		cfg.Env.ExitPlan = a.exitPlan
		cfg.Env.OnTodos = func(t []tools.Todo) { a.emit(TodosUpdate{Todos: t}) }
	}
	return a
}

func (a *Agent) setEmit(emit func(Event)) {
	a.mu.Lock()
	a.emitFunc = emit
	a.mu.Unlock()
}

func (a *Agent) emit(e Event) {
	a.mu.Lock()
	f := a.emitFunc
	a.mu.Unlock()
	if f != nil {
		f(e)
	}
}

func (a *Agent) History() []llm.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]llm.Message(nil), a.history...)
}

func (a *Agent) SetHistory(h []llm.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.history = h
	a.ctxToks = 0
}

func (a *Agent) Usage() (llm.Usage, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.usage, a.ctxToks
}

func (a *Agent) SetModel(m string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Model = m
}

// CurrentModel returns the model in use; safe to call from any goroutine.
func (a *Agent) CurrentModel() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Model
}

func (a *Agent) appendMsg(m llm.Message) {
	a.mu.Lock()
	a.history = append(a.history, m)
	a.mu.Unlock()
	if err := a.Session.Append(m); err != nil {
		log.Printf("session: %v", err)
	}
}

// AddContext appends a user-role message without running the model, e.g.
// the output of a "!" shell command.
func (a *Agent) AddContext(content string) {
	a.appendMsg(llm.Message{Role: llm.RoleUser, Content: content})
}

func (a *Agent) hookInput(event string) hooks.Input {
	in := hooks.Input{HookEventName: event, Cwd: a.Env.Cwd(), PermissionMode: a.Perms.Mode().String()}
	if a.Session != nil {
		in.SessionID = a.Session.ID
		in.TranscriptPath = a.Session.Path
	}
	return in
}

// Run sends prompt and drives the tool loop until the model finishes, the
// turn limit is hit, or ctx is cancelled.
func (a *Agent) Run(ctx context.Context, prompt string, emit func(Event)) error {
	a.setEmit(emit)
	defer a.setEmit(nil)

	in := a.hookInput(hooks.UserPromptSubmit)
	in.Prompt = prompt
	out := a.Hooks.Run(ctx, in)
	a.reportHookErrors(out)
	if out.Block {
		return fmt.Errorf("%w: %s", ErrBlocked, out.Reason)
	}
	if out.Context != "" {
		prompt += "\n\n<system-reminder>\n" + out.Context + "\n</system-reminder>"
	}
	a.appendMsg(llm.Message{Role: llm.RoleUser, Content: prompt})
	return a.loop(ctx)
}

func (a *Agent) loop(ctx context.Context) error {
	for turn := 0; turn < a.MaxTurns; turn++ {
		resp, err := a.complete(ctx, a.Tools.Specs(), true)
		if err != nil {
			if ctx.Err() != nil {
				a.appendMsg(llm.Message{Role: llm.RoleUser, Content: "[Request interrupted by user]"})
			}
			return err
		}
		msg := resp.Message
		a.appendMsg(msg)
		a.emit(AssistantMessage{Agent: a.name, Message: msg})

		if len(msg.ToolCalls) == 0 {
			event := hooks.Stop
			if a.name != "" {
				event = hooks.SubagentStop
			}
			out := a.Hooks.Run(ctx, a.hookInput(event))
			a.reportHookErrors(out)
			if out.Block && out.Reason != "" {
				a.appendMsg(llm.Message{Role: llm.RoleUser, Content: "Stop hook feedback:\n" + out.Reason})
				continue
			}
			return a.maybeAutoCompact(ctx)
		}

		for i, call := range msg.ToolCalls {
			if ctx.Err() != nil {
				for _, c := range msg.ToolCalls[i:] {
					a.appendToolResult(c, tools.Result{Output: "Tool call cancelled: interrupted by user", IsError: true})
				}
				a.appendMsg(llm.Message{Role: llm.RoleUser, Content: "[Request interrupted by user]"})
				return ctx.Err()
			}
			res, err := a.execTool(ctx, call)
			a.appendToolResult(call, res)
			if errors.Is(err, errStopTurn) {
				for _, c := range msg.ToolCalls[i+1:] {
					a.appendToolResult(c, tools.Result{Output: "Tool call skipped: the user rejected an earlier call", IsError: true})
				}
				return nil
			}
		}
		if err := a.maybeAutoCompact(ctx); err != nil {
			return err
		}
	}
	a.emit(Notice{Level: NoticeWarn, Text: fmt.Sprintf("Stopped after %d turns (maxTurns).", a.MaxTurns)})
	return nil
}

func (a *Agent) options() map[string]any {
	opts := map[string]any{}
	if a.NumCtx > 0 {
		opts["num_ctx"] = a.NumCtx
	}
	if a.Temperature != nil {
		opts["temperature"] = *a.Temperature
	}
	return opts
}

func (a *Agent) complete(ctx context.Context, specs []llm.ToolSpec, stream bool) (llm.Response, error) {
	msgs := []llm.Message{{Role: llm.RoleSystem, Content: a.SystemPrompt()}}
	msgs = append(msgs, a.History()...)
	resp, err := a.Provider.Chat(ctx, llm.Request{
		Model:    a.CurrentModel(),
		Messages: msgs,
		Tools:    specs,
		Think:    a.Think,
		Options:  a.options(),
	}, func(d llm.Delta) {
		if !stream {
			return
		}
		if d.Thinking != "" {
			a.emit(ThinkingDelta{Agent: a.name, Text: d.Thinking})
		}
		if d.Content != "" {
			a.emit(TextDelta{Agent: a.name, Text: d.Content})
		}
	})
	if err != nil {
		return resp, err
	}
	a.mu.Lock()
	a.usage.Add(resp.Usage)
	if resp.Usage.PromptTokens > 0 {
		a.ctxToks = resp.Usage.PromptTokens + resp.Usage.CompletionTokens
	}
	total, ctxToks := a.usage, a.ctxToks
	a.mu.Unlock()
	a.emit(UsageUpdate{Total: total, ContextTokens: ctxToks})
	return resp, nil
}

func (a *Agent) appendToolResult(call llm.ToolCall, res tools.Result) {
	content := res.Output
	if res.IsError && !strings.HasPrefix(content, "Error") {
		content = "Error: " + content
	}
	a.appendMsg(llm.Message{Role: llm.RoleTool, Content: content, ToolCallID: call.ID, ToolName: call.Name})
}

func (a *Agent) execTool(ctx context.Context, call llm.ToolCall) (tools.Result, error) {
	t, ok := a.Tools.Get(call.Name)
	if !ok {
		res := tools.Errorf("no such tool %q", call.Name)
		a.emit(ToolEnd{Agent: a.name, ID: call.ID, Name: call.Name, Result: res})
		return res, nil
	}
	input := call.Args
	if len(input) == 0 || string(input) == "null" {
		input = json.RawMessage("{}")
	}
	subject := tools.Subject(t, input)
	a.emit(ToolStart{Agent: a.name, ID: call.ID, Name: call.Name, Subject: subject, Input: input})

	finish := func(res tools.Result, err error) (tools.Result, error) {
		a.emit(ToolEnd{Agent: a.name, ID: call.ID, Name: call.Name, Result: res})
		return res, err
	}

	hin := a.hookInput(hooks.PreToolUse)
	hin.ToolName, hin.ToolInput = call.Name, input
	pre := a.Hooks.Run(ctx, hin)
	a.reportHookErrors(pre)
	if pre.Block || pre.PermissionDecision == "deny" {
		return finish(tools.Errorf("Blocked by PreToolUse hook: %s", pre.Reason), nil)
	}

	decision := a.Perms.Check(t, input)
	switch {
	case decision.Behavior == permission.Deny:
		return finish(tools.Errorf("Permission denied: %s", decision.Reason), nil)
	case pre.PermissionDecision == "allow":
	case decision.Behavior == permission.Ask || pre.PermissionDecision == "ask":
		reply, err := a.askPermission(ctx, t, call, subject, input)
		if err != nil {
			return finish(tools.Errorf("Permission request cancelled: %v", err), err)
		}
		if !reply.Allow {
			if reply.Feedback != "" {
				return finish(tools.Errorf("The user rejected this tool call and said: %s", reply.Feedback), nil)
			}
			return finish(tools.Errorf("The user rejected this tool call. Stop and wait for their instructions."), errStopTurn)
		}
	}

	res := t.Run(ctx, a.Env, input)

	hin = a.hookInput(hooks.PostToolUse)
	hin.ToolName, hin.ToolInput, hin.ToolResponse = call.Name, input, res.Output
	post := a.Hooks.Run(ctx, hin)
	a.reportHookErrors(post)
	if post.Block {
		res.Output += "\n\nPostToolUse hook feedback: " + post.Reason
	}
	if post.Context != "" {
		res.Output += "\n\n<system-reminder>" + post.Context + "</system-reminder>"
	}
	return finish(res, nil)
}

func (a *Agent) askPermission(ctx context.Context, t tools.Tool, call llm.ToolCall, subject string, input json.RawMessage) (PermissionReply, error) {
	req := PermissionRequest{
		Agent:      a.name,
		ToolName:   call.Name,
		Subject:    subject,
		Input:      input,
		Preview:    preview(t, input),
		Suggestion: permission.SuggestRule(t, input),
		Reply:      make(chan PermissionReply, 1),
	}
	n := a.hookInput(hooks.Notification)
	n.Message = fmt.Sprintf("nextpage needs your permission to use %s", call.Name)
	go a.Hooks.Run(context.Background(), n)

	a.emit(req)
	select {
	case reply := <-req.Reply:
		switch reply.Remember {
		case RememberSession:
			a.Perms.AddAllow(req.Suggestion)
		case RememberProject:
			a.Perms.AddAllow(req.Suggestion)
			if err := config.AddAllowRule(a.LocalSettings, req.Suggestion); err != nil {
				a.emit(Notice{Level: NoticeError, Text: "saving permission rule: " + err.Error()})
			}
		}
		return reply, nil
	case <-ctx.Done():
		return PermissionReply{}, ctx.Err()
	}
}

// preview renders what a pending edit would change, for the permission dialog.
func preview(t tools.Tool, input json.RawMessage) string {
	var a struct {
		Content   string `json:"content"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
		Command   string `json:"command"`
	}
	_ = json.Unmarshal(input, &a)
	switch t.Name() {
	case "Edit":
		return tools.Diff(a.OldString, a.NewString)
	case "Write":
		return tools.Diff("", a.Content)
	case "Bash":
		return a.Command
	}
	return string(input)
}

func (a *Agent) reportHookErrors(o hooks.Outcome) {
	for _, e := range o.Errors {
		a.emit(Notice{Level: NoticeWarn, Text: e})
	}
}

func (a *Agent) ask(ctx context.Context, q tools.Question) (string, error) {
	req := QuestionRequest{Question: q, Reply: make(chan string, 1)}
	a.emit(req)
	select {
	case ans := <-req.Reply:
		return ans, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (a *Agent) exitPlan(ctx context.Context, plan string) (bool, error) {
	if a.Perms.Mode() != permission.ModePlan {
		return true, nil
	}
	req := PlanRequest{Plan: plan, Reply: make(chan bool, 1)}
	a.emit(req)
	select {
	case ok := <-req.Reply:
		if ok {
			a.Perms.SetMode(permission.ModeDefault)
		}
		return ok, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}
