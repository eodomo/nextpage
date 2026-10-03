package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/eodomo/nextpage/internal/agent"
	"github.com/eodomo/nextpage/internal/learn"
)

// handleInteraction shows plugin-defined requests (tools.Env.Interact).
// Unknown payloads get a nil reply so the agent never hangs.
func (m *Model) handleInteraction(req agent.InteractionRequest) {
	switch p := req.Payload.(type) {
	case learn.QuizRequest:
		m.startQuiz(p, req.Reply)
	case learn.DirectoryRequest:
		m.askDirectory(p, req.Reply)
	default:
		req.Reply <- nil
	}
}

func (m *Model) askDirectory(p learn.DirectoryRequest, reply chan any) {
	m.dialog = &dialog{
		title:      "Where should course notes be saved?",
		body:       styleDim.Render("Pick a folder inside your Obsidian vault to read lessons there. This is remembered in ~/.nextpage/settings.json."),
		options:    []string{"Use " + p.Default, "Choose another folder"},
		textOption: 1,
		textPrompt: "Folder path (e.g. ~/Obsidian/Vault/Courses)",
		onSelect: func(i int, text string) tea.Cmd {
			if i == 0 || text == "" {
				reply <- p.Default
			} else {
				reply <- text
			}
			return nil
		},
		onCancel: func() tea.Cmd {
			reply <- p.Default
			return nil
		},
	}
}

// startQuiz walks through the questions one dialog at a time and replies
// with all answers at the end.
func (m *Model) startQuiz(q learn.QuizRequest, reply chan any) {
	answers := make([]string, len(q.Questions))
	m.add(&entry{kind: kindNotice, text: fmt.Sprintf("📝 %s — %d questions", q.Title, len(q.Questions))})
	var ask func(i int)
	ask = func(i int) {
		if i == len(q.Questions) {
			m.dialog = nil
			var lines []string
			for j, qq := range q.Questions {
				lines = append(lines, fmt.Sprintf("%d. %s\n   → %s", j+1, oneLine(qq.Prompt, 80), orBlank(answers[j])))
			}
			m.notice(kindNotice, "Your answers:\n"+strings.Join(lines, "\n"))
			reply <- answers
			return
		}
		qq := q.Questions[i]
		title := fmt.Sprintf("%s · question %d of %d", q.Title, i+1, len(q.Questions))
		body := m.markdown(qq.Prompt, m.width-6)
		next := func(ans string) tea.Cmd {
			answers[i] = ans
			ask(i + 1)
			return nil
		}
		if qq.Type == learn.TypeMultipleChoice {
			m.dialog = &dialog{
				title:      title,
				body:       body,
				options:    qq.Options,
				textOption: -1,
				onSelect:   func(j int, _ string) tea.Cmd { return next(qq.Options[j]) },
				onCancel:   func() tea.Cmd { return next("") },
			}
			return
		}
		m.dialog = &dialog{
			title:      title,
			body:       body,
			options:    []string{"Type your answer", "Skip"},
			textOption: 0,
			textMode:   true,
			textPrompt: "Your answer (enter to submit)",
			onSelect: func(j int, text string) tea.Cmd {
				if j == 1 {
					return next("")
				}
				return next(text)
			},
			onCancel: func() tea.Cmd { return next("") },
		}
		m.input.Reset()
		m.input.Placeholder = "Your answer (enter to submit, esc for options)"
	}
	ask(0)
}

func orBlank(s string) string {
	if s == "" {
		return "(skipped)"
	}
	return s
}

func (m *Model) cmdCourses(string) tea.Cmd {
	c := m.d.Courses
	if c == nil {
		m.notice(kindNotice, "Courses are only available in the learn profile (--profile learn).")
		return nil
	}
	var b strings.Builder
	if c.Dir() == "" {
		b.WriteString("No courses directory yet; you'll be asked when you start a course (or pass --courses-dir).")
	} else {
		fmt.Fprintf(&b, "Courses in %s:\n", c.Dir())
		list := c.List()
		if len(list) == 0 {
			b.WriteString("  (none yet; tell me what you want to learn)")
		}
		for _, s := range list {
			active := ""
			if s.Active {
				active = "  ← active"
			}
			fmt.Fprintf(&b, "  %s — %s, %d/%d sections passed%s\n", s.Folder, s.Phase, s.Passed, s.Sections, active)
		}
		b.WriteString("\nSay \"continue <course>\" to pick one up again.")
	}
	m.notice(kindNotice, strings.TrimRight(b.String(), "\n"))
	return nil
}
