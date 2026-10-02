package main

import (
	"log"
	"math/rand"
	"os"
	"strings"

	"charm.land/bubbles/v2/cursor"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type streamEvent struct {
	chunk string
	err   error
}

type chunkMsg string
type requestErrorMsg struct{ err error }
type requestDoneMsg struct{ err error }

func (m model) runRequest(prompt string, model string, events chan<- streamEvent) tea.Cmd {
	return func() tea.Msg {
		defer close(events)

		err := SendRequest(prompt, model, func(chunk string) error {
			events <- streamEvent{chunk: chunk}
			return nil
		})
		if err != nil {
			events <- streamEvent{err: err}
		}
		return nil
	}
}

func waitForEvent(events <-chan streamEvent) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-events
		if !ok {
			return requestDoneMsg{}
		}
		if event.err != nil {
			return requestErrorMsg{event.err}
		}
		return chunkMsg(event.chunk)
	}
}

type statusLine struct {
	spinner     spinner.Model
	loadingText string
}

func getRandomSpinner() spinner.Spinner {
	spinners := []spinner.Spinner{
		spinner.Line,
		spinner.Dot,
		spinner.MiniDot,
		spinner.Jump,
		spinner.Points,
		spinner.Globe,
		spinner.Moon,
		spinner.Monkey,
		spinner.Meter,
		spinner.Ellipsis,
	}

	randomIndex := rand.Intn(len(spinners))
	return spinners[randomIndex]

}

type model struct {
	viewport    viewport.Model
	messages    []string
	aiModel     string
	textarea    textarea.Model
	events      chan streamEvent
	statusLine  statusLine
	borderStyle lipgloss.Style
	err         error
}

func initialModel() model {
	aiModel := os.Getenv("MODEL")
	if aiModel == "" {
		log.Fatal("set MODEL")
	}
	ta := textarea.New()
	ta.Placeholder = "Send a message..."
	ta.SetVirtualCursor(true)
	ta.Focus()

	ta.Prompt = "| "
	ta.CharLimit = 280

	ta.SetWidth(30)
	ta.SetHeight(3)

	// Remove cursor line styling
	cursorStyle := ta.Styles()
	cursorStyle.Focused.CursorLine = lipgloss.NewStyle()
	ta.SetStyles(cursorStyle)

	ta.ShowLineNumbers = false

	vp := viewport.New(viewport.WithWidth(30), viewport.WithHeight(5))
	vp.SetContent(`Welcome to Eric Chat!
Type a message and press Enter to send.`)
	vp.KeyMap.Left.SetEnabled(false)
	vp.KeyMap.Right.SetEnabled(false)

	ta.KeyMap.InsertNewline.SetEnabled(false)

	s := spinner.New()
	s.Spinner = getRandomSpinner()
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	statusLine := statusLine{
		spinner:     s,
		loadingText: "Thinking...",
	}

	borderStyle := lipgloss.NewStyle().BorderStyle(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#b4befe"))

	return model{
		textarea:    ta,
		messages:    []string{},
		aiModel:     aiModel,
		viewport:    vp,
		statusLine:  statusLine,
		borderStyle: borderStyle,
		err:         nil,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(
		textarea.Blink,
		m.statusLine.spinner.Tick,
	)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.viewport.SetWidth(msg.Width)
		m.textarea.SetWidth(msg.Width - m.borderStyle.GetHorizontalFrameSize())
		m.viewport.SetHeight(msg.Height - m.borderStyle.GetVerticalBorderSize() - m.textarea.Height() - 1) // The -1 is from the statusbar

		if len(m.messages) > 0 {
			m.refreshViewport()
		}
		m.viewport.GotoBottom()
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "enter":
			if m.events != nil {
				return m, nil
			}
			userInput := m.textarea.Value()
			m.textarea.Reset()
			m.messages = append(m.messages, "You: "+userInput)
			m.messages = append(m.messages, "Assistant: ")
			m.refreshViewport()
			m.viewport.GotoBottom()

			events := make(chan streamEvent, 16)
			m.events = events

			m.statusLine.spinner.Spinner = getRandomSpinner()

			return m, tea.Batch(
				m.runRequest(userInput, m.aiModel, events),
				waitForEvent(events),
			)
		default:
			var cmd tea.Cmd
			m.textarea, cmd = m.textarea.Update(msg)
			return m, cmd
		}

	case chunkMsg:
		last := len(m.messages) - 1
		m.messages[last] += string(msg)
		m.refreshViewport()

		return m, waitForEvent(m.events)

	case requestErrorMsg:
		m.messages = append(m.messages, "Error: "+msg.err.Error())
		m.events = nil
		m.refreshViewport()
		return m, nil

	case requestDoneMsg:
		m.events = nil
		return m, nil

	case cursor.BlinkMsg:
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		return m, cmd
	default:
		var cmd tea.Cmd
		m.statusLine.spinner, cmd = m.statusLine.spinner.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m model) View() tea.View {
	viewportView := m.viewport.View()

	inputView := m.borderStyle.Render(m.textarea.View())

	var statusLine string
	if m.events != nil {
		statusLine += m.statusLine.spinner.View() + " " + m.statusLine.loadingText
	}
	rightStyle := lipgloss.NewStyle().Width(m.viewport.Width() - lipgloss.Width(statusLine)).Align(lipgloss.Right).Foreground(lipgloss.Color("#FA8072")).Italic(true)
	statusLine += rightStyle.Render(m.aiModel)
	// statusColor := lipgloss.NewStyle().Background(lipgloss.Color("#313244"))
	// statusLine = statusColor.Render(statusLine)

	v := tea.NewView(viewportView + "\n" + inputView + "\n" + statusLine)
	c := m.textarea.Cursor()
	// Move cursor under the viewport
	if c != nil {
		c.Y += lipgloss.Height(viewportView)
	}
	v.Cursor = c
	v.AltScreen = true
	return v
}

func (m *model) refreshViewport() {
	content := strings.Join(m.messages, "\n")
	content = lipgloss.NewStyle().Width(m.viewport.Width()).Render(content)
	m.viewport.SetContent(content)
	m.viewport.GotoBottom()
}
