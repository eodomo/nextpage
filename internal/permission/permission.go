// Package permission decides whether a tool call may run, must be confirmed
// by the user, or is refused. Rules use Claude Code's syntax:
//
//	Read                    any call to the tool
//	Bash(git status)        exact command
//	Bash(git diff:*)        command prefix
//	Edit(src/**)            path glob, relative to the project root (also covers Write)
//	WebFetch(domain:go.dev) URL host
//	mcp__github             every tool from an MCP server
package permission

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	"github.com/eodomo/nextpage/internal/tools"
)

type Mode int

const (
	ModeDefault Mode = iota
	ModeAcceptEdits
	ModePlan
	ModeBypass
)

func (m Mode) String() string {
	switch m {
	case ModeAcceptEdits:
		return "acceptEdits"
	case ModePlan:
		return "plan"
	case ModeBypass:
		return "bypassPermissions"
	default:
		return "default"
	}
}

func ParseMode(s string) (Mode, error) {
	switch s {
	case "", "default":
		return ModeDefault, nil
	case "acceptEdits":
		return ModeAcceptEdits, nil
	case "plan":
		return ModePlan, nil
	case "bypassPermissions":
		return ModeBypass, nil
	}
	return ModeDefault, fmt.Errorf("unknown permission mode %q", s)
}

type Behavior int

const (
	Allow Behavior = iota
	Ask
	Deny
)

type Decision struct {
	Behavior Behavior
	Reason   string
}

type Checker struct {
	mu          sync.Mutex
	mode        Mode
	allow       []string
	deny        []string
	ask         []string
	projectRoot string
	// AllowBypass gates whether bypassPermissions can be entered by cycling modes.
	AllowBypass bool
}

func NewChecker(projectRoot string, mode Mode, allow, deny, ask []string) *Checker {
	return &Checker{projectRoot: projectRoot, mode: mode, allow: allow, deny: deny, ask: ask}
}

func (c *Checker) Mode() Mode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mode
}

func (c *Checker) SetMode(m Mode) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mode = m
}

// CycleMode advances default → acceptEdits → plan (→ bypass) → default.
func (c *Checker) CycleMode() Mode {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mode++
	if c.mode == ModeBypass && !c.AllowBypass || c.mode > ModeBypass {
		c.mode = ModeDefault
	}
	return c.mode
}

func (c *Checker) AddAllow(rule string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.allow = append(c.allow, rule)
}

func (c *Checker) Rules() (allow, deny, ask []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.allow...), append([]string(nil), c.deny...), append([]string(nil), c.ask...)
}

func (c *Checker) Check(t tools.Tool, input json.RawMessage) Decision {
	c.mu.Lock()
	mode, allow, deny, ask := c.mode, c.allow, c.deny, c.ask
	c.mu.Unlock()

	name := t.Name()
	subject := tools.Subject(t, input)
	if r := c.firstMatch(deny, name, subject); r != "" {
		return Decision{Deny, "denied by rule " + r}
	}
	if mode == ModePlan && t.Kind() != tools.KindRead && t.Kind() != tools.KindInternal {
		return Decision{Deny, "plan mode is active: only read-only tools may run until the user approves a plan via ExitPlanMode"}
	}
	if r := c.firstMatch(ask, name, subject); r != "" {
		return Decision{Ask, "rule " + r}
	}
	if mode == ModeBypass {
		return Decision{Allow, "bypass mode"}
	}
	if r := c.firstMatch(allow, name, subject); r != "" {
		return Decision{Allow, "allowed by rule " + r}
	}
	switch t.Kind() {
	case tools.KindRead, tools.KindInternal:
		return Decision{Allow, "read-only"}
	case tools.KindEdit:
		if mode == ModeAcceptEdits {
			return Decision{Allow, "acceptEdits mode"}
		}
	case tools.KindExecute:
		if name == "Bash" && isSafeCommand(subject) {
			return Decision{Allow, "read-only command"}
		}
	}
	return Decision{Ask, ""}
}

func (c *Checker) firstMatch(rules []string, name, subject string) string {
	for _, r := range rules {
		if c.matches(r, name, subject) {
			return r
		}
	}
	return ""
}

func (c *Checker) matches(rule, name, subject string) bool {
	ruleTool, pattern, hasPattern := strings.Cut(rule, "(")
	pattern = strings.TrimSuffix(pattern, ")")

	switch {
	case ruleTool == name:
	case ruleTool == "Edit" && name == "Write":
	case strings.HasPrefix(name, "mcp__") && strings.HasPrefix(name, ruleTool+"__"):
	default:
		return false
	}
	if !hasPattern || pattern == "*" {
		return true
	}
	switch name {
	case "Bash":
		if prefix, ok := strings.CutSuffix(pattern, ":*"); ok {
			return subject == prefix || strings.HasPrefix(subject, prefix+" ")
		}
		return subject == pattern
	case "WebFetch":
		if host, ok := strings.CutPrefix(pattern, "domain:"); ok {
			u, err := url.Parse(subject)
			return err == nil && (u.Hostname() == host || strings.HasSuffix(u.Hostname(), "."+host))
		}
		return subject == pattern
	case "Read", "Write", "Edit", "Glob", "Grep":
		return tools.MatchGlob(pattern, c.relPath(subject)) || tools.MatchGlob(pattern, subject)
	default:
		return subject == pattern
	}
}

func (c *Checker) relPath(p string) string {
	if filepath.IsAbs(p) {
		if rel, err := filepath.Rel(c.projectRoot, p); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(filepath.Clean(p))
}

// SuggestRule proposes an allow rule for "don't ask again" on this call.
func SuggestRule(t tools.Tool, input json.RawMessage) string {
	name := t.Name()
	subject := tools.Subject(t, input)
	switch name {
	case "Bash":
		fields := strings.Fields(subject)
		if len(fields) == 0 {
			return name
		}
		prefix := fields[0]
		if len(fields) > 1 && !strings.HasPrefix(fields[1], "-") && !strings.ContainsAny(fields[1], "/.'\"$") {
			prefix += " " + fields[1]
		}
		return fmt.Sprintf("Bash(%s:*)", prefix)
	case "Write", "Edit":
		return "Edit"
	case "WebFetch":
		if u, err := url.Parse(subject); err == nil && u.Hostname() != "" {
			return fmt.Sprintf("WebFetch(domain:%s)", u.Hostname())
		}
	}
	return name
}

var safePrefixes = []string{
	"ls", "pwd", "cat", "head", "tail", "wc", "file", "stat", "which", "echo", "date", "whoami", "uname",
	"tree", "du", "df",
	"git status", "git diff", "git log", "git show", "git branch", "git rev-parse", "git remote -v", "git blame",
	"go version", "go env", "go list", "go doc", "go vet",
	"grep", "rg", "find",
}

// isSafeCommand recognizes a small set of read-only commands that run
// without prompting. Anything with shell control operators, redirection or
// substitution is excluded so "ls; rm -rf ." doesn't slip through.
func isSafeCommand(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" || strings.ContainsAny(cmd, ";&|<>`$\n\\") {
		return false
	}
	if strings.HasPrefix(cmd, "find ") && (strings.Contains(cmd, "-exec") || strings.Contains(cmd, "-delete") || strings.Contains(cmd, "-ok")) {
		return false
	}
	for _, p := range safePrefixes {
		if cmd == p || strings.HasPrefix(cmd, p+" ") {
			return true
		}
	}
	return false
}
