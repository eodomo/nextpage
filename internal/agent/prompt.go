package agent

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/eodomo/nextpage/internal/ext"
	"github.com/eodomo/nextpage/internal/permission"
)

// PromptContext supplies the dynamic parts of the system prompt.
type PromptContext struct {
	Cwd         func() string
	ProjectRoot string
	Model       func() string
	Mode        func() permission.Mode
	Memory      func() string
	Skills      []ext.Skill
	Agents      []ext.AgentDef
	GitStatus   string
	// Extra is appended verbatim (from --append-system-prompt).
	Extra string
}

const basePrompt = `You are nextpage, an interactive coding agent running in the user's terminal. You help with software engineering tasks: fixing bugs, adding features, refactoring, explaining code, running commands.

# Working style
- Be concise and direct. Your output is shown in a terminal; use GitHub-flavored markdown sparingly.
- When you have enough information to act, act. Don't ask for permission to do what was requested.
- Before changing code, read it and understand the surrounding conventions; mimic the existing style, naming and libraries.
- Never assume a library is available; check the project's dependency files first.
- Don't add comments unless they explain something non-obvious.
- After making changes, verify them when possible (build, run tests, lint) using the project's own commands.
- Never commit, push or perform destructive operations unless the user asks.
- Reference code as path:line so the user can navigate to it.

# Tools
- Use tools to gather information instead of guessing. Call several independent tools in one response when you can.
- Use Read, Edit, Write, Glob and Grep for files instead of shell equivalents (cat, sed, find, grep).
- Read a file before editing it. Edit needs old_string to match the file exactly.
- Use Bash for builds, tests, git and other commands. The working directory persists between calls.
- For multi-step tasks, track progress with TodoWrite and keep it updated.
- Use Task to delegate broad searches or self-contained investigations to a sub-agent.
- If the user rejects a tool call, don't retry it; adjust or ask what they want.
- Tool results may include <system-reminder> tags; they come from the harness, not the user.`

func BuildSystemPrompt(pc PromptContext) string {
	var b strings.Builder
	b.WriteString(basePrompt)

	if pc.Mode != nil && pc.Mode() == permission.ModePlan {
		b.WriteString(`

# Plan mode is active
The user wants a plan before any changes. You MUST NOT edit files, run commands with side effects, or otherwise change the system. Research with read-only tools (Read, Glob, Grep, WebFetch, Task), then call ExitPlanMode with a concise, concrete implementation plan in markdown. Wait for approval before implementing.`)
	}

	if len(pc.Skills) > 0 {
		b.WriteString("\n\n# Skills\nLoad a skill with the Skill tool before starting a task it covers:\n")
		for _, s := range pc.Skills {
			fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
		}
	}
	if len(pc.Agents) > 0 {
		b.WriteString("\n\n# Sub-agent types (Task subagent_type)\n- general-purpose: all tools, for any self-contained task\n")
		for _, a := range pc.Agents {
			fmt.Fprintf(&b, "- %s: %s\n", a.Name, a.Description)
		}
	}

	cwd := pc.Cwd()
	fmt.Fprintf(&b, `

# Environment
- Working directory: %s
- Project root: %s
- Platform: %s/%s
- Today's date: %s
- Model: %s`, cwd, pc.ProjectRoot, runtime.GOOS, runtime.GOARCH, time.Now().Format("2006-01-02"), pc.Model())
	if pc.GitStatus != "" {
		fmt.Fprintf(&b, "\n\n# Git status at session start\n%s", pc.GitStatus)
	}

	if pc.Memory != nil {
		if mem := pc.Memory(); mem != "" {
			b.WriteString("\n\n# Project and user instructions\nThe following instructions OVERRIDE default behavior and must be followed exactly.\n\n")
			b.WriteString(mem)
		}
	}
	if pc.Extra != "" {
		b.WriteString("\n\n" + pc.Extra)
	}
	return b.String()
}

// GitSummary describes the repository state for the system prompt.
func GitSummary(dir string) string {
	branch, err := exec.Command("git", "-C", dir, "branch", "--show-current").Output()
	if err != nil {
		return ""
	}
	status, _ := exec.Command("git", "-C", dir, "status", "--short").Output()
	logOut, _ := exec.Command("git", "-C", dir, "log", "--oneline", "-5").Output()
	st := strings.TrimSpace(string(status))
	if st == "" {
		st = "(clean)"
	}
	if lines := strings.Split(st, "\n"); len(lines) > 40 {
		st = strings.Join(lines[:40], "\n") + fmt.Sprintf("\n... (%d more)", len(lines)-40)
	}
	return fmt.Sprintf("Current branch: %s\nStatus:\n%s\n\nRecent commits:\n%s",
		strings.TrimSpace(string(branch)), st, strings.TrimSpace(string(logOut)))
}
