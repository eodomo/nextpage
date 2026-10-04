package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/eodomo/nextpage/internal/permission"
	"github.com/eodomo/nextpage/internal/tools"
)

const (
	defaultPlaceholder = "Ask anything · / for commands · ! for shell · # to remember · @ to attach files"
	learnPlaceholder   = "What do you want to learn? · /courses to see your courses · / for commands"
)

func (m *Model) placeholder() string {
	if m.d.Courses != nil {
		return learnPlaceholder
	}
	return defaultPlaceholder
}

type entryKind int

const (
	kindUser entryKind = iota
	kindAssistant
	kindThinking
	kindTool
	kindBash
	kindNotice
	kindWarn
	kindError
	kindDebug // harness housekeeping, shown only in verbose mode
)

type entry struct {
	kind      entryKind
	text      string
	streaming bool

	// tool calls
	agent    string
	toolName string
	subject  string
	result   *tools.Result

	// bash mode
	output string
	isErr  bool

	dirty  bool
	cache  string
	cacheW int
}

func (m *Model) invalidate() {
	for _, e := range m.entries {
		e.dirty = true
	}
}

// refresh re-renders the transcript, following the bottom unless the user
// has scrolled up (force jumps to the bottom regardless).
func (m *Model) refresh(force bool) {
	follow := force || m.viewport.AtBottom()
	w := max(m.width, 20)
	var parts []string
	for _, e := range m.entries {
		if e.dirty || e.cacheW != w {
			e.cache = clipWidth(m.render(e, w), w)
			e.cacheW = w
			e.dirty = false
		}
		if e.cache != "" {
			parts = append(parts, e.cache)
		}
	}
	content := strings.Join(parts, "\n\n")
	if len(m.entries) == 0 {
		content = m.welcome()
	}
	m.viewport.SetContent(content)
	if follow {
		m.viewport.GotoBottom()
	}
}

func (m *Model) welcome() string {
	box := lipgloss.NewStyle().BorderStyle(lipgloss.RoundedBorder()).BorderForeground(colorAccent).Padding(0, 1)
	if m.d.Courses != nil {
		dir := m.d.Courses.Dir()
		if dir == "" {
			dir = "(chosen when you start your first course)"
		}
		return box.Render(fmt.Sprintf("%s nextpage · your AI tutor\n\n%s\n%s\n%s",
			styleSelected.Render("✻"),
			"Tell me a topic you want to learn. I'll quiz you to see where you are,\nplan a course, write lessons for Obsidian, and check your progress.",
			styleDim.Render("/courses to resume a course · /help for commands"),
			styleDim.Render("courses: "+dir)))
	}
	return box.Render(fmt.Sprintf("%s nextpage\n\n%s\n%s",
		styleSelected.Render("✻"),
		styleDim.Render("/help for help · shift+tab to change permission mode"),
		styleDim.Render("cwd: "+m.d.Env.Cwd())))
}

func (m *Model) markdown(text string, width int) string {
	if m.md == nil || m.mdWidth != width {
		r, err := glamour.NewTermRenderer(glamour.WithStandardStyle("dark"), glamour.WithWordWrap(max(width-4, 20)))
		if err != nil {
			return text
		}
		m.md, m.mdWidth = r, width
	}
	out, err := m.md.Render(text)
	if err != nil {
		return text
	}
	return strings.Trim(out, "\n")
}

func wrap(s string, width int) string {
	return lipgloss.NewStyle().Width(max(width, 10)).Render(s)
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

// result indents s under a tool line, with the ⎿ connector on the first line only.
func result(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		if i == 0 {
			lines[i] = "  ⎿  " + lines[i]
		} else {
			lines[i] = "     " + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

func (m *Model) render(e *entry, w int) string {
	switch e.kind {
	case kindUser:
		return styleUser.Render(wrap("> "+e.text, w))
	case kindAssistant:
		if e.streaming {
			return wrap("● "+e.text, w)
		}
		return m.markdown(e.text, w)
	case kindThinking:
		text := strings.TrimSpace(e.text)
		if !m.verbose && !e.streaming {
			return styleThinking.Render("✻ Thought for a while (ctrl+o to show)")
		}
		return styleThinking.Render(wrap("✻ "+text, w))
	case kindTool:
		return m.renderTool(e, w)
	case kindBash:
		head := styleWarn.Render("! ") + e.text
		out := e.output
		if out == "" {
			out = "(no output)"
		}
		style := styleDim
		if e.isErr {
			style = styleErr
		}
		return head + "\n" + result(style.Render(wrap(m.clip(out, 20), w-5)))
	case kindNotice:
		return styleDim.Render(wrap(e.text, w))
	case kindWarn:
		return styleWarn.Render(wrap(e.text, w))
	case kindError:
		return styleErr.Render(wrap(e.text, w))
	case kindDebug:
		if !m.verbose {
			return ""
		}
		return styleDim.Render(wrap("· "+e.text, w))
	}
	return e.text
}

func displayToolName(name string) string {
	if rest, ok := strings.CutPrefix(name, "mcp__"); ok {
		server, tool, _ := strings.Cut(rest, "__")
		return fmt.Sprintf("%s - %s (MCP)", server, tool)
	}
	return name
}

func (m *Model) renderTool(e *entry, w int) string {
	// For the tutor, failed calls are internal retries; show them only in
	// verbose mode (they are always in the log).
	if m.d.Courses != nil && e.result != nil && e.result.IsError && !m.verbose {
		return ""
	}
	bullet := styleWarn.Render("○")
	if e.result != nil {
		if e.result.IsError {
			bullet = styleErr.Render("●")
		} else {
			bullet = styleOK.Render("●")
		}
	}
	head := styleBold.Render(displayToolName(e.toolName))
	if e.subject != "" {
		head += "(" + oneLine(e.subject, max(w-len(e.toolName)-8, 20)) + ")"
	}
	line := bullet + " " + head

	var body string
	switch {
	case e.result == nil:
		body = styleDim.Render("Running…")
	case e.result.Display != "" && !e.result.IsError:
		body = m.clip(e.result.Display, 12)
		if strings.HasPrefix(e.result.Display, "+ ") || strings.HasPrefix(e.result.Display, "- ") || strings.Contains(e.result.Display, "\n+ ") || strings.Contains(e.result.Display, "\n- ") {
			body = colorDiff(body)
		} else {
			body = styleDim.Render(body)
		}
	default:
		out := strings.TrimSpace(e.result.Output)
		if out == "" {
			out = "(no output)"
		}
		body = m.clip(out, 4)
		if e.result.IsError {
			body = styleErr.Render(body)
		} else {
			body = styleDim.Render(body)
		}
	}
	out := line + "\n" + result(wrap(body, w-6))
	if e.agent != "" {
		out = indent(styleDim.Render("↳ "+e.agent)+"\n"+out, "    ")
	}
	return out
}

// clip shows the first n lines unless verbose mode is on.
func (m *Model) clip(s string, n int) string {
	lines := strings.Split(s, "\n")
	if m.verbose {
		n = 400
	}
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n… +%d lines (ctrl+o to expand)", len(lines)-n)
}

func colorDiff(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "+"):
			lines[i] = styleAdd.Render(l)
		case strings.HasPrefix(l, "-"):
			lines[i] = styleDel.Render(l)
		default:
			lines[i] = styleDim.Render(l)
		}
	}
	return strings.Join(lines, "\n")
}

// layout renders the bottom panel and sizes the viewport to fill the rest
// of the screen. It runs at the end of every Update, before View, so the
// viewport's height is correct before anything scrolls it. If the transcript
// was pinned to the bottom it stays pinned when the panel grows or shrinks.
func (m *Model) layout() {
	var bottom []string
	if m.showTodos && len(m.todos) > 0 && !allDone(m.todos) {
		bottom = append(bottom, m.renderTodos())
	}
	if m.running {
		elapsed := time.Since(m.runStart).Round(time.Second)
		status := fmt.Sprintf("%s %s… (%s · esc to interrupt", m.spinner.View(), m.verb, elapsed)
		if m.usage.CompletionTokens > 0 {
			status += fmt.Sprintf(" · ↓ %d tokens", m.usage.CompletionTokens)
		}
		status += ")"
		if len(m.queue) > 0 {
			status += styleDim.Render(fmt.Sprintf("  [%d queued]", len(m.queue)))
		}
		bottom = append(bottom, styleWarn.Render(status))
	}
	if m.dialog != nil {
		bottom = append(bottom, m.renderDialog())
	}
	if m.dialog == nil || m.dialog.textMode {
		bottom = append(bottom, styleInputBox.Render(m.input.View()))
	}
	if len(m.completions) > 0 {
		bottom = append(bottom, m.renderCompletions())
	}
	bottom = append(bottom, m.footer())

	// Clip to the terminal width so nothing soft-wraps and throws off the
	// height calculation.
	m.bottom = clipWidth(strings.Join(bottom, "\n"), m.width)
	h := max(m.height-lipgloss.Height(m.bottom), 1)
	if h != m.viewport.Height() {
		pinned := m.viewport.AtBottom()
		m.viewport.SetHeight(h)
		if pinned {
			m.viewport.GotoBottom()
		}
	}
}

func clipWidth(s string, w int) string {
	if w <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if ansi.StringWidth(l) > w {
			lines[i] = ansi.Truncate(l, w, "")
		}
	}
	return strings.Join(lines, "\n")
}

func (m *Model) View() tea.View {
	v := tea.NewView(m.viewport.View() + "\n" + m.bottom)
	v.AltScreen = true
	if m.mouse {
		v.MouseMode = tea.MouseModeCellMotion
	}
	v.WindowTitle = "nextpage"
	return v
}

func allDone(ts []tools.Todo) bool {
	for _, t := range ts {
		if t.Status != "completed" {
			return false
		}
	}
	return true
}

func (m *Model) renderTodos() string {
	var lines []string
	for _, t := range m.todos {
		switch t.Status {
		case "completed":
			lines = append(lines, styleDim.Render("  ☒ "+lipgloss.NewStyle().Strikethrough(true).Render(t.Content)))
		case "in_progress":
			label := t.ActiveForm
			if label == "" {
				label = t.Content
			}
			lines = append(lines, styleSelected.Render("  ◐ "+label))
		default:
			lines = append(lines, "  ☐ "+t.Content)
		}
	}
	return styleDim.Render("Todos (ctrl+t to hide)") + "\n" + strings.Join(lines, "\n")
}

func (m *Model) renderCompletions() string {
	var lines []string
	start := max(0, min(m.compIdx-3, len(m.completions)-8))
	for i := start; i < len(m.completions) && i < start+8; i++ {
		c := m.completions[i]
		name := fmt.Sprintf("  /%-20s", c.value)
		if i == m.compIdx {
			lines = append(lines, styleSelected.Render(name)+" "+c.desc)
		} else {
			lines = append(lines, name+" "+styleDim.Render(c.desc))
		}
	}
	return strings.Join(lines, "\n")
}

func (m *Model) footer() string {
	var left string
	switch m.d.Perms.Mode() {
	case permission.ModeAcceptEdits:
		left = lipgloss.NewStyle().Foreground(colorEdits).Render("⏵⏵ accept edits on") + styleDim.Render(" (shift+tab to cycle)")
	case permission.ModePlan:
		left = lipgloss.NewStyle().Foreground(colorPlan).Render("⏸ plan mode on") + styleDim.Render(" (shift+tab to cycle)")
	case permission.ModeBypass:
		left = lipgloss.NewStyle().Foreground(colorBypass).Render("⏵⏵ bypass permissions on") + styleDim.Render(" (shift+tab to cycle)")
	default:
		left = styleDim.Render("? /help for shortcuts")
	}
	right := m.d.Agent.Model
	if n := m.d.Agent.NumCtx; n > 0 && m.ctxTokens > 0 {
		right += fmt.Sprintf(" · ctx %d%%", m.ctxTokens*100/n)
	}
	if m.d.Agent.Think {
		right += " · thinking"
	}
	right = styleModel.Render(right)
	gap := max(m.width-lipgloss.Width(left)-lipgloss.Width(right), 1)
	return left + strings.Repeat(" ", gap) + right
}
