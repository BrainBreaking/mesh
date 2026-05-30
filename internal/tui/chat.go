// Package tui provides a Bubbletea-based TUI for interactive chat.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"

	"github.com/BrainBreaking/mesh/internal/chat"
)

// ── tea messages ─────────────────────────────────────────────────────────────

type chunkMsg string

type streamDone struct{ err error }

// ── entry ─────────────────────────────────────────────────────────────────────

type entryRole int

const (
	roleUser entryRole = iota
	roleAssistant
	roleError
)

type entry struct {
	role      entryRole
	label     string    // e.g. "claude-code"
	raw       string    // original text / markdown
	rendered  string    // glamour-rendered (set when streaming finishes)
	streaming bool      // true while chunks are still arriving
	at        time.Time
}

// ── ChatModel ─────────────────────────────────────────────────────────────────

// ChatModel is the Bubbletea model for the chat TUI.
type ChatModel struct {
	// terminal dimensions
	width, height int
	ready         bool

	// UI components
	vp   viewport.Model
	ta   textarea.Model
	spin spinner.Model

	// conversation state
	entries []entry
	eventCh chan tea.Msg // receives chunkMsg and streamDone from goroutine

	// session dependencies
	sess        *chat.Session
	backendID   string
	backendType string
	modelName   string
	rulesCount  int

	// context for cancellation
	ctx    context.Context
	cancel context.CancelFunc

	// glamour markdown renderer (rebuilt on resize)
	renderer  *glamour.TermRenderer
	rendererW int // last width the renderer was built for
}

// ── RunChat ───────────────────────────────────────────────────────────────────

// RunChat starts the TUI and blocks until the user quits.
func RunChat(sess *chat.Session, backendID, backendType, modelName string, rulesCount int) error {
	m := newChatModel(sess, backendID, backendType, modelName, rulesCount)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()
	return err
}

// ── constructor ───────────────────────────────────────────────────────────────

func newChatModel(
	sess *chat.Session,
	backendID, backendType, modelName string,
	rulesCount int,
) ChatModel {
	ta := textarea.New()
	ta.Placeholder = "Type a message…  (shift+enter for newline)"
	ta.Focus()
	ta.Prompt = " ┃ "
	ta.CharLimit = 0
	ta.SetWidth(80)
	ta.SetHeight(3)
	ta.ShowLineNumbers = false
	// Remap shift+enter → insert newline; plain enter is intercepted in Update.
	ta.KeyMap.InsertNewline.SetKeys("shift+enter")

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = styleSpinner

	ctx, cancel := context.WithCancel(context.Background())

	return ChatModel{
		ta:          ta,
		spin:        sp,
		sess:        sess,
		backendID:   backendID,
		backendType: backendType,
		modelName:   modelName,
		rulesCount:  rulesCount,
		ctx:         ctx,
		cancel:      cancel,
	}
}

// ── Init ──────────────────────────────────────────────────────────────────────

func (m ChatModel) Init() tea.Cmd {
	return textarea.Blink
}

// ── Update ────────────────────────────────────────────────────────────────────

func (m ChatModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {

	// ── window resize ──────────────────────────────────────────────────────
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.applyResize()

	// ── keyboard ───────────────────────────────────────────────────────────
	case tea.KeyMsg:
		switch msg.Type {

		case tea.KeyCtrlC:
			m.cancel()
			return m, tea.Quit

		case tea.KeyEnter:
			// Don't accept input while streaming.
			if m.isStreaming() {
				return m, nil
			}
			text := strings.TrimSpace(m.ta.Value())
			if text == "" {
				return m, nil
			}
			m.ta.Reset()
			cmds = append(cmds, m.startSend(text))
			// Early return: don't forward enter to textarea (avoid newline insertion).
			return m, tea.Batch(cmds...)

		case tea.KeyCtrlL:
			m.entries = nil
			m.updateViewport()
			return m, nil
		}

	// ── stream chunk ───────────────────────────────────────────────────────
	case chunkMsg:
		if n := len(m.entries); n > 0 {
			last := &m.entries[n-1]
			if last.streaming {
				last.raw += string(msg)
			}
		}
		m.updateViewport()
		cmds = append(cmds, m.listenForEvent())

	// ── stream done ────────────────────────────────────────────────────────
	case streamDone:
		if n := len(m.entries); n > 0 {
			last := &m.entries[n-1]
			if last.streaming {
				last.streaming = false
				last.rendered = m.renderMD(last.raw)
			}
		}
		if msg.err != nil {
			m.entries = append(m.entries, entry{
				role:  roleError,
				raw:   msg.err.Error(),
				at:    time.Now(),
			})
		}
		m.updateViewport()
		m.vp.GotoBottom()

	// ── spinner tick ───────────────────────────────────────────────────────
	case spinner.TickMsg:
		if m.isStreaming() {
			var cmd tea.Cmd
			m.spin, cmd = m.spin.Update(msg)
			cmds = append(cmds, cmd)
			m.updateViewport()
		}
	}

	// Forward remaining messages to textarea and viewport.
	var taCmd tea.Cmd
	m.ta, taCmd = m.ta.Update(msg)
	cmds = append(cmds, taCmd)

	var vpCmd tea.Cmd
	m.vp, vpCmd = m.vp.Update(msg)
	cmds = append(cmds, vpCmd)

	return m, tea.Batch(cmds...)
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m ChatModel) View() string {
	if !m.ready {
		return "loading…\n"
	}
	return m.renderStatusBar() + "\n" +
		m.vp.View() + "\n" +
		m.ta.View() + "\n" +
		m.renderHintBar()
}

// ── helpers ───────────────────────────────────────────────────────────────────

func (m *ChatModel) isStreaming() bool {
	if n := len(m.entries); n > 0 {
		return m.entries[n-1].streaming
	}
	return false
}

// ── resize ────────────────────────────────────────────────────────────────────

func (m *ChatModel) applyResize() {
	const (
		statusLines = 1
		hintLines   = 1
		taLines     = 5 // textarea height + surrounding newlines
		separators  = 3 // newlines between sections
	)
	vpH := m.height - statusLines - hintLines - taLines - separators
	if vpH < 4 {
		vpH = 4
	}

	if !m.ready {
		m.vp = viewport.New(m.width, vpH)
		m.ready = true
	} else {
		m.vp.Width = m.width
		m.vp.Height = vpH
	}

	m.ta.SetWidth(m.width - 2)
	m.rebuildRenderer()
	m.rerenderAll()
	m.updateViewport()
}

// ── markdown renderer ─────────────────────────────────────────────────────────

func (m *ChatModel) rebuildRenderer() {
	// Inner content width: full width minus border (2) minus padding (2) minus margin (2).
	w := m.width - 6
	if w < 20 {
		w = 20
	}
	if w == m.rendererW && m.renderer != nil {
		return
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(w),
	)
	if err == nil {
		m.renderer = r
		m.rendererW = w
	}
}

func (m *ChatModel) renderMD(raw string) string {
	if m.renderer == nil || strings.TrimSpace(raw) == "" {
		return raw
	}
	out, err := m.renderer.Render(raw)
	if err != nil {
		return raw
	}
	return strings.TrimRight(out, "\n")
}

func (m *ChatModel) rerenderAll() {
	for i := range m.entries {
		e := &m.entries[i]
		if e.role == roleAssistant && !e.streaming {
			e.rendered = m.renderMD(e.raw)
		}
	}
}

// ── viewport content ──────────────────────────────────────────────────────────

func (m *ChatModel) updateViewport() {
	if !m.ready {
		return
	}
	m.vp.SetContent(m.buildContent())
	if m.isStreaming() {
		m.vp.GotoBottom()
	}
}

func (m *ChatModel) buildContent() string {
	if len(m.entries) == 0 {
		return styleEmpty.Render("No messages yet. Start typing below.\n")
	}

	// Bubble inner width: full width minus border (2) minus padding (2) minus margin (2).
	bubbleW := m.width - 6
	if bubbleW < 20 {
		bubbleW = 20
	}

	var sb strings.Builder
	sb.WriteString("\n")
	for _, e := range m.entries {
		sb.WriteString(m.renderBubble(e, bubbleW))
		sb.WriteString("\n")
	}
	return sb.String()
}

func (m *ChatModel) renderBubble(e entry, innerW int) string {
	// ── header: "label   <spacer>   HH:MM [spinner]"
	ts := e.at.Format("15:04")

	var labelStr string
	switch e.role {
	case roleUser:
		labelStr = styleUserLabel.Render("you")
	case roleAssistant:
		lbl := e.label
		if lbl == "" {
			lbl = "assistant"
		}
		labelStr = styleAssistLabel.Render(lbl)
	case roleError:
		labelStr = styleErrorLabel.Render("error")
	}

	spinStr := ""
	if e.streaming {
		spinStr = " " + m.spin.View()
	}
	tsStr := styleTimestamp.Render(ts + spinStr)

	labelW := lipgloss.Width(labelStr)
	tsW := lipgloss.Width(tsStr)
	spacerW := innerW - labelW - tsW
	if spacerW < 1 {
		spacerW = 1
	}
	header := labelStr + strings.Repeat(" ", spacerW) + tsStr

	// ── body
	var body string
	switch e.role {
	case roleAssistant:
		if e.streaming {
			if e.raw == "" {
				body = styleBodyMuted.Render("…")
			} else {
				// Show raw text while streaming; glamour render applied on completion.
				body = e.raw
			}
		} else {
			if e.rendered != "" {
				body = e.rendered
			} else {
				body = m.renderMD(e.raw)
			}
		}
	default:
		body = e.raw
	}

	content := header + "\n" + body

	// ── bubble border style
	fullW := innerW + 4 // border (2) + padding (2)
	switch e.role {
	case roleUser:
		return styleUserBubble.Width(fullW).Render(content)
	case roleAssistant:
		return styleAssistBubble.Width(fullW).Render(content)
	default:
		return styleErrorBubble.Width(fullW).Render(content)
	}
}

// ── status bar ────────────────────────────────────────────────────────────────

func (m ChatModel) renderStatusBar() string {
	mn := m.modelName
	if mn == "" {
		mn = "—"
	}
	left := fmt.Sprintf(" mesh · %s · %s · %d rules",
		m.backendType, mn, m.rulesCount)

	turns := 0
	for _, e := range m.entries {
		if e.role == roleUser {
			turns++
		}
	}
	right := fmt.Sprintf("%d turn(s) ", turns)

	spacerW := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if spacerW < 0 {
		spacerW = 0
	}

	return styleStatusBar.Render(left) +
		styleStatusDim.Render(strings.Repeat(" ", spacerW)) +
		styleStatusDim.Render(right)
}

// ── hint bar ──────────────────────────────────────────────────────────────────

func (m ChatModel) renderHintBar() string {
	return styleHint.Render(
		" enter send  ·  shift+enter newline  ·  ↑/↓ scroll  ·  ctrl+l clear  ·  ctrl+c quit",
	)
}

// ── streaming plumbing ────────────────────────────────────────────────────────

// startSend appends user + assistant entries and launches the backend call.
func (m *ChatModel) startSend(text string) tea.Cmd {
	m.entries = append(m.entries, entry{
		role: roleUser,
		raw:  text,
		at:   time.Now(),
	})
	m.entries = append(m.entries, entry{
		role:      roleAssistant,
		label:     m.backendID,
		streaming: true,
		at:        time.Now(),
	})
	m.updateViewport()

	ch := make(chan tea.Msg, 256)
	m.eventCh = ch
	sess := m.sess
	ctx := m.ctx

	go func() {
		_, err := sess.Send(ctx, text, func(chunk string) {
			ch <- chunkMsg(chunk)
		})
		ch <- streamDone{err: err}
	}()

	return tea.Batch(m.listenForEvent(), m.spin.Tick)
}

// listenForEvent returns a command that reads the next event from the channel.
func (m *ChatModel) listenForEvent() tea.Cmd {
	ch := m.eventCh
	return func() tea.Msg {
		return <-ch
	}
}
