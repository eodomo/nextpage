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

	Think bool
	// ThinkExplicit sends Think even when false, turning thinking off for
	// models that think by default. Otherwise false means "model default".
	ThinkExplicit bool
	NumCtx        int
	Temperature   *float64
	MaxTurns      int
	// MaxTokens caps each reply (thinking included) so a model stuck in a
	// repetition loop can't run for hours on a slow server.
	MaxTokens   int
	AutoCompact bool
	// LocalSettings is where "always allow" rules are saved.
	LocalSettings string
	// StopCheck runs when the model ends its turn without calling a tool.
	// A non-empty result is sent back as a reminder and the loop continues
	// (at most four times per prompt). Plugins use it to keep the model moving
	// through steps that don't need the user.
	StopCheck func() string
	// OnPrompt is told about each prompt the user sends (plugins use it to
	// know the user has been back, e.g. after reading a lesson).
	OnPrompt func(prompt string)
}

type Agent struct {
	Config
	name string // "" for the main agent

	mu       sync.Mutex
	capture  *capture
	captures int
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
		cfg.Env.Interact = a.interact
		cfg.Env.CaptureReply = a.captureReply
		cfg.Env.Complete = a.completeText
	}
	return a
}

func (a *Agent) setEmit(emit func(Event)) {
	a.mu.Lock()
	a.emitFunc = emit
	a.mu.Unlock()
}

// debug records harness housekeeping in the log and as a hidden notice.
func (a *Agent) debug(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Printf("agent%s: %s", a.logName(), msg)
	a.emit(Notice{Level: NoticeDebug, Text: msg})
}

func (a *Agent) logName() string {
	if a.name == "" {
		return ""
	}
	return "[" + a.name + "]"
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

// SetProvider swaps the model backend (e.g. after the user changes the server
// in the web app's settings).
func (a *Agent) SetProvider(p llm.Provider) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Provider = p
}

func (a *Agent) CurrentProvider() llm.Provider {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Provider
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

func truncateLog(s string) string {
	if len(s) > 500 {
		return s[:500] + "…"
	}
	return s
}

func (a *Agent) appendHarness(content string) {
	a.appendMsg(llm.Message{Role: llm.RoleUser, Content: content, Harness: true})
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
	if a.OnPrompt != nil && a.name == "" {
		a.OnPrompt(prompt)
	}
	a.appendMsg(llm.Message{Role: llm.RoleUser, Content: prompt})
	// A capture left over from an interrupted run must not swallow the reply
	// to a new prompt.
	a.mu.Lock()
	a.capture = nil
	a.mu.Unlock()
	return a.loop(ctx)
}

// emptyNudge is sent when the model replies with nothing at all. With small
// local models this usually means a tool call it generated couldn't be
// parsed and was dropped, so it is told to retry.
const emptyNudge = "[Your last reply was empty. If you tried to call a tool, the call could not be parsed: try again, keeping the arguments short and simple. Otherwise, reply to the user.]"

// narrationNudge is sent when the model names one of its tools in a reply
// without calling it, a common failure of small models.
const narrationNudge = "[You described calling %s but did not call it. Call the tool now instead of describing it.]"

const lengthNudge = "[Your last reply ran out of tokens before finishing, so it was lost. Try again with less thinking and shorter tool arguments.]"

func (a *Agent) loop(ctx context.Context) error {
	nudges, stopNudges := 0, 0
	lastFailure, repeats := "", 0
	for turn := 0; turn < a.MaxTurns; turn++ {
		resp, err := a.complete(ctx, a.Tools.Specs(), true)
		if err != nil {
			if ctx.Err() != nil {
				a.appendHarness("[Request interrupted by user]")
			}
			return err
		}
		msg := resp.Message
		if len(msg.ToolCalls) == 0 && strings.TrimSpace(msg.Content) != "" && a.captureArmed() {
			// The reply is content for a tool (e.g. a lesson file), not a
			// chat message; keep it out of reloaded transcripts.
			msg.Harness = true
		}
		a.appendMsg(msg)
		a.emit(AssistantMessage{Agent: a.name, Message: msg})

		if len(msg.ToolCalls) == 0 && a.takeCapture(ctx, msg.Content) {
			continue
		}
		if resp.DoneReason == "length" {
			a.debug("reply hit the %d-token output limit (maxTokens) and was cut off", a.MaxTokens)
		}
		if len(msg.ToolCalls) == 0 && strings.TrimSpace(msg.Content) == "" && nudges < 2 {
			nudges++
			a.debug("empty reply; asking the model to try again")
			nudge := emptyNudge
			if resp.DoneReason == "length" {
				nudge = lengthNudge
			}
			a.appendHarness(nudge)
			continue
		}
		if len(msg.ToolCalls) == 0 && nudges < 2 {
			if name := a.mentionedTool(msg.Content); name != "" {
				nudges++
				a.debug("model described %s without calling it; nudging", name)
				a.appendHarness(fmt.Sprintf(narrationNudge, name))
				continue
			}
		}
		if len(msg.ToolCalls) == 0 && a.StopCheck != nil && stopNudges < 4 {
			if reminder := a.StopCheck(); reminder != "" {
				stopNudges++
				a.debug("stop check: %s", reminder)
				a.appendHarness("[" + reminder + "]")
				continue
			}
		}
		if len(msg.ToolCalls) == 0 {
			event := hooks.Stop
			if a.name != "" {
				event = hooks.SubagentStop
			}
			out := a.Hooks.Run(ctx, a.hookInput(event))
			a.reportHookErrors(out)
			if out.Block && out.Reason != "" {
				a.appendHarness("Stop hook feedback:\n" + out.Reason)
				continue
			}
			return a.maybeAutoCompact(ctx)
		}

		for i, call := range msg.ToolCalls {
			if ctx.Err() != nil {
				for _, c := range msg.ToolCalls[i:] {
					a.appendToolResult(c, tools.Result{Output: "Tool call cancelled: interrupted by user", IsError: true})
				}
				a.appendHarness("[Request interrupted by user]")
				return ctx.Err()
			}
			res, err := a.execTool(ctx, call)
			a.appendToolResult(call, res)
			if res.EndTurn {
				for _, c := range msg.ToolCalls[i+1:] {
					a.appendToolResult(c, tools.Result{Output: "Tool call skipped: waiting for the user", IsError: true})
				}
				if res.Display != "" {
					a.emit(Notice{Text: res.Display})
				}
				return nil
			}
			// Stop a model that keeps making the same failing call.
			if res.IsError {
				key := call.Name + string(call.Args) + res.Output
				if key == lastFailure {
					repeats++
				} else {
					lastFailure, repeats = key, 1
				}
				if repeats >= 3 {
					log.Printf("agent%s: stopped after 3 identical failing %s calls; last error: %s", a.logName(), call.Name, res.Output)
					a.emit(Notice{Level: NoticeError, Text: "Sorry, I got stuck on that. Please try again, or rephrase your request."})
					return nil
				}
			} else {
				lastFailure, repeats = "", 0
			}
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
	log.Printf("agent%s: stopped after %d turns (maxTurns)", a.logName(), a.MaxTurns)
	a.emit(Notice{Level: NoticeWarn, Text: "I stopped after a lot of steps without finishing. Tell me how to continue."})
	return nil
}

func (a *Agent) thinkOption() *bool {
	if !a.Think && !a.ThinkExplicit {
		return nil
	}
	v := a.Think
	return &v
}

// mentionedTool returns the name of a tool the text refers to as if calling
// it, or "". Only distinctive (CamelCase, multi-word) names count, so
// ordinary words like "Read" or "Bash" in prose don't trigger it.
func (a *Agent) mentionedTool(text string) string {
	for _, t := range a.Tools.All() {
		name := t.Name()
		if len(name) < 8 || strings.HasPrefix(name, "mcp__") || strings.ToLower(name) == name {
			continue
		}
		upper := 0
		for _, r := range name {
			if r >= 'A' && r <= 'Z' {
				upper++
			}
		}
		if upper >= 2 && strings.Contains(text, name) {
			return name
		}
	}
	return ""
}

func (a *Agent) options() map[string]any {
	opts := map[string]any{}
	if a.NumCtx > 0 {
		opts["num_ctx"] = a.NumCtx
	}
	if a.Temperature != nil {
		opts["temperature"] = *a.Temperature
	}
	if a.MaxTokens > 0 {
		opts["num_predict"] = a.MaxTokens
	}
	return opts
}

func (a *Agent) complete(ctx context.Context, specs []llm.ToolSpec, stream bool) (llm.Response, error) {
	msgs := []llm.Message{{Role: llm.RoleSystem, Content: a.SystemPrompt()}}
	msgs = append(msgs, a.History()...)
	provider := a.CurrentProvider()
	if provider == nil {
		return llm.Response{}, errors.New("no model server is configured")
	}
	resp, err := provider.Chat(ctx, llm.Request{
		Model:    a.CurrentModel(),
		Messages: msgs,
		Tools:    specs,
		Think:    a.thinkOption(),
		Options:  a.options(),
	}, func(d llm.Delta) {
		if !stream {
			return
		}
		if d.Content != "" && a.captureHidden() {
			return // e.g. quiz text with answers: never shown while streaming
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
		if res.IsError {
			log.Printf("agent%s: tool %s failed: %s (input: %s)", a.logName(), call.Name, res.Output, truncateLog(string(input)))
		}
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
		log.Printf("agent%s: %s", a.logName(), e)
		a.emit(Notice{Level: NoticeWarn, Text: e})
	}
}

type capture struct {
	tools.Capture
	id string
}

func (a *Agent) captureReply(tc tools.Capture) {
	a.mu.Lock()
	a.captures++
	c := &capture{Capture: tc, id: fmt.Sprintf("capture_%d", a.captures)}
	a.capture = c
	a.mu.Unlock()
	a.emit(ToolStart{Agent: a.name, ID: c.id, Name: tc.Name, Subject: "writing…"})
}

// completeText runs a one-off, tool-less completion for tools.Env.Complete.
func (a *Agent) completeText(ctx context.Context, system, prompt string) (string, error) {
	p := a.CurrentProvider()
	if p == nil {
		return "", errors.New("no model server is configured")
	}
	resp, err := p.Chat(ctx, llm.Request{
		Model: a.CurrentModel(),
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: system},
			{Role: llm.RoleUser, Content: prompt},
		},
		Think:   a.thinkOption(),
		Options: a.options(),
	}, nil)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.usage.Add(resp.Usage)
	a.mu.Unlock()
	return resp.Message.Content, nil
}

func (a *Agent) captureHidden() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.capture != nil && a.capture.Hidden
}

func (a *Agent) captureArmed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.capture != nil
}

// takeCapture runs a pending capture on text. It reports whether a capture
// consumed the reply.
func (a *Agent) takeCapture(ctx context.Context, text string) bool {
	a.mu.Lock()
	c := a.capture
	a.mu.Unlock()
	if c == nil || strings.TrimSpace(text) == "" {
		return false
	}
	res := c.Handle(ctx, text)
	if res.IsError {
		log.Printf("agent%s: captured reply for %s rejected: %s", a.logName(), c.Name, res.Output)
	}
	if !res.IsError {
		a.mu.Lock()
		a.capture = nil
		a.mu.Unlock()
	}
	a.emit(ToolEnd{Agent: a.name, ID: c.id, Name: c.Name, Result: res})
	a.appendHarness("[" + c.Name + ": " + res.Output + "]")
	return true
}

func (a *Agent) interact(ctx context.Context, payload any) (any, error) {
	req := InteractionRequest{Agent: a.name, Payload: payload, Reply: make(chan any, 1)}
	a.emit(req)
	select {
	case v := <-req.Reply:
		return v, nil
	case <-ctx.Done():
		return nil, ctx.Err()
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
