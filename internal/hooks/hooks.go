// Package hooks runs user-configured shell commands at lifecycle events,
// with the same contract as Claude Code hooks:
//
//   - The event payload is written to the command's stdin as JSON.
//   - Exit code 0: success. Stdout may be JSON ({"decision":"block","reason":...},
//     {"hookSpecificOutput":{"permissionDecision":"allow|deny|ask",
//     "additionalContext":...}}); for UserPromptSubmit and SessionStart plain
//     stdout is added to the model's context.
//   - Exit code 2: blocking error; stderr is fed back (to the model for tool
//     events, to the user for UserPromptSubmit).
//   - Any other exit code: non-blocking error, logged.
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/eodomo/nextpage/internal/config"
)

const (
	PreToolUse       = "PreToolUse"
	PostToolUse      = "PostToolUse"
	UserPromptSubmit = "UserPromptSubmit"
	Stop             = "Stop"
	SubagentStop     = "SubagentStop"
	SessionStart     = "SessionStart"
	SessionEnd       = "SessionEnd"
	PreCompact       = "PreCompact"
	Notification     = "Notification"
)

type Input struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path,omitempty"`
	Cwd            string          `json:"cwd"`
	HookEventName  string          `json:"hook_event_name"`
	PermissionMode string          `json:"permission_mode,omitempty"`
	ToolName       string          `json:"tool_name,omitempty"`
	ToolInput      json.RawMessage `json:"tool_input,omitempty"`
	ToolResponse   any             `json:"tool_response,omitempty"`
	Prompt         string          `json:"prompt,omitempty"`
	Message        string          `json:"message,omitempty"`
	Source         string          `json:"source,omitempty"`
	Trigger        string          `json:"trigger,omitempty"`
}

// Outcome is the combined result of all hooks that ran for an event.
type Outcome struct {
	Block              bool   // exit 2 or decision=block
	Reason             string // why it was blocked
	PermissionDecision string // PreToolUse: allow | deny | ask
	Context            string // extra context for the model
	Errors             []string
}

type Runner struct {
	hooks map[string][]config.HookMatcher
}

func NewRunner(h map[string][]config.HookMatcher) *Runner {
	return &Runner{hooks: h}
}

func (r *Runner) Configured() map[string][]config.HookMatcher {
	if r == nil {
		return nil
	}
	return r.hooks
}

// Run executes every hook registered for in.HookEventName whose matcher
// matches in.ToolName (tool events) and merges their results.
func (r *Runner) Run(ctx context.Context, in Input) Outcome {
	var out Outcome
	if r == nil {
		return out
	}
	for _, m := range r.hooks[in.HookEventName] {
		if !matcherMatches(m.Matcher, in.ToolName) {
			continue
		}
		for _, h := range m.Hooks {
			if h.Type != "" && h.Type != "command" {
				continue
			}
			res := runOne(ctx, h, in)
			if res.Block {
				out.Block = true
				out.Reason = joinNonEmpty(out.Reason, res.Reason)
			}
			if res.PermissionDecision != "" {
				out.PermissionDecision = res.PermissionDecision
			}
			out.Context = joinNonEmpty(out.Context, res.Context)
			out.Errors = append(out.Errors, res.Errors...)
		}
	}
	return out
}

func matcherMatches(matcher, tool string) bool {
	if matcher == "" || matcher == "*" {
		return true
	}
	re, err := regexp.Compile("^(?:" + matcher + ")$")
	if err != nil {
		return matcher == tool
	}
	return re.MatchString(tool)
}

func runOne(ctx context.Context, h config.HookCommand, in Input) Outcome {
	timeout := 60 * time.Second
	if h.Timeout > 0 {
		timeout = time.Duration(h.Timeout) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	payload, _ := json.Marshal(in)
	cmd := exec.CommandContext(ctx, "bash", "-c", h.Command)
	cmd.Dir = in.Cwd
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr) && exitErr.ExitCode() == 2:
		return Outcome{Block: true, Reason: strings.TrimSpace(stderr.String())}
	case err != nil:
		msg := fmt.Sprintf("%s hook %q failed: %v %s", in.HookEventName, h.Command, err, strings.TrimSpace(stderr.String()))
		log.Println(msg)
		return Outcome{Errors: []string{msg}}
	}

	text := strings.TrimSpace(stdout.String())
	var parsed struct {
		Decision           string `json:"decision"`
		Reason             string `json:"reason"`
		HookSpecificOutput struct {
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
			AdditionalContext        string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if strings.HasPrefix(text, "{") && json.Unmarshal([]byte(text), &parsed) == nil {
		o := Outcome{
			Block:              parsed.Decision == "block",
			Reason:             parsed.Reason,
			PermissionDecision: parsed.HookSpecificOutput.PermissionDecision,
			Context:            parsed.HookSpecificOutput.AdditionalContext,
		}
		if o.Reason == "" {
			o.Reason = parsed.HookSpecificOutput.PermissionDecisionReason
		}
		return o
	}
	if in.HookEventName == UserPromptSubmit || in.HookEventName == SessionStart {
		return Outcome{Context: text}
	}
	return Outcome{}
}

func joinNonEmpty(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "\n" + b
}
