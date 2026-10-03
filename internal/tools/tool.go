// Package tools contains the tools the model can call. Each tool is a small
// type implementing Tool; register new ones in the Registry (see
// DefaultRegistry) or add them at runtime, as the MCP adapter does.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/eodomo/nextpage/internal/ext"
	"github.com/eodomo/nextpage/internal/llm"
)

// Kind classifies a tool for the permission system.
type Kind int

const (
	KindRead     Kind = iota // never needs approval
	KindEdit                 // file modifications; auto-approved in acceptEdits mode
	KindExecute              // runs commands; always asks unless allowed by rule
	KindNetwork              // talks to the network
	KindInternal             // harness bookkeeping (todos, questions, plan mode)
)

type Tool interface {
	Name() string
	Description() string
	Schema() json.RawMessage
	Kind() Kind
	Run(ctx context.Context, env *Env, input json.RawMessage) Result
}

// Subjecter is implemented by tools whose calls have a natural subject (a
// command, a path, a URL). It is shown in the UI and matched by permission
// rules such as Bash(git status:*) or Edit(src/**).
type Subjecter interface {
	Subject(input json.RawMessage) string
}

type Result struct {
	Output  string // sent back to the model
	IsError bool
	// Display, when set, replaces Output in the UI (e.g. a diff for edits).
	Display string
}

func Errorf(format string, args ...any) Result {
	return Result{Output: fmt.Sprintf(format, args...), IsError: true}
}

func Subject(t Tool, input json.RawMessage) string {
	if s, ok := t.(Subjecter); ok {
		return s.Subject(input)
	}
	return ""
}

type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
	order []string
}

func NewRegistry(ts ...Tool) *Registry {
	r := &Registry{tools: map[string]Tool{}}
	for _, t := range ts {
		r.Add(t)
	}
	return r
}

func (r *Registry) Add(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[t.Name()]; !ok {
		r.order = append(r.order, t.Name())
	}
	r.tools[t.Name()] = t
}

func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

func (r *Registry) All() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.tools[n])
	}
	return out
}

// Filter returns a new registry with the tools for which keep returns true.
func (r *Registry) Filter(keep func(Tool) bool) *Registry {
	out := NewRegistry()
	for _, t := range r.All() {
		if keep(t) {
			out.Add(t)
		}
	}
	return out
}

func (r *Registry) Specs() []llm.ToolSpec {
	var specs []llm.ToolSpec
	for _, t := range r.All() {
		specs = append(specs, llm.ToolSpec{Name: t.Name(), Description: t.Description(), Schema: t.Schema()})
	}
	return specs
}

// DefaultRegistry returns all built-in tools.
func DefaultRegistry() *Registry {
	return NewRegistry(
		ReadTool{}, WriteTool{}, EditTool{}, BashTool{}, BashOutputTool{}, KillShellTool{},
		GlobTool{}, GrepTool{}, WebFetchTool{}, TodoWriteTool{}, TaskTool{}, SkillTool{},
		AskUserTool{}, ExitPlanModeTool{},
	)
}

type Todo struct {
	Content    string `json:"content"`
	Status     string `json:"status"` // pending | in_progress | completed
	ActiveForm string `json:"activeForm,omitempty"`
}

type Question struct {
	Question string
	Options  []string
}

// Env is the state tools share during a session. Callback fields are wired
// up by the agent so tools can reach the UI without importing it.
type Env struct {
	mu          sync.Mutex
	cwd         string
	ProjectRoot string
	readFiles   map[string]time.Time
	todos       []Todo
	Shells      *ShellManager
	Skills      []ext.Skill
	Agents      []ext.AgentDef

	// Spawn runs a sub-agent and returns its final answer.
	Spawn func(ctx context.Context, agentType, description, prompt string) (string, error)
	// Ask poses a question to the user and returns the answer.
	Ask func(ctx context.Context, q Question) (string, error)
	// ExitPlan presents a plan and reports whether the user approved it.
	ExitPlan func(ctx context.Context, plan string) (bool, error)
	// OnTodos is notified whenever the todo list changes.
	OnTodos func([]Todo)
}

func NewEnv(cwd, projectRoot string) *Env {
	return &Env{
		cwd:         cwd,
		ProjectRoot: projectRoot,
		readFiles:   map[string]time.Time{},
		Shells:      NewShellManager(),
	}
}

func (e *Env) Cwd() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cwd
}

func (e *Env) SetCwd(dir string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cwd = dir
}

// Abs resolves p relative to the current working directory.
func (e *Env) Abs(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(e.Cwd(), p)
	}
	return filepath.Clean(p)
}

// MarkRead records that the model has seen the current contents of path,
// which Write and Edit require before modifying an existing file.
func (e *Env) MarkRead(path string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if fi, err := os.Stat(path); err == nil {
		e.readFiles[path] = fi.ModTime()
	}
}

// CheckFresh returns an error if path exists but hasn't been read, or has
// changed on disk since it was read.
func (e *Env) CheckFresh(path string) error {
	fi, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	e.mu.Lock()
	seen, ok := e.readFiles[path]
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("file has not been read yet; read it first before writing to it")
	}
	if fi.ModTime().After(seen) {
		return fmt.Errorf("file has been modified since it was last read; read it again before writing to it")
	}
	return nil
}

func (e *Env) Todos() []Todo {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Todo(nil), e.todos...)
}

func (e *Env) SetTodos(t []Todo) {
	e.mu.Lock()
	e.todos = t
	cb := e.OnTodos
	e.mu.Unlock()
	if cb != nil {
		cb(t)
	}
}

func decode[T any](input json.RawMessage) (T, error) {
	var v T
	if len(input) == 0 {
		input = []byte("{}")
	}
	if err := json.Unmarshal(input, &v); err != nil {
		return v, fmt.Errorf("invalid tool input: %w", err)
	}
	return v, nil
}

const maxOutput = 30000

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("\n... [truncated %d characters]", len(s)-n)
}

// sortByMtime sorts paths newest-first.
func sortByMtime(paths []string) {
	mt := make(map[string]time.Time, len(paths))
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil {
			mt[p] = fi.ModTime()
		}
	}
	sort.SliceStable(paths, func(i, j int) bool { return mt[paths[i]].After(mt[paths[j]]) })
}
