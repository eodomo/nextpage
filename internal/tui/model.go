// Package tui is the interactive Bubble Tea front end for the agent.
package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"charm.land/bubbles/v2/cursor"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"

	"github.com/eodomo/nextpage/internal/agent"
	"github.com/eodomo/nextpage/internal/config"
	"github.com/eodomo/nextpage/internal/ext"
	"github.com/eodomo/nextpage/internal/hooks"
	"github.com/eodomo/nextpage/internal/learn"
	"github.com/eodomo/nextpage/internal/llm"
	"github.com/eodomo/nextpage/internal/mcp"
	"github.com/eodomo/nextpage/internal/memory"
	"github.com/eodomo/nextpage/internal/permission"
	"github.com/eodomo/nextpage/internal/session"
	"github.com/eodomo/nextpage/internal/tools"
)

// Deps is everything the UI needs from the rest of the program.
type Deps struct {
	Agent      *agent.Agent
	Perms      *permission.Checker
	Env        *tools.Env
	Hooks      *hooks.Runner
	MCP        *mcp.Manager
	Paths      config.Paths
	Settings   config.Settings
	Commands   []ext.Command
	Skills     []ext.Skill
	Agents     []ext.AgentDef
	SessionDir string
	// Memory returns the loaded memory files; ReloadMemory re-reads them.
	Memory       func() []memory.File
	ReloadMemory func()
	// InitialPrompt is submitted as soon as the UI starts.
	InitialPrompt string
	// Courses is set in the learn profile.
	Courses *learn.Manager
}

type agentEventMsg struct{ ev agent.Event }
type runDoneMsg struct{ err error }
type bashDoneMsg struct {
	command string
	output  string
	code    int
}
type editorDoneMsg struct{ err error }
type modelsMsg struct {
	names []string
	err   error
}
type initialPromptMsg struct{}

type Model struct {
	d Deps

	width, height int
	viewport      viewport.Model
	input         textarea.Model
	spinner       spinner.Model
	md            *glamour.TermRenderer
	mdWidth       int

	entries []*entry
	toolIdx map[string]*entry

	events   chan any
	cancel   context.CancelFunc
	running  bool
	runKind  string
	runStart time.Time
	verb     string
	queue    []string

	dialog    *dialog
	todos     []tools.Todo
	showTodos bool
	verbose   bool

	history     []string
	histIdx     int
	completions []completion
	compIdx     int
	lastCtrlC   time.Time

	bottom string // rendered panel below the transcript; see layout
	mouse  bool   // capture the mouse for wheel scrolling

	usage     llm.Usage
	ctxTokens int
	commands  []command
}

func New(d Deps) *Model {
	ta := textarea.New()
	ta.Placeholder = "Ask anything · / for commands · ! for shell · # to remember · @ to attach files"
	ta.SetVirtualCursor(true)
	ta.Focus()
	ta.Prompt = "> "
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	ta.DynamicHeight = true
	ta.MinHeight = 1
	ta.MaxHeight = 10
	ta.SetHeight(1)
	ta.KeyMap.InsertNewline.SetKeys("shift+enter", "ctrl+j", "alt+enter")
	st := ta.Styles()
	st.Focused.CursorLine = lipgloss.NewStyle()
	ta.SetStyles(st)

	vp := viewport.New(viewport.WithWidth(80), viewport.WithHeight(20))
	vp.KeyMap.Left.SetEnabled(false)
	vp.KeyMap.Right.SetEnabled(false)

	s := spinner.New()
	s.Spinner = randomSpinner()
	s.Style = lipgloss.NewStyle().Foreground(colorSpinner)

	m := &Model{
		d:         d,
		viewport:  vp,
		input:     ta,
		spinner:   s,
		toolIdx:   map[string]*entry{},
		showTodos: true,
		mouse:     true,
		width:     80,
		height:    24,
	}
	m.commands = m.buildCommands()
	if d.Courses != nil {
		m.input.Placeholder = learnPlaceholder
	}
	m.replayHistory(d.Agent.History())
	return m
}

func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{textarea.Blink, m.spinner.Tick}
	if m.d.InitialPrompt != "" {
		cmds = append(cmds, func() tea.Msg { return initialPromptMsg{} })
	}
	return tea.Batch(cmds...)
}

func waitForEvent(ch <-chan any) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		if done, ok := ev.(runDoneMsg); ok {
			return done
		}
		return agentEventMsg{ev.(agent.Event)}
	}
}

// startRun runs fn on a goroutine, bridging its events into the Update loop
// through a channel that waitForEvent drains one message at a time.
func (m *Model) startRun(kind string, fn func(ctx context.Context, emit func(agent.Event)) error) tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan any, 256)
	m.events = ch
	m.cancel = cancel
	m.running = true
	m.runKind = kind
	m.runStart = time.Now()
	m.verb = randomVerb()
	m.spinner.Spinner = randomSpinner()
	go func() {
		err := fn(ctx, func(e agent.Event) { ch <- e })
		ch <- runDoneMsg{err}
		close(ch)
	}()
	return waitForEvent(ch)
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.update(msg)
	m.layout()
	return m, cmd
}

func (m *Model) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.viewport.SetWidth(msg.Width)
		m.input.SetWidth(msg.Width - styleInputBox.GetHorizontalFrameSize())
		m.invalidate()
		m.refresh(true)
		return nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			m.viewport.ScrollUp(3)
		case tea.MouseWheelDown:
			m.viewport.ScrollDown(3)
		}
		return nil

	case tea.PasteMsg:
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return cmd

	case initialPromptMsg:
		return m.submit(m.d.InitialPrompt)

	case agentEventMsg:
		cmd := m.handleEvent(msg.ev)
		return tea.Batch(cmd, waitForEvent(m.events))

	case runDoneMsg:
		return m.finishRun(msg.err)

	case bashDoneMsg:
		e := &entry{kind: kindBash, text: msg.command, output: msg.output, isErr: msg.code != 0}
		m.add(e)
		ctxMsg := fmt.Sprintf("<bash-input>%s</bash-input>\n<bash-stdout>%s</bash-stdout>\n<bash-exit-code>%d</bash-exit-code>", msg.command, msg.output, msg.code)
		m.d.Agent.AddContext(ctxMsg)
		m.running = false
		return m.dequeue()

	case editorDoneMsg:
		if msg.err != nil {
			m.notice(kindError, "editor: "+msg.err.Error())
		} else {
			m.d.ReloadMemory()
			m.notice(kindNotice, "Memory reloaded.")
		}
		return nil

	case modelsMsg:
		if msg.err != nil {
			m.notice(kindError, "listing models: "+msg.err.Error())
			return nil
		}
		m.openModelPicker(msg.names)
		return nil

	case cursor.BlinkMsg:
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return cmd

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return cmd
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return cmd
}

func (m *Model) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.String()

	if key == "ctrl+c" {
		switch {
		case m.running && m.cancel != nil:
			m.cancel()
			return nil
		case m.input.Value() != "":
			m.input.Reset()
			return nil
		case time.Since(m.lastCtrlC) < 2*time.Second:
			return m.quit()
		}
		m.lastCtrlC = time.Now()
		m.notice(kindNotice, "Press ctrl+c again to exit.")
		return nil
	}
	if key == "ctrl+d" && m.input.Value() == "" {
		return m.quit()
	}

	if m.dialog != nil {
		if cmd, handled := m.dialogKey(msg); handled {
			return cmd
		}
	}

	switch key {
	case "esc":
		if len(m.completions) > 0 {
			m.completions = nil
			return nil
		}
		if m.running && m.cancel != nil {
			m.cancel()
		}
		return nil
	case "shift+tab":
		mode := m.d.Perms.CycleMode()
		m.notice(kindNotice, "Permission mode: "+mode.String())
		return nil
	case "ctrl+o":
		m.verbose = !m.verbose
		m.invalidate()
		m.refresh(false)
		return nil
	case "ctrl+t":
		m.showTodos = !m.showTodos
		return nil
	case "ctrl+l":
		m.refresh(true)
		return nil
	case "pgup":
		m.viewport.PageUp()
		return nil
	case "pgdown":
		m.viewport.PageDown()
		return nil
	case "shift+up":
		m.viewport.ScrollUp(3)
		return nil
	case "shift+down":
		m.viewport.ScrollDown(3)
		return nil
	case "ctrl+home":
		m.viewport.GotoTop()
		return nil
	case "ctrl+end":
		m.viewport.GotoBottom()
		return nil
	case "tab":
		m.complete()
		return nil
	case "up":
		if len(m.completions) > 0 {
			m.compIdx = (m.compIdx - 1 + len(m.completions)) % len(m.completions)
			return nil
		}
		if m.input.Line() == 0 && m.historyPrev() {
			return nil
		}
	case "down":
		if len(m.completions) > 0 {
			m.compIdx = (m.compIdx + 1) % len(m.completions)
			return nil
		}
		if m.input.Line() == m.input.LineCount()-1 && m.historyNext() {
			return nil
		}
	case "enter":
		if m.dialog != nil && m.dialog.textMode {
			return m.dialogSubmitText()
		}
		val := m.input.Value()
		if strings.HasSuffix(val, "\\") {
			m.input.SetValue(strings.TrimSuffix(val, "\\") + "\n")
			return nil
		}
		if len(m.completions) > 0 && strings.HasPrefix(val, "/") && !strings.Contains(val, " ") {
			c := m.completions[m.compIdx]
			if c.isCommand && "/"+c.value != val {
				val = "/" + c.value
			}
		}
		m.input.Reset()
		m.completions = nil
		return m.submit(val)
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.updateCompletions()
	return cmd
}

func (m *Model) quit() tea.Cmd {
	if m.cancel != nil {
		m.cancel()
	}
	return tea.Quit
}

// submit handles a line of user input.
func (m *Model) submit(text string) tea.Cmd {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	m.pushHistory(text)

	if strings.HasPrefix(trimmed, "/") {
		return m.runCommand(trimmed)
	}
	if m.running {
		m.queue = append(m.queue, text)
		m.notice(kindNotice, "Queued: "+oneLine(trimmed, 60))
		return nil
	}
	switch {
	case strings.HasPrefix(trimmed, "!"):
		return m.runBash(strings.TrimSpace(trimmed[1:]))
	case strings.HasPrefix(trimmed, "#"):
		m.remember(strings.TrimSpace(trimmed[1:]))
		return nil
	}
	return m.sendPrompt(text, text)
}

// sendPrompt shows display in the transcript and sends prompt (with any
// @file attachments expanded) to the agent.
func (m *Model) sendPrompt(display, prompt string) tea.Cmd {
	m.add(&entry{kind: kindUser, text: display})
	prompt = m.expandMentions(prompt)
	a := m.d.Agent
	return m.startRun("prompt", func(ctx context.Context, emit func(agent.Event)) error {
		return a.Run(ctx, prompt, emit)
	})
}

func (m *Model) finishRun(err error) tea.Cmd {
	kind := m.runKind
	m.running = false
	m.events = nil
	m.cancel = nil
	m.dialog = nil
	m.input.Placeholder = m.placeholder()
	for _, e := range m.entries {
		e.streaming = false
		if e.kind == kindTool && e.result == nil {
			e.result = &tools.Result{Output: "Interrupted", IsError: true}
		}
		e.dirty = true
	}
	if prev, ok := strings.CutPrefix(kind, "custom:"); ok {
		// A custom command temporarily switched models.
		m.d.Agent.SetModel(prev)
	}
	switch {
	case errors.Is(err, context.Canceled):
		m.notice(kindError, "Interrupted · what should nextpage do instead?")
	case errors.Is(err, agent.ErrBlocked):
		m.notice(kindError, err.Error())
	case err != nil:
		m.notice(kindError, "Error: "+err.Error())
	case kind == "compact":
		m.notice(kindNotice, "✻ Conversation compacted. The model now sees a summary of everything above.")
	}
	m.refresh(false)
	return m.dequeue()
}

func (m *Model) dequeue() tea.Cmd {
	if m.running || len(m.queue) == 0 {
		return nil
	}
	next := strings.Join(m.queue, "\n\n")
	m.queue = nil
	return m.submit(next)
}

func (m *Model) runBash(command string) tea.Cmd {
	if command == "" {
		return nil
	}
	m.running = true
	m.runStart = time.Now()
	m.verb = "Running"
	env := m.d.Env
	return func() tea.Msg {
		out, code, newCwd, err := tools.RunShell(context.Background(), env.Cwd(), command, 10*time.Minute)
		if newCwd != "" {
			env.SetCwd(newCwd)
		}
		if err != nil {
			out += "\n" + err.Error()
			code = -1
		}
		return bashDoneMsg{command: command, output: out, code: code}
	}
}

// remember appends a note to the project's memory file.
func (m *Model) remember(note string) {
	if note == "" {
		return
	}
	path := m.projectMemoryPath()
	if err := memory.Append(path, note); err != nil {
		m.notice(kindError, "saving memory: "+err.Error())
		return
	}
	m.d.ReloadMemory()
	m.notice(kindNotice, "Saved to "+path)
}

func (m *Model) projectMemoryPath() string {
	root := m.d.Paths.ProjectRoot
	for _, n := range []string{"NEXTPAGE.md", "AGENTS.md", "CLAUDE.md"} {
		p := filepath.Join(root, n)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join(root, "NEXTPAGE.md")
}

var mentionRe = regexp.MustCompile(`(^|\s)@(\S+)`)

// expandMentions attaches the contents of @path references to the prompt.
func (m *Model) expandMentions(prompt string) string {
	var attachments []string
	seen := map[string]bool{}
	for _, match := range mentionRe.FindAllStringSubmatch(prompt, -1) {
		ref := strings.TrimRight(match[2], ".,;:!?)")
		path := m.d.Env.Abs(ref)
		if seen[path] {
			continue
		}
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		seen[path] = true
		if fi.IsDir() {
			ents, _ := os.ReadDir(path)
			var names []string
			for _, e := range ents {
				n := e.Name()
				if e.IsDir() {
					n += "/"
				}
				names = append(names, n)
			}
			attachments = append(attachments, fmt.Sprintf("<directory path=%q>\n%s\n</directory>", path, strings.Join(names, "\n")))
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil || len(data) > 256*1024 {
			continue
		}
		m.d.Env.MarkRead(path)
		attachments = append(attachments, fmt.Sprintf("<file path=%q>\n%s\n</file>", path, string(data)))
	}
	if len(attachments) == 0 {
		return prompt
	}
	return prompt + "\n\n<system-reminder>The user attached these files:\n" + strings.Join(attachments, "\n") + "\n</system-reminder>"
}

func (m *Model) handleEvent(ev agent.Event) tea.Cmd {
	switch e := ev.(type) {
	case agent.TextDelta:
		if e.Agent != "" {
			return nil
		}
		last := m.last()
		if last == nil || last.kind != kindAssistant || !last.streaming {
			m.finalizeStreaming()
			last = &entry{kind: kindAssistant, streaming: true}
			m.add(last)
		}
		last.text += e.Text
		last.dirty = true
	case agent.ThinkingDelta:
		if e.Agent != "" {
			return nil
		}
		last := m.last()
		if last == nil || last.kind != kindThinking || !last.streaming {
			m.finalizeStreaming()
			last = &entry{kind: kindThinking, streaming: true}
			m.add(last)
		}
		last.text += e.Text
		last.dirty = true
	case agent.AssistantMessage:
		if e.Agent == "" {
			m.finalizeStreaming()
		}
	case agent.ToolStart:
		m.finalizeStreaming()
		t := &entry{kind: kindTool, agent: e.Agent, toolName: e.Name, subject: e.Subject}
		m.toolIdx[e.Agent+"/"+e.ID] = t
		m.add(t)
	case agent.ToolEnd:
		t := m.toolIdx[e.Agent+"/"+e.ID]
		if t == nil {
			t = &entry{kind: kindTool, agent: e.Agent, toolName: e.Name}
			m.add(t)
		}
		res := e.Result
		t.result = &res
		t.dirty = true
	case agent.PermissionRequest:
		m.openPermissionDialog(e)
	case agent.QuestionRequest:
		m.openQuestionDialog(e)
	case agent.PlanRequest:
		m.openPlanDialog(e)
	case agent.InteractionRequest:
		m.handleInteraction(e)
	case agent.Notice:
		kind := kindNotice
		if e.Level == agent.NoticeError {
			kind = kindError
		} else if e.Level == agent.NoticeWarn {
			kind = kindWarn
		}
		m.notice(kind, e.Text)
	case agent.UsageUpdate:
		m.usage = e.Total
		m.ctxTokens = e.ContextTokens
	case agent.TodosUpdate:
		m.todos = e.Todos
	}
	m.refresh(false)
	return nil
}

func (m *Model) finalizeStreaming() {
	for i := len(m.entries) - 1; i >= 0 && i >= len(m.entries)-3; i-- {
		if m.entries[i].streaming {
			m.entries[i].streaming = false
			m.entries[i].dirty = true
		}
	}
}

func (m *Model) last() *entry {
	if len(m.entries) == 0 {
		return nil
	}
	return m.entries[len(m.entries)-1]
}

func (m *Model) add(e *entry) {
	e.dirty = true
	m.entries = append(m.entries, e)
	m.refresh(false)
}

func (m *Model) notice(kind entryKind, text string) {
	m.add(&entry{kind: kind, text: text})
}

// replayHistory rebuilds transcript entries from a loaded conversation.
func (m *Model) replayHistory(h []llm.Message) {
	results := map[string]llm.Message{}
	for _, msg := range h {
		if msg.Role == llm.RoleTool {
			results[msg.ToolCallID] = msg
		}
	}
	for _, msg := range h {
		switch msg.Role {
		case llm.RoleUser:
			if msg.Harness {
				continue
			}
			text := msg.Content
			if i := strings.Index(text, "\n\n<system-reminder>"); i >= 0 {
				text = text[:i]
			}
			m.entries = append(m.entries, &entry{kind: kindUser, text: text, dirty: true})
		case llm.RoleAssistant:
			if strings.TrimSpace(msg.Content) != "" && !msg.Harness {
				m.entries = append(m.entries, &entry{kind: kindAssistant, text: msg.Content, dirty: true})
			}
			for _, tc := range msg.ToolCalls {
				e := &entry{kind: kindTool, toolName: tc.Name, dirty: true}
				if t, ok := m.d.Agent.Tools.Get(tc.Name); ok {
					e.subject = tools.Subject(t, tc.Args)
				}
				if r, ok := results[tc.ID]; ok {
					e.result = &tools.Result{Output: r.Content, IsError: strings.HasPrefix(r.Content, "Error")}
				}
				m.entries = append(m.entries, e)
			}
		}
	}
}

func (m *Model) newSession() {
	if m.d.Agent.Session != nil {
		m.d.Agent.Session.Close()
	}
	s, err := session.New(m.d.SessionDir)
	if err != nil {
		m.notice(kindError, "starting session: "+err.Error())
		m.d.Agent.Session = nil
		return
	}
	m.d.Agent.Session = s
}

// ---- history & completion ----

func (m *Model) pushHistory(s string) {
	if n := len(m.history); n == 0 || m.history[n-1] != s {
		m.history = append(m.history, s)
	}
	m.histIdx = len(m.history)
}

func (m *Model) historyPrev() bool {
	if m.histIdx == 0 || len(m.history) == 0 {
		return false
	}
	if m.input.Value() != "" && (m.histIdx >= len(m.history) || m.input.Value() != m.history[m.histIdx]) {
		return false
	}
	m.histIdx--
	m.input.SetValue(m.history[m.histIdx])
	return true
}

func (m *Model) historyNext() bool {
	if m.histIdx >= len(m.history) {
		return false
	}
	m.histIdx++
	if m.histIdx == len(m.history) {
		m.input.Reset()
	} else {
		m.input.SetValue(m.history[m.histIdx])
	}
	return true
}

type completion struct {
	value     string
	desc      string
	isCommand bool
}

func (m *Model) updateCompletions() {
	val := m.input.Value()
	m.completions = nil
	m.compIdx = 0
	if !strings.HasPrefix(val, "/") || strings.ContainsAny(val, " \n") {
		return
	}
	prefix := strings.TrimPrefix(val, "/")
	for _, c := range m.commands {
		names := append([]string{c.name}, c.aliases...)
		for _, n := range names {
			if strings.HasPrefix(n, prefix) {
				m.completions = append(m.completions, completion{value: c.name, desc: c.desc, isCommand: true})
				break
			}
		}
	}
}

// complete handles Tab: finish a slash command or an @path.
func (m *Model) complete() {
	if len(m.completions) > 0 {
		m.input.SetValue("/" + m.completions[m.compIdx].value + " ")
		m.completions = nil
		return
	}
	val := m.input.Value()
	i := strings.LastIndexAny(val, " \n")
	word := val[i+1:]
	if !strings.HasPrefix(word, "@") {
		return
	}
	partial := word[1:]
	matches, _ := filepath.Glob(m.d.Env.Abs(partial) + "*")
	if partial == "" || strings.HasSuffix(partial, "/") {
		matches, _ = filepath.Glob(filepath.Join(m.d.Env.Abs(partial), "*"))
	}
	if len(matches) == 0 {
		return
	}
	common := matches[0]
	for _, p := range matches[1:] {
		for !strings.HasPrefix(p, common) {
			common = common[:len(common)-1]
		}
	}
	rel := common
	if r, err := filepath.Rel(m.d.Env.Cwd(), common); err == nil && !strings.HasPrefix(r, "..") {
		rel = r
	}
	if len(matches) == 1 {
		if fi, err := os.Stat(common); err == nil && fi.IsDir() {
			rel += "/"
		} else {
			rel += " "
		}
	}
	m.input.SetValue(val[:i+1] + "@" + rel)
	m.input.CursorEnd()
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}
