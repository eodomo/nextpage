package tui

import (
	"math/rand"

	"charm.land/bubbles/v2/spinner"
	"charm.land/lipgloss/v2"
)

var (
	colorAccent  = lipgloss.Color("#b4befe")
	colorUser    = lipgloss.Color("#89b4fa")
	colorOK      = lipgloss.Color("#a6e3a1")
	colorErr     = lipgloss.Color("#f38ba8")
	colorWarn    = lipgloss.Color("#f9e2af")
	colorDim     = lipgloss.Color("#7f849c")
	colorModel   = lipgloss.Color("#FA8072")
	colorPlan    = lipgloss.Color("#94e2d5")
	colorEdits   = lipgloss.Color("#cba6f7")
	colorBypass  = lipgloss.Color("#f38ba8")
	colorSpinner = lipgloss.Color("205")

	styleUser      = lipgloss.NewStyle().Foreground(colorUser)
	styleDim       = lipgloss.NewStyle().Foreground(colorDim)
	styleThinking  = lipgloss.NewStyle().Foreground(colorDim).Italic(true)
	styleErr       = lipgloss.NewStyle().Foreground(colorErr)
	styleWarn      = lipgloss.NewStyle().Foreground(colorWarn)
	styleOK        = lipgloss.NewStyle().Foreground(colorOK)
	styleBold      = lipgloss.NewStyle().Bold(true)
	styleAdd       = lipgloss.NewStyle().Foreground(colorOK)
	styleDel       = lipgloss.NewStyle().Foreground(colorErr)
	styleModel     = lipgloss.NewStyle().Foreground(colorModel).Italic(true)
	styleSelected  = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	styleInputBox  = lipgloss.NewStyle().BorderStyle(lipgloss.RoundedBorder()).BorderForeground(colorAccent)
	styleDialogBox = lipgloss.NewStyle().BorderStyle(lipgloss.RoundedBorder()).BorderForeground(colorWarn).Padding(0, 1)
)

func randomSpinner() spinner.Spinner {
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
	return spinners[rand.Intn(len(spinners))]
}

var verbs = []string{
	"Thinking", "Pondering", "Musing", "Cogitating", "Noodling", "Percolating",
	"Ruminating", "Scheming", "Tinkering", "Conjuring", "Brewing", "Mulling",
}

func randomVerb() string { return verbs[rand.Intn(len(verbs))] }
