package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type BashTool struct{}

func (BashTool) Name() string { return "Bash" }
func (BashTool) Kind() Kind   { return KindExecute }
func (BashTool) Description() string {
	return `Executes a bash command and returns its combined stdout/stderr.
- The working directory persists between calls (cd carries over); environment variables and shell state do not.
- Default timeout is 120000ms, maximum 600000ms.
- Prefer the dedicated tools over shell equivalents: Read instead of cat/head/tail, Edit instead of sed, Glob instead of find, Grep instead of grep/rg.
- Quote paths containing spaces.
- Set run_in_background for long-running processes (servers, watchers); read their output later with BashOutput and stop them with KillShell.
- Output longer than 30000 characters is truncated.`
}
func (BashTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"command":{"type":"string","description":"The command to execute"},
"description":{"type":"string","description":"Short (5-10 word) description of what the command does"},
"timeout":{"type":"integer","description":"Timeout in milliseconds (max 600000)"},
"run_in_background":{"type":"boolean","description":"Run without waiting for completion"}},
"required":["command"]}`)
}
func (BashTool) Subject(in json.RawMessage) string {
	a, _ := decode[struct{ Command string }](in)
	return a.Command
}

func (BashTool) Run(ctx context.Context, env *Env, in json.RawMessage) Result {
	a, err := decode[struct {
		Command         string `json:"command"`
		Timeout         int    `json:"timeout"`
		RunInBackground bool   `json:"run_in_background"`
	}](in)
	if err != nil {
		return Errorf("%v", err)
	}
	if strings.TrimSpace(a.Command) == "" {
		return Errorf("command is empty")
	}
	if a.RunInBackground {
		id, err := env.Shells.Start(env.Cwd(), a.Command)
		if err != nil {
			return Errorf("%v", err)
		}
		return Result{Output: fmt.Sprintf("Started background shell %s. Use BashOutput with shell_id=%q to read its output.", id, id)}
	}

	timeout := 120 * time.Second
	if a.Timeout > 0 {
		timeout = min(time.Duration(a.Timeout)*time.Millisecond, 10*time.Minute)
	}
	out, code, newCwd, err := RunShell(ctx, env.Cwd(), a.Command, timeout)
	if newCwd != "" {
		env.SetCwd(newCwd)
	}
	out = truncate(out, maxOutput)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return Result{Output: out + fmt.Sprintf("\n[command timed out after %s]", timeout), IsError: true}
	case errors.Is(err, context.Canceled):
		return Result{Output: out + "\n[command interrupted]", IsError: true}
	case err != nil:
		return Errorf("%s\n%v", out, err)
	case code != 0:
		return Result{Output: fmt.Sprintf("%s\n[exit code %d]", out, code), IsError: true}
	}
	if out == "" {
		out = "(no output)"
	}
	return Result{Output: out}
}

// RunShell runs command with bash in dir and returns combined output, the
// exit code and the working directory the command ended in.
func RunShell(ctx context.Context, dir, command string, timeout time.Duration) (string, int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cwdFile, err := os.CreateTemp("", "nextpage-cwd-*")
	if err != nil {
		return "", -1, "", err
	}
	cwdFile.Close()
	defer os.Remove(cwdFile.Name())

	script := command + "\n__np_ec=$?\npwd -P > " + shellQuote(cwdFile.Name()) + "\nexit $__np_ec\n"
	cmd := exec.CommandContext(ctx, "bash", "-c", script)
	cmd.Dir = dir
	cmd.Stdin = nil
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	setProcessGroup(cmd)

	err = cmd.Run()
	newCwd := ""
	if b, rerr := os.ReadFile(cwdFile.Name()); rerr == nil {
		if p := strings.TrimSpace(string(b)); p != "" && filepath.IsAbs(p) {
			newCwd = p
		}
	}
	out := strings.TrimRight(buf.String(), "\n")
	if ctx.Err() != nil {
		return out, -1, newCwd, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out, exitErr.ExitCode(), newCwd, nil
	}
	return out, 0, newCwd, err
}

// setProcessGroup makes cancellation kill the whole process tree, not just
// bash, so pipelines and child processes don't outlive an interrupt.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ---- background shells ----

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) From(off int) (string, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.buf.String()
	return s[min(off, len(s)):], len(s)
}

type bgShell struct {
	ID      string
	Command string
	cmd     *exec.Cmd
	out     lockedBuffer
	readOff int
	done    chan struct{}
	exit    int
}

type ShellManager struct {
	mu     sync.Mutex
	next   int
	shells map[string]*bgShell
}

func NewShellManager() *ShellManager {
	return &ShellManager{shells: map[string]*bgShell{}}
}

func (m *ShellManager) Start(dir, command string) (string, error) {
	m.mu.Lock()
	m.next++
	id := fmt.Sprintf("bash_%d", m.next)
	m.mu.Unlock()

	sh := &bgShell{ID: id, Command: command, done: make(chan struct{})}
	sh.cmd = exec.Command("bash", "-c", command)
	sh.cmd.Dir = dir
	sh.cmd.Stdout = &sh.out
	sh.cmd.Stderr = &sh.out
	setProcessGroup(sh.cmd)
	if err := sh.cmd.Start(); err != nil {
		return "", err
	}
	go func() {
		err := sh.cmd.Wait()
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			sh.exit = exitErr.ExitCode()
		}
		close(sh.done)
	}()

	m.mu.Lock()
	m.shells[id] = sh
	m.mu.Unlock()
	return id, nil
}

func (m *ShellManager) get(id string) (*bgShell, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sh, ok := m.shells[id]
	if !ok {
		return nil, fmt.Errorf("no background shell with id %q", id)
	}
	return sh, nil
}

// List describes all background shells, for the /bashes command.
func (m *ShellManager) List() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for i := 1; i <= m.next; i++ {
		sh, ok := m.shells[fmt.Sprintf("bash_%d", i)]
		if !ok {
			continue
		}
		state := "running"
		select {
		case <-sh.done:
			state = fmt.Sprintf("exited %d", sh.exit)
		default:
		}
		out = append(out, fmt.Sprintf("%s [%s] %s", sh.ID, state, sh.Command))
	}
	return out
}

func (m *ShellManager) KillAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, sh := range m.shells {
		if sh.cmd.Process != nil {
			_ = syscall.Kill(-sh.cmd.Process.Pid, syscall.SIGKILL)
		}
	}
}

type BashOutputTool struct{}

func (BashOutputTool) Name() string { return "BashOutput" }
func (BashOutputTool) Kind() Kind   { return KindRead }
func (BashOutputTool) Description() string {
	return "Returns new output from a background shell started with Bash run_in_background, plus whether it is still running."
}
func (BashOutputTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"shell_id":{"type":"string"}},"required":["shell_id"]}`)
}
func (BashOutputTool) Run(ctx context.Context, env *Env, in json.RawMessage) Result {
	a, err := decode[struct {
		ShellID string `json:"shell_id"`
	}](in)
	if err != nil {
		return Errorf("%v", err)
	}
	sh, err := env.Shells.get(a.ShellID)
	if err != nil {
		return Errorf("%v", err)
	}
	out, off := sh.out.From(sh.readOff)
	sh.readOff = off
	status := "running"
	select {
	case <-sh.done:
		status = fmt.Sprintf("exited with code %d", sh.exit)
	default:
	}
	return Result{Output: fmt.Sprintf("<status>%s</status>\n%s", status, truncate(out, maxOutput))}
}

type KillShellTool struct{}

func (KillShellTool) Name() string { return "KillShell" }
func (KillShellTool) Kind() Kind   { return KindInternal }
func (KillShellTool) Description() string {
	return "Kills a background shell started with Bash run_in_background."
}
func (KillShellTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"shell_id":{"type":"string"}},"required":["shell_id"]}`)
}
func (KillShellTool) Run(ctx context.Context, env *Env, in json.RawMessage) Result {
	a, err := decode[struct {
		ShellID string `json:"shell_id"`
	}](in)
	if err != nil {
		return Errorf("%v", err)
	}
	sh, err := env.Shells.get(a.ShellID)
	if err != nil {
		return Errorf("%v", err)
	}
	if sh.cmd.Process != nil {
		_ = syscall.Kill(-sh.cmd.Process.Pid, syscall.SIGKILL)
	}
	return Result{Output: "Killed " + a.ShellID}
}
