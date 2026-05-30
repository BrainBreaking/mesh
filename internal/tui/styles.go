package tui

import "github.com/charmbracelet/lipgloss"

// ── palette ───────────────────────────────────────────────────────────────────

var (
	colGreen  = lipgloss.Color("2")
	colCyan   = lipgloss.Color("6")
	colRed    = lipgloss.Color("1")
	colMuted  = lipgloss.Color("8") // bright-black / gray
	colWhite  = lipgloss.Color("7")
	colBlack  = lipgloss.Color("0")
)

// ── status bar ────────────────────────────────────────────────────────────────

var (
	styleStatusBar = lipgloss.NewStyle().
		Background(colBlack).
		Foreground(colGreen).
		Bold(true)

	styleStatusDim = lipgloss.NewStyle().
		Background(colBlack).
		Foreground(colMuted)
)

// ── hint bar ──────────────────────────────────────────────────────────────────

var styleHint = lipgloss.NewStyle().
	Foreground(colMuted)

// ── message bubbles ───────────────────────────────────────────────────────────

var (
	styleUserBubble = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colGreen).
		Padding(0, 1).
		MarginLeft(1).
		MarginBottom(1)

	styleAssistBubble = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colMuted).
		Padding(0, 1).
		MarginLeft(1).
		MarginBottom(1)

	styleErrorBubble = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colRed).
		Padding(0, 1).
		MarginLeft(1).
		MarginBottom(1)
)

// ── bubble labels ─────────────────────────────────────────────────────────────

var (
	styleUserLabel = lipgloss.NewStyle().
		Foreground(colGreen).
		Bold(true)

	styleAssistLabel = lipgloss.NewStyle().
		Foreground(colCyan).
		Bold(true)

	styleErrorLabel = lipgloss.NewStyle().
		Foreground(colRed).
		Bold(true)

	styleTimestamp = lipgloss.NewStyle().
		Foreground(colMuted)

	styleBodyMuted = lipgloss.NewStyle().
		Foreground(colMuted).
		Italic(true)

	styleSpinner = lipgloss.NewStyle().
		Foreground(colCyan)

	styleEmpty = lipgloss.NewStyle().
		Foreground(colMuted).
		Italic(true).
		PaddingLeft(2).
		PaddingTop(1)

	// System messages (slash-command responses) use a minimal inline style —
	// no bubble border, just a dim indicator prefix.
	styleSystemLine = lipgloss.NewStyle().
		Foreground(colMuted)

	styleSystemLabel = lipgloss.NewStyle().
		Foreground(lipgloss.Color("3")). // yellow
		Bold(true)
)
