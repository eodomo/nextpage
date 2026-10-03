package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/eodomo/nextpage/internal/agent"
)

// dialog is a modal list of choices shown in place of the input box. One
// option may switch to free-text entry (textOption), reusing the input box.
type dialog struct {
	title      string
	body       string
	options    []string
	cursor     int
	textOption int // -1 if none
	textMode   bool
	textPrompt string
	onSelect   func(idx int, text string) tea.Cmd
	onCancel   func() tea.Cmd
}

func (m *Model) closeDialog() {
	m.dialog = nil
	m.input.Placeholder = m.placeholder()
	m.input.Reset()
}

func (m *Model) dialogKey(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	d := m.dialog
	key := msg.String()
	if d.textMode {
		if key == "esc" {
			d.textMode = false
			m.input.Placeholder = m.placeholder()
			m.input.Reset()
			return nil, true
		}
		return nil, false // enter is handled by dialogSubmitText; other keys edit the input
	}
	switch key {
	case "up", "k":
		d.cursor = (d.cursor - 1 + len(d.options)) % len(d.options)
	case "down", "j", "tab":
		d.cursor = (d.cursor + 1) % len(d.options)
	case "enter":
		return m.dialogChoose(d.cursor), true
	case "esc":
		m.closeDialog()
		if d.onCancel != nil {
			return d.onCancel(), true
		}
	default:
		if len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
			if i := int(key[0] - '1'); i < len(d.options) {
				return m.dialogChoose(i), true
			}
		}
	}
	return nil, true
}

func (m *Model) dialogChoose(i int) tea.Cmd {
	d := m.dialog
	if i == d.textOption {
		d.textMode = true
		m.input.Reset()
		m.input.Placeholder = d.textPrompt
		return nil
	}
	m.closeDialog()
	return d.onSelect(i, "")
}

func (m *Model) dialogSubmitText() tea.Cmd {
	d := m.dialog
	text := strings.TrimSpace(m.input.Value())
	m.closeDialog()
	return d.onSelect(d.textOption, text)
}

func (m *Model) renderDialog() string {
	d := m.dialog
	var b strings.Builder
	b.WriteString(styleBold.Render(d.title))
	if d.body != "" {
		b.WriteString("\n\n" + d.body)
	}
	b.WriteString("\n")
	for i, o := range d.options {
		label := fmt.Sprintf("%d. %s", i+1, o)
		if i == d.cursor {
			b.WriteString("\n" + styleSelected.Render("❯ "+label))
		} else {
			b.WriteString("\n  " + label)
		}
	}
	if d.textMode {
		b.WriteString("\n\n" + styleDim.Render(d.textPrompt+" (enter to send, esc to go back)"))
	} else {
		b.WriteString("\n\n" + styleDim.Render("↑/↓ to select · enter to confirm · esc to cancel"))
	}
	return styleDialogBox.Width(max(m.width, 20)).Render(b.String())
}

func (m *Model) openPermissionDialog(req agent.PermissionRequest) {
	title := "Allow " + displayToolName(req.ToolName)
	if req.Agent != "" {
		title += " (sub-agent: " + req.Agent + ")"
	}
	title += "?"
	body := req.Subject
	if req.Preview != "" && req.Preview != req.Subject {
		preview := m.clip(req.Preview, 20)
		if req.ToolName == "Edit" || req.ToolName == "Write" {
			preview = colorDiff(preview)
		}
		body = strings.TrimSpace(body + "\n\n" + preview)
	}
	once := false
	reply := func(r agent.PermissionReply) tea.Cmd {
		if !once {
			once = true
			req.Reply <- r
		}
		return nil
	}
	m.dialog = &dialog{
		title: title,
		body:  body,
		options: []string{
			"Yes",
			fmt.Sprintf("Yes, and don't ask again this session for %s", req.Suggestion),
			fmt.Sprintf("Yes, and always allow %s in this project", req.Suggestion),
			"No, and tell nextpage what to do differently",
		},
		textOption: 3,
		textPrompt: "Tell nextpage what to do differently",
		onSelect: func(i int, text string) tea.Cmd {
			switch i {
			case 0:
				return reply(agent.PermissionReply{Allow: true})
			case 1:
				return reply(agent.PermissionReply{Allow: true, Remember: agent.RememberSession})
			case 2:
				return reply(agent.PermissionReply{Allow: true, Remember: agent.RememberProject})
			}
			return reply(agent.PermissionReply{Allow: false, Feedback: text})
		},
		onCancel: func() tea.Cmd { return reply(agent.PermissionReply{Allow: false}) },
	}
}

func (m *Model) openQuestionDialog(req agent.QuestionRequest) {
	opts := append(append([]string{}, req.Question.Options...), "Other (type an answer)")
	m.dialog = &dialog{
		title:      req.Question.Question,
		options:    opts,
		textOption: len(opts) - 1,
		textPrompt: "Your answer",
		onSelect: func(i int, text string) tea.Cmd {
			if i == len(opts)-1 {
				req.Reply <- text
			} else {
				req.Reply <- opts[i]
			}
			return nil
		},
		onCancel: func() tea.Cmd {
			req.Reply <- "(the user declined to answer)"
			return nil
		},
	}
}

func (m *Model) openPlanDialog(req agent.PlanRequest) {
	m.dialog = &dialog{
		title:      "Ready to code? Here is the plan:",
		body:       m.markdown(req.Plan, m.width-4),
		options:    []string{"Yes, start implementing", "No, keep planning"},
		textOption: -1,
		onSelect: func(i int, _ string) tea.Cmd {
			req.Reply <- i == 0
			return nil
		},
		onCancel: func() tea.Cmd {
			req.Reply <- false
			return nil
		},
	}
}

func (m *Model) openPicker(title string, options []string, onSelect func(i int) tea.Cmd) {
	m.dialog = &dialog{
		title:      title,
		options:    options,
		textOption: -1,
		onSelect:   func(i int, _ string) tea.Cmd { return onSelect(i) },
	}
}
