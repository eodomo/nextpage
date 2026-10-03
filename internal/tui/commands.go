package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/eodomo/nextpage/internal/agent"
	"github.com/eodomo/nextpage/internal/ext"
	"github.com/eodomo/nextpage/internal/llm"
	"github.com/eodomo/nextpage/internal/permission"
	"github.com/eodomo/nextpage/internal/session"
	"github.com/eodomo/nextpage/internal/tools"
)

type command struct {
	name    string
	aliases []string
	desc    string
	// idleOnly commands are refused while the agent is running.
	idleOnly bool
	run      func(m *Model, args string) tea.Cmd
}

const initPrompt = `Please analyze this codebase and create a NEXTPAGE.md file in the project root containing guidance for future AI coding sessions in this repository.

Include:
1. The commands used to build, lint and test (including how to run a single test).
2. High-level architecture: the big-picture structure that requires reading several files to understand.

Don't list every file or component, don't invent sections, and don't repeat generic development advice. If a NEXTPAGE.md, AGENTS.md, CLAUDE.md or README already exists, use it as input and suggest improvements rather than duplicating it. Begin the file with:

# NEXTPAGE.md

This file provides guidance to AI coding agents working with code in this repository.`

const reviewPrompt = `Review the current changes in this repository. Run git status and git diff (including staged changes) to see them. Look for bugs, regressions, missing error handling, security problems and unclear code. Report findings ranked by severity with file:line references and concrete fixes. Do not modify any files.`

func (m *Model) buildCommands() []command {
	cmds := []command{
		{name: "help", aliases: []string{"?"}, desc: "Show commands and shortcuts", run: (*Model).cmdHelp},
		{name: "clear", aliases: []string{"new", "reset"}, desc: "Start a new conversation", idleOnly: true, run: (*Model).cmdClear},
		{name: "compact", desc: "Summarize the conversation to free context [instructions]", idleOnly: true, run: (*Model).cmdCompact},
		{name: "resume", desc: "Resume a previous conversation", idleOnly: true, run: (*Model).cmdResume},
		{name: "model", desc: "Show or switch the model [name]", idleOnly: true, run: (*Model).cmdModel},
		{name: "plan", desc: "Toggle plan mode", run: (*Model).cmdPlan},
		{name: "permissions", aliases: []string{"allowed-tools"}, desc: "Show permission mode and rules", run: (*Model).cmdPermissions},
		{name: "memory", desc: "Edit project memory in $EDITOR", idleOnly: true, run: (*Model).cmdMemory},
		{name: "init", desc: "Create a NEXTPAGE.md for this project", idleOnly: true, run: func(m *Model, _ string) tea.Cmd { return m.sendPrompt("/init", initPrompt) }},
		{name: "review", desc: "Review the uncommitted changes", idleOnly: true, run: func(m *Model, args string) tea.Cmd {
			return m.sendPrompt("/review "+args, ext.ExpandArgs(reviewPrompt, args))
		}},
		{name: "context", desc: "Show context window usage", run: (*Model).cmdContext},
		{name: "cost", aliases: []string{"usage"}, desc: "Show token usage for this session", run: (*Model).cmdCost},
		{name: "status", desc: "Show session status", run: (*Model).cmdStatus},
		{name: "tools", desc: "List available tools", run: (*Model).cmdTools},
		{name: "mcp", desc: "Show MCP servers", run: (*Model).cmdMCP},
		{name: "agents", desc: "List sub-agent types", run: (*Model).cmdAgents},
		{name: "skills", desc: "List skills", run: (*Model).cmdSkills},
		{name: "hooks", desc: "Show configured hooks", run: (*Model).cmdHooks},
		{name: "todos", desc: "Show the todo list", run: (*Model).cmdTodos},
		{name: "bashes", desc: "List background shells", run: (*Model).cmdBashes},
		{name: "think", desc: "Toggle extended thinking", idleOnly: true, run: (*Model).cmdThink},
		{name: "mouse", desc: "Toggle mouse capture (wheel scrolling vs. native text selection)", run: (*Model).cmdMouse},
		{name: "export", desc: "Export the conversation to a markdown file [path]", run: (*Model).cmdExport},
		{name: "exit", aliases: []string{"quit"}, desc: "Exit nextpage", run: func(m *Model, _ string) tea.Cmd { return m.quit() }},
	}
	builtin := map[string]bool{}
	for _, c := range cmds {
		builtin[c.name] = true
	}
	for _, c := range m.d.Commands {
		if builtin[c.Name] {
			continue
		}
		cc := c
		desc := cc.Description
		if cc.ArgumentHint != "" {
			desc += " " + cc.ArgumentHint
		}
		cmds = append(cmds, command{name: cc.Name, desc: desc + " (custom)", idleOnly: true, run: func(m *Model, args string) tea.Cmd {
			return m.runCustomCommand(cc, args)
		}})
	}
	for _, s := range m.d.Skills {
		if builtin[s.Name] {
			continue
		}
		sk := s
		cmds = append(cmds, command{name: sk.Name, desc: sk.Description + " (skill)", idleOnly: true, run: func(m *Model, args string) tea.Cmd {
			prompt := fmt.Sprintf("Load the %q skill with the Skill tool and follow its instructions.", sk.Name)
			if args != "" {
				prompt += "\n\nArguments: " + args
			}
			return m.sendPrompt("/"+sk.Name+" "+args, prompt)
		}})
	}
	return cmds
}

func (m *Model) runCommand(line string) tea.Cmd {
	name, args, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
	args = strings.TrimSpace(args)
	for _, c := range m.commands {
		if c.name == name || contains(c.aliases, name) {
			if c.idleOnly && m.running {
				m.notice(kindWarn, fmt.Sprintf("/%s is unavailable while a response is running (esc to interrupt).", c.name))
				return nil
			}
			return c.run(m, args)
		}
	}
	m.notice(kindError, fmt.Sprintf("Unknown command /%s. Type /help for a list.", name))
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

var bangRe = regexp.MustCompile("!`([^`]+)`")

func (m *Model) runCustomCommand(c ext.Command, args string) tea.Cmd {
	body := ext.ExpandArgs(c.Body, args)
	// !`cmd` runs a shell command and inlines its output.
	body = bangRe.ReplaceAllStringFunc(body, func(s string) string {
		cmd := bangRe.FindStringSubmatch(s)[1]
		out, _, _, err := tools.RunShell(context.Background(), m.d.Env.Cwd(), cmd, time.Minute)
		if err != nil {
			out += "\n" + err.Error()
		}
		return out
	})
	if c.Model != "" {
		prev := m.d.Agent.Model
		m.d.Agent.SetModel(c.Model)
		cmd := m.sendPrompt("/"+c.Name+" "+args, body)
		m.runKind = "custom:" + prev
		return cmd
	}
	return m.sendPrompt("/"+c.Name+" "+args, body)
}

func (m *Model) cmdHelp(string) tea.Cmd {
	var b strings.Builder
	b.WriteString("Commands\n")
	for _, c := range m.commands {
		name := "/" + c.name
		if len(c.aliases) > 0 {
			name += " (" + strings.Join(c.aliases, ", ") + ")"
		}
		fmt.Fprintf(&b, "  %-26s %s\n", name, c.desc)
	}
	b.WriteString(`
Input
  !command          run a shell command; output is added to the conversation
  #note             save a note to the project memory file
  @path             attach a file or directory (tab completes)
  shift+enter, ctrl+j, or \ then enter   new line

Shortcuts
  enter             send (while running: queue the message)
  esc               interrupt the running response
  shift+tab         cycle permission mode (default → accept edits → plan)
  ctrl+o            toggle verbose output (full tool results, thinking)
  ctrl+t            toggle the todo list
  up/down           input history
  mouse wheel, pgup/pgdown, shift+up/down   scroll the transcript
  ctrl+home/ctrl+end   jump to top/bottom of the transcript
  ctrl+c twice, ctrl+d   exit`)
	m.notice(kindNotice, b.String())
	return nil
}

func (m *Model) cmdClear(string) tea.Cmd {
	m.d.Agent.SetHistory(nil)
	m.d.Env.SetTodos(nil)
	m.entries = nil
	m.todos = nil
	m.toolIdx = map[string]*entry{}
	m.usage = llm.Usage{}
	m.ctxTokens = 0
	m.newSession()
	m.refresh(true)
	return nil
}

func (m *Model) cmdCompact(args string) tea.Cmd {
	a := m.d.Agent
	m.notice(kindNotice, "Compacting conversation…")
	return m.startRun("compact", func(ctx context.Context, emit func(agent.Event)) error {
		return a.Compact(ctx, args, emit)
	})
}

func (m *Model) cmdResume(args string) tea.Cmd {
	sessions := session.List(m.d.SessionDir)
	var current string
	if m.d.Agent.Session != nil {
		current = m.d.Agent.Session.ID
	}
	var list []session.Info
	for _, s := range sessions {
		if s.ID != current {
			list = append(list, s)
		}
	}
	if len(list) == 0 {
		m.notice(kindNotice, "No previous conversations in this project.")
		return nil
	}
	if len(list) > 20 {
		list = list[:20]
	}
	var opts []string
	for _, s := range list {
		opts = append(opts, fmt.Sprintf("%s  %s  (%d msgs)", s.Modified.Format("Jan 02 15:04"), s.Summary, s.Messages))
	}
	m.openPicker("Resume a conversation", opts, func(i int) tea.Cmd {
		info := list[i]
		msgs, err := session.Load(info.Path)
		if err != nil {
			m.notice(kindError, err.Error())
			return nil
		}
		if m.d.Agent.Session != nil {
			m.d.Agent.Session.Close()
		}
		s, err := session.Open(m.d.SessionDir, info.ID)
		if err != nil {
			m.notice(kindError, err.Error())
			return nil
		}
		m.d.Agent.Session = s
		m.d.Agent.SetHistory(msgs)
		m.entries = nil
		m.replayHistory(msgs)
		m.notice(kindNotice, "Resumed conversation "+info.ID)
		m.refresh(true)
		return nil
	})
	return nil
}

func (m *Model) cmdModel(args string) tea.Cmd {
	if args != "" {
		m.d.Agent.SetModel(args)
		m.notice(kindNotice, "Model set to "+args)
		return nil
	}
	p := m.d.Agent.Provider
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		names, err := p.ListModels(ctx)
		return modelsMsg{names, err}
	}
}

func (m *Model) openModelPicker(names []string) {
	if len(names) == 0 {
		m.notice(kindNotice, "The server reports no models.")
		return
	}
	opts := make([]string, len(names))
	cur := 0
	for i, n := range names {
		opts[i] = n
		if n == m.d.Agent.Model {
			opts[i] += " (current)"
			cur = i
		}
	}
	m.openPicker("Select a model", opts, func(i int) tea.Cmd {
		m.d.Agent.SetModel(names[i])
		m.notice(kindNotice, "Model set to "+names[i])
		return nil
	})
	m.dialog.cursor = cur
}

func (m *Model) cmdPlan(string) tea.Cmd {
	if m.d.Perms.Mode() == permission.ModePlan {
		m.d.Perms.SetMode(permission.ModeDefault)
	} else {
		m.d.Perms.SetMode(permission.ModePlan)
	}
	m.notice(kindNotice, "Permission mode: "+m.d.Perms.Mode().String())
	return nil
}

func (m *Model) cmdPermissions(string) tea.Cmd {
	allow, deny, ask := m.d.Perms.Rules()
	var b strings.Builder
	fmt.Fprintf(&b, "Mode: %s\n", m.d.Perms.Mode())
	section := func(title string, rules []string) {
		fmt.Fprintf(&b, "\n%s:\n", title)
		if len(rules) == 0 {
			b.WriteString("  (none)\n")
		}
		for _, r := range rules {
			fmt.Fprintf(&b, "  %s\n", r)
		}
	}
	section("Allow", allow)
	section("Ask", ask)
	section("Deny", deny)
	fmt.Fprintf(&b, "\nEdit rules in %s, %s or %s.", m.d.Paths.User, m.d.Paths.Project, m.d.Paths.Local)
	m.notice(kindNotice, b.String())
	return nil
}

func (m *Model) cmdMemory(string) tea.Cmd {
	var b strings.Builder
	b.WriteString("Loaded memory files:\n")
	files := m.d.Memory()
	if len(files) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, f := range files {
		fmt.Fprintf(&b, "  %s\n", f.Path)
	}
	path := m.projectMemoryPath()
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	fmt.Fprintf(&b, "Opening %s in %s…", path, editor)
	m.notice(kindNotice, b.String())
	parts := strings.Fields(editor)
	c := exec.Command(parts[0], append(parts[1:], path)...)
	return tea.ExecProcess(c, func(err error) tea.Msg { return editorDoneMsg{err} })
}

func (m *Model) cmdContext(string) tea.Cmd {
	h := m.d.Agent.History()
	chars := 0
	counts := map[llm.Role]int{}
	for _, msg := range h {
		chars += len(msg.Content) + len(msg.Thinking)
		for _, tc := range msg.ToolCalls {
			chars += len(tc.Args)
		}
		counts[msg.Role]++
	}
	sys := len(m.d.Agent.SystemPrompt())
	var b strings.Builder
	n := m.d.Agent.NumCtx
	if m.ctxTokens > 0 && n > 0 {
		fmt.Fprintf(&b, "Context: %d / %d tokens (%d%%)\n", m.ctxTokens, n, m.ctxTokens*100/n)
		filled := min(m.ctxTokens*40/n, 40)
		b.WriteString("[" + strings.Repeat("█", filled) + strings.Repeat("░", 40-filled) + "]\n")
	} else {
		fmt.Fprintf(&b, "Context window: %d tokens (no usage reported yet)\n", n)
	}
	fmt.Fprintf(&b, "System prompt: ~%d tokens\n", sys/4)
	fmt.Fprintf(&b, "Messages: %d user, %d assistant, %d tool results (~%d tokens)\n", counts[llm.RoleUser], counts[llm.RoleAssistant], counts[llm.RoleTool], chars/4)
	fmt.Fprintf(&b, "Tools: %d\nUse /compact to summarize and free space.", len(m.d.Agent.Tools.All()))
	m.notice(kindNotice, b.String())
	return nil
}

func (m *Model) cmdCost(string) tea.Cmd {
	u, _ := m.d.Agent.Usage()
	m.notice(kindNotice, fmt.Sprintf("Tokens this session: %d input, %d output (%d total).", u.PromptTokens, u.CompletionTokens, u.PromptTokens+u.CompletionTokens))
	return nil
}

func (m *Model) cmdStatus(string) tea.Cmd {
	var b strings.Builder
	sid := "(none)"
	if m.d.Agent.Session != nil {
		sid = m.d.Agent.Session.ID + "\n  " + m.d.Agent.Session.Path
	}
	fmt.Fprintf(&b, "Model: %s (%s @ %s)\n", m.d.Agent.Model, m.d.Agent.Provider.Name(), m.d.Settings.Server)
	fmt.Fprintf(&b, "Context window: %d · thinking: %v · auto-compact: %v\n", m.d.Agent.NumCtx, m.d.Agent.Think, m.d.Agent.AutoCompact)
	fmt.Fprintf(&b, "Working directory: %s\nProject root: %s\n", m.d.Env.Cwd(), m.d.Paths.ProjectRoot)
	fmt.Fprintf(&b, "Permission mode: %s\n", m.d.Perms.Mode())
	fmt.Fprintf(&b, "Session: %s\n", sid)
	b.WriteString("Settings files:\n")
	for _, p := range []string{m.d.Paths.User, m.d.Paths.Project, m.d.Paths.Local, m.d.Paths.MCP} {
		state := "missing"
		if _, err := os.Stat(p); err == nil {
			state = "loaded"
		}
		fmt.Fprintf(&b, "  %s (%s)\n", p, state)
	}
	fmt.Fprintf(&b, "Memory files: %d · skills: %d · agents: %d · custom commands: %d · MCP servers: %d",
		len(m.d.Memory()), len(m.d.Skills), len(m.d.Agents), len(m.d.Commands), len(m.d.Settings.MCPServers))
	m.notice(kindNotice, b.String())
	return nil
}

func (m *Model) cmdTools(string) tea.Cmd {
	kinds := map[tools.Kind]string{tools.KindRead: "read", tools.KindEdit: "edit", tools.KindExecute: "execute", tools.KindNetwork: "network", tools.KindInternal: "internal"}
	var b strings.Builder
	b.WriteString("Tools:\n")
	for _, t := range m.d.Agent.Tools.All() {
		desc := strings.SplitN(t.Description(), "\n", 2)[0]
		fmt.Fprintf(&b, "  %-28s [%s] %s\n", t.Name(), kinds[t.Kind()], oneLine(desc, 70))
	}
	m.notice(kindNotice, strings.TrimRight(b.String(), "\n"))
	return nil
}

func (m *Model) cmdMCP(string) tea.Cmd {
	if len(m.d.Settings.MCPServers) == 0 {
		m.notice(kindNotice, fmt.Sprintf("No MCP servers configured. Add them under \"mcpServers\" in %s or settings.json.", m.d.Paths.MCP))
		return nil
	}
	var names []string
	for n := range m.d.Settings.MCPServers {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("MCP servers:\n")
	for _, n := range names {
		if err := m.d.MCP.Errors[n]; err != nil {
			fmt.Fprintf(&b, "  ✗ %s: %v\n", n, err)
			continue
		}
		fmt.Fprintf(&b, "  ✓ %s (%d tools)\n", n, len(m.d.MCP.Tools[n]))
		for _, t := range m.d.MCP.Tools[n] {
			fmt.Fprintf(&b, "      %s\n", t)
		}
	}
	m.notice(kindNotice, strings.TrimRight(b.String(), "\n"))
	return nil
}

func (m *Model) cmdAgents(string) tea.Cmd {
	var b strings.Builder
	b.WriteString("Sub-agent types:\n  general-purpose: all tools (built in)\n")
	for _, a := range m.d.Agents {
		tl := "all tools"
		if len(a.Tools) > 0 {
			tl = strings.Join(a.Tools, ", ")
		}
		fmt.Fprintf(&b, "  %s: %s [%s] (%s)\n", a.Name, a.Description, tl, a.Path)
	}
	fmt.Fprintf(&b, "Define more in %s or %s.", filepath.Join(m.d.Paths.UserDir, "agents"), filepath.Join(m.d.Paths.ProjectRoot, ".nextpage", "agents"))
	m.notice(kindNotice, b.String())
	return nil
}

func (m *Model) cmdSkills(string) tea.Cmd {
	if len(m.d.Skills) == 0 {
		m.notice(kindNotice, fmt.Sprintf("No skills found. Add <name>/SKILL.md under %s or %s.", filepath.Join(m.d.Paths.UserDir, "skills"), filepath.Join(m.d.Paths.ProjectRoot, ".nextpage", "skills")))
		return nil
	}
	var b strings.Builder
	b.WriteString("Skills:\n")
	for _, s := range m.d.Skills {
		fmt.Fprintf(&b, "  %s: %s\n", s.Name, s.Description)
	}
	m.notice(kindNotice, strings.TrimRight(b.String(), "\n"))
	return nil
}

func (m *Model) cmdHooks(string) tea.Cmd {
	hooks := m.d.Hooks.Configured()
	if len(hooks) == 0 {
		m.notice(kindNotice, "No hooks configured. Add them under \"hooks\" in settings.json.")
		return nil
	}
	var events []string
	for e := range hooks {
		events = append(events, e)
	}
	sort.Strings(events)
	var b strings.Builder
	for _, e := range events {
		fmt.Fprintf(&b, "%s:\n", e)
		for _, mt := range hooks[e] {
			matcher := mt.Matcher
			if matcher == "" {
				matcher = "*"
			}
			for _, h := range mt.Hooks {
				fmt.Fprintf(&b, "  [%s] %s\n", matcher, h.Command)
			}
		}
	}
	m.notice(kindNotice, strings.TrimRight(b.String(), "\n"))
	return nil
}

func (m *Model) cmdTodos(string) tea.Cmd {
	if len(m.todos) == 0 {
		m.notice(kindNotice, "No todos.")
		return nil
	}
	m.notice(kindNotice, m.renderTodos())
	return nil
}

func (m *Model) cmdBashes(string) tea.Cmd {
	list := m.d.Env.Shells.List()
	if len(list) == 0 {
		m.notice(kindNotice, "No background shells.")
		return nil
	}
	m.notice(kindNotice, "Background shells:\n  "+strings.Join(list, "\n  "))
	return nil
}

func (m *Model) cmdThink(string) tea.Cmd {
	m.d.Agent.Think = !m.d.Agent.Think
	m.notice(kindNotice, fmt.Sprintf("Extended thinking: %v", m.d.Agent.Think))
	return nil
}

func (m *Model) cmdExport(args string) tea.Cmd {
	path := args
	if path == "" {
		path = fmt.Sprintf("nextpage-%s.md", time.Now().Format("20060102-150405"))
	}
	path = m.d.Env.Abs(path)
	var b strings.Builder
	for _, msg := range m.d.Agent.History() {
		switch msg.Role {
		case llm.RoleUser:
			fmt.Fprintf(&b, "## User\n\n%s\n\n", msg.Content)
		case llm.RoleAssistant:
			if msg.Content != "" {
				fmt.Fprintf(&b, "## Assistant\n\n%s\n\n", msg.Content)
			}
			for _, tc := range msg.ToolCalls {
				fmt.Fprintf(&b, "**Tool call** `%s` `%s`\n\n", tc.Name, string(tc.Args))
			}
		case llm.RoleTool:
			fmt.Fprintf(&b, "**Result** (%s)\n\n```\n%s\n```\n\n", msg.ToolName, msg.Content)
		}
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		m.notice(kindError, err.Error())
		return nil
	}
	m.notice(kindNotice, "Exported conversation to "+path)
	return nil
}

func (m *Model) cmdMouse(string) tea.Cmd {
	m.mouse = !m.mouse
	if m.mouse {
		m.notice(kindNotice, "Mouse capture on: the wheel scrolls the transcript. Hold shift to select text.")
	} else {
		m.notice(kindNotice, "Mouse capture off: native text selection; scroll with pgup/pgdown or shift+up/down.")
	}
	return nil
}
