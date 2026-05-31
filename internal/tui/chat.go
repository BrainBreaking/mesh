// Package tui provides a Bubbletea-based TUI for interactive chat.
package tui

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	engstore "github.com/BrainBreaking/engram/store"
	"github.com/BrainBreaking/mesh/internal/chat"
)

// ── tea messages ─────────────────────────────────────────────────────────────

type chunkMsg string

type streamDone struct {
	err       error
	userMsg   string // original user message, used for auto-save
	sessionID string
}

// ── entry ─────────────────────────────────────────────────────────────────────

type entryRole int

const (
	roleUser entryRole = iota
	roleAssistant
	roleError
	roleSystem // slash-command responses
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

	// message queue — filled when Enter is pressed during streaming;
	// drained automatically when each response completes.
	queue []string

	// elapsed timer — set when streaming starts, read on every spinner tick.
	streamStartedAt time.Time

	// autocomplete state
	comps        []suggestion // current candidate list (nil = popup hidden)
	compIdx      int          // selected index; -1 = nothing selected
	hasCommander bool         // backend supports slash commands

	// engram memory
	mem       *engstore.Store // nil when engram is unavailable
	sessionID string          // active engram session
	memCount  int64           // cached observation count (updated optimistically)

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
	renderer    *glamour.TermRenderer
	rendererW   int    // last width the renderer was built for
	glamourStyle string // "dark" or "light", detected before Bubbletea starts
}

// ── RunChat ───────────────────────────────────────────────────────────────────

// RunChat starts the TUI and blocks until the user quits.
// mem may be nil — all memory features degrade gracefully when absent.
func RunChat(sess *chat.Session, backendID, backendType, modelName string, rulesCount int, mem *engstore.Store) error {
	// Detect terminal background colour HERE, before tea.NewProgram() puts the
	// terminal into raw mode. glamour.WithAutoStyle() sends an OSC escape to
	// query the background; if that query fires while Bubbletea owns stdin, the
	// terminal's reply (e.g. "11;rgb:2828/2c2c/3434") is read as keyboard input
	// and injected into the textarea as garbage text.
	glamourStyle := probeGlamourStyle()

	// Start an engram session so we can associate memories with this chat.
	// The defer ends it when p.Run() returns (covers Ctrl+C and normal quit).
	var sessionID string
	if mem != nil {
		sessionID = fmt.Sprintf("mesh-%d", time.Now().UnixNano())
		cwd, _ := os.Getwd()
		mem.StartSession(sessionID, backendID, cwd)
		defer mem.EndSession(sessionID, //nolint:errcheck
			fmt.Sprintf("mesh chat · backend=%s · model=%s", backendID, modelName))
	}

	m := newChatModel(sess, backendID, backendType, modelName, rulesCount, glamourStyle, mem, sessionID)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()
	return err
}

// probeGlamourStyle queries the terminal background colour while stdin is still
// in normal (cooked) mode and returns the matching glamour style name.
func probeGlamourStyle() string {
	if termenv.NewOutput(os.Stdout).HasDarkBackground() {
		return "dark"
	}
	return "light"
}

// ── constructor ───────────────────────────────────────────────────────────────

func newChatModel(
	sess *chat.Session,
	backendID, backendType, modelName string,
	rulesCount int,
	glamourStyle string,
	mem *engstore.Store,
	sessionID string,
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

	hasCommander := sess.HasCommander()

	// Seed memory count from store stats (best-effort).
	var memCount int64
	if mem != nil {
		if stats, err := mem.Stats(); err == nil {
			if n, ok := stats["total_observations"].(int64); ok {
				memCount = n
			}
		}
	}

	return ChatModel{
		ta:           ta,
		spin:         sp,
		sess:         sess,
		backendID:    backendID,
		backendType:  backendType,
		modelName:    modelName,
		rulesCount:   rulesCount,
		ctx:          ctx,
		cancel:       cancel,
		compIdx:      -1,
		hasCommander: hasCommander,
		glamourStyle: glamourStyle,
		mem:          mem,
		sessionID:    sessionID,
		memCount:     memCount,
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

		// ── autocomplete navigation ────────────────────────────────────────
		case tea.KeyTab:
			if len(m.comps) > 0 {
				m.compIdx = (m.compIdx + 1) % len(m.comps)
				return m, nil
			}

		case tea.KeyShiftTab:
			if len(m.comps) > 0 {
				if m.compIdx <= 0 {
					m.compIdx = len(m.comps) - 1
				} else {
					m.compIdx--
				}
				return m, nil
			}

		case tea.KeyEscape:
			if len(m.comps) > 0 {
				m.comps = nil
				m.compIdx = -1
				m.applyResize()
				return m, nil
			}

		case tea.KeyEnter:
			// Accept the highlighted completion instead of sending.
			if m.compIdx >= 0 && m.compIdx < len(m.comps) {
				m.ta.SetValue(m.comps[m.compIdx].full)
				m.comps = nil
				m.compIdx = -1
				m.applyResize()
				return m, nil
			}

			text := strings.TrimSpace(m.ta.Value())
			if text == "" {
				return m, nil
			}
			m.ta.Reset()
			m.comps = nil
			m.compIdx = -1
			m.applyResize()

			// Slash commands are always handled immediately, never queued.
			if strings.HasPrefix(text, "/") {
				var sysMsg string

				// ── memory commands (handled by TUI, no backend call) ──────────
				switch {
				case strings.HasPrefix(text, "/remember "):
					sysMsg = m.handleRemember(strings.TrimPrefix(text, "/remember "))
				case text == "/remember":
					sysMsg = "usage: /remember <text to store>"
				case strings.HasPrefix(text, "/recall "):
					sysMsg = m.handleRecall(strings.TrimPrefix(text, "/recall "))
				case text == "/recall":
					sysMsg = "usage: /recall <search query>"
				case strings.HasPrefix(text, "/forget "):
					sysMsg = m.handleForget(strings.TrimPrefix(text, "/forget "))
				case text == "/forget":
					sysMsg = "usage: /forget <memory id>"

				// ── orchestrator + fallback commands ──────────────────────────
				default:
					response, handled := m.sess.Command(text)
					sysMsg = response
					if !handled {
						sysMsg = "this backend doesn't support commands — only orchestrators do\ntry: mesh chat --backend brain"
					}
				}

				m.entries = append(m.entries, entry{
					role: roleSystem,
					raw:  sysMsg,
					at:   time.Now(),
				})
				m.updateViewport()
				m.vp.GotoBottom()
				return m, nil
			}

			// While streaming: queue the message instead of blocking.
			if m.isStreaming() {
				m.queue = append(m.queue, text)
				preview := text
				if len(preview) > 60 {
					preview = preview[:57] + "…"
				}
				m.entries = append(m.entries, entry{
					role: roleSystem,
					raw:  fmt.Sprintf("↑ queued (%d): %q", len(m.queue), preview),
					at:   time.Now(),
				})
				m.updateViewport()
				m.vp.GotoBottom()
				return m, nil
			}

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
				role: roleError,
				raw:  msg.err.Error(),
				at:   time.Now(),
			})
		}
		m.updateViewport()
		m.vp.GotoBottom()

		// Auto-save the exchange to engram (fire-and-forget goroutine).
		if m.mem != nil && msg.err == nil && len(m.entries) >= 2 {
			n := len(m.entries)
			assistantResp := m.entries[n-1].raw
			if assistantResp != "" && msg.userMsg != "" {
				m.memCount++
				mem, sessionID, project, userMsg := m.mem, m.sessionID, m.backendID, msg.userMsg
				go autoSave(mem, sessionID, project, userMsg, assistantResp)
			}
		}

		// Auto-send the next queued message, if any.
		if len(m.queue) > 0 {
			next := m.queue[0]
			m.queue = m.queue[1:]
			cmds = append(cmds, m.startSend(next))
		}

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

	// Recompute autocomplete suggestions after every textarea change.
	m.updateCompletions(m.ta.Value())

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
	parts := m.renderStatusBar() + "\n" +
		m.vp.View() + "\n"
	if len(m.comps) > 0 {
		parts += m.renderCompletions() + "\n"
	}
	return parts + m.ta.View() + "\n" + m.renderHintBar()
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
	vpH := m.height - statusLines - hintLines - taLines - separators - m.completionHeight()
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
		glamour.WithStandardStyle(m.glamourStyle),
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
	// System messages use a compact inline style — no bubble border.
	if e.role == roleSystem {
		return m.renderSystemMsg(e, innerW)
	}

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
		elapsed := formatElapsed(time.Since(m.streamStartedAt))
		spinStr = " " + m.spin.View() + " " + elapsed
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

// renderSystemMsg renders a slash-command response as a compact inline block
// (no rounded border — visually distinct from chat bubbles).
func (m *ChatModel) renderSystemMsg(e entry, width int) string {
	ts := styleTimestamp.Render(e.at.Format("15:04"))
	header := styleSystemLabel.Render("○ mesh") + "  " + ts

	// Each line of the body gets a leading dim bar.
	var bodyLines []string
	for _, line := range strings.Split(e.raw, "\n") {
		bodyLines = append(bodyLines, styleSystemLine.Render("  "+line))
	}
	body := strings.Join(bodyLines, "\n")

	content := header + "\n" + body
	return lipgloss.NewStyle().
		MarginLeft(1).
		MarginBottom(1).
		Width(width + 4).
		Render(content)
}

// ── status bar ────────────────────────────────────────────────────────────────

func (m ChatModel) renderStatusBar() string {
	mn := m.modelName
	if mn == "" {
		mn = "—"
	}
	memPart := ""
	if m.mem != nil {
		memPart = fmt.Sprintf(" · mem:%d", m.memCount)
	}
	left := fmt.Sprintf(" mesh · %s · %s · %d rules%s",
		m.backendType, mn, m.rulesCount, memPart)

	turns := 0
	for _, e := range m.entries {
		if e.role == roleUser {
			turns++
		}
	}
	right := fmt.Sprintf("%d turn(s) ", turns)
	if len(m.queue) > 0 {
		right = fmt.Sprintf("↑ %d queued  ", len(m.queue)) + right
	}

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
		" enter send  ·  / commands  ·  tab complete  ·  shift+enter newline  ·  ↑/↓ scroll  ·  ctrl+l clear  ·  ctrl+c quit",
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

	m.streamStartedAt = time.Now()

	ch := make(chan tea.Msg, 256)
	m.eventCh = ch
	// Capture values for the goroutine — don't hold a reference to m.
	sess := m.sess
	ctx := m.ctx
	mem := m.mem
	project := m.backendID
	sessionID := m.sessionID

	go func() {
		// Search engram for relevant context and inject it into this turn's
		// system prompt. If memory is unavailable or empty the call is a no-op.
		memCtx := buildMemoryContext(mem, text, project)
		_, err := sess.SendWithContext(ctx, memCtx, text, func(chunk string) {
			ch <- chunkMsg(chunk)
		})
		ch <- streamDone{err: err, userMsg: text, sessionID: sessionID}
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

// ── engram memory helpers ─────────────────────────────────────────────────────

// buildMemoryContext searches engram for observations relevant to the user's
// message and formats them as a compact context block to prepend to the system
// prompt. Returns "" when memory is unavailable or nothing matches.
func buildMemoryContext(mem *engstore.Store, query, project string) string {
	if mem == nil || strings.TrimSpace(query) == "" {
		return ""
	}
	results, err := mem.Search(query, project, "project", "", 4, 0)
	if err != nil || len(results) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("── engram memory context ────────────────────────────────\n")
	for _, obs := range results {
		body := obs.Content
		if len(body) > 200 {
			body = body[:197] + "…"
		}
		sb.WriteString(fmt.Sprintf("• [%s] %s: %s\n", obs.Type, obs.Title, body))
	}
	sb.WriteString("─────────────────────────────────────────────────────────")
	return sb.String()
}

// autoSave persists a completed chat exchange as an engram observation.
// Runs in its own goroutine — errors are silently discarded so the UI is
// never blocked or interrupted.
func autoSave(mem *engstore.Store, sessionID, project, userMsg, response string) {
	title := "Q: " + userMsg
	if len(title) > 80 {
		title = title[:77] + "…"
	}
	mem.SaveObservation(engstore.SaveObservationParams{ //nolint:errcheck
		SessionID: sessionID,
		Type:      "response",
		Title:     title,
		Content:   response,
		ToolName:  "mesh",
		Project:   project,
		Scope:     "project",
		TopicKey:  msgTopicKey(userMsg),
	})
}

// msgTopicKey returns a short stable key for deduplicating repeated questions.
func msgTopicKey(s string) string {
	h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(s))))
	return fmt.Sprintf("%x", h[:8])
}

// handleRemember saves a manual note to engram and returns a status string.
func (m *ChatModel) handleRemember(content string) string {
	if m.mem == nil {
		return "memory not available (engram not loaded)"
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return "usage: /remember <text to store>"
	}
	title := content
	if len(title) > 72 {
		title = title[:69] + "…"
	}
	obs, err := m.mem.SaveObservation(engstore.SaveObservationParams{
		SessionID: m.sessionID,
		Type:      "manual",
		Title:     title,
		Content:   content,
		ToolName:  "mesh",
		Project:   m.backendID,
		Scope:     "project",
	})
	if err != nil {
		return "error saving memory: " + err.Error()
	}
	m.memCount++
	return fmt.Sprintf("✓ saved memory #%d: %s", obs.ID, title)
}

// handleRecall searches engram and returns a formatted results string.
func (m *ChatModel) handleRecall(query string) string {
	if m.mem == nil {
		return "memory not available (engram not loaded)"
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return "usage: /recall <search query>"
	}
	results, err := m.mem.Search(query, m.backendID, "project", "", 5, 0)
	if err != nil {
		return "error searching memory: " + err.Error()
	}
	if len(results) == 0 {
		return fmt.Sprintf("no memories found for %q", query)
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%d memory result(s) for %q:\n", len(results), query))
	for _, obs := range results {
		body := obs.Content
		if len(body) > 140 {
			body = body[:137] + "…"
		}
		sb.WriteString(fmt.Sprintf("  #%d [%s] %s\n    %s\n", obs.ID, obs.Type, obs.Title, body))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// handleForget soft-deletes an observation by ID and returns a status string.
func (m *ChatModel) handleForget(idStr string) string {
	if m.mem == nil {
		return "memory not available (engram not loaded)"
	}
	id, err := strconv.ParseInt(strings.TrimSpace(idStr), 10, 64)
	if err != nil {
		return fmt.Sprintf("invalid id %q — usage: /forget <number>", idStr)
	}
	if err := m.mem.DeleteObservation(id, false); err != nil {
		return "error deleting memory: " + err.Error()
	}
	if m.memCount > 0 {
		m.memCount--
	}
	return fmt.Sprintf("✓ forgot memory #%d", id)
}

// ── helpers ───────────────────────────────────────────────────────────────────

// ── autocomplete ──────────────────────────────────────────────────────────────

const maxCompletions = 6

// completionHeight returns the number of terminal lines the completion popup
// currently occupies (0 when hidden, N items + 2 for the border otherwise).
func (m *ChatModel) completionHeight() int {
	n := len(m.comps)
	if n == 0 {
		return 0
	}
	if n > maxCompletions {
		n = maxCompletions
	}
	return n + 2 // +2: top + bottom border
}

// updateCompletions recomputes the suggestion list from the current input.
// When the list size changes it calls applyResize so the viewport adjusts.
func (m *ChatModel) updateCompletions(input string) {
	prev := len(m.comps)
	m.comps = buildSuggestions(input, m.hasCommander)
	// Keep selection in bounds.
	if m.compIdx >= len(m.comps) {
		m.compIdx = -1
	}
	if len(m.comps) != prev && m.ready {
		m.applyResize() // adjusts viewport height; also calls updateViewport
	}
}

// renderCompletions draws the autocomplete popup.
func (m *ChatModel) renderCompletions() string {
	// Inner width: full width minus border(2) minus padding(2) minus margin(1).
	innerW := m.width - 5
	if innerW < 20 {
		innerW = 20
	}

	// Command column: widest command text + 2 padding.
	cmdW := 0
	limit := len(m.comps)
	if limit > maxCompletions {
		limit = maxCompletions
	}
	for _, s := range m.comps[:limit] {
		if w := lipgloss.Width(s.full); w > cmdW {
			cmdW = w
		}
	}
	cmdW += 2

	var rows []string
	for i, s := range m.comps[:limit] {
		// Pad command to fixed width.
		cmdPart := s.full + strings.Repeat(" ", cmdW-len(s.full))
		descPart := s.desc

		// Truncate description if it would overflow.
		descMax := innerW - cmdW - 1
		if descMax < 0 {
			descMax = 0
		}
		if len(descPart) > descMax {
			descPart = descPart[:descMax-1] + "…"
		}

		line := cmdPart + styleCompDesc.Render(descPart)

		if i == m.compIdx {
			// Selected row: highlight entire line.
			rows = append(rows, styleCompSelected.Width(innerW).Render(cmdPart+descPart))
		} else {
			rows = append(rows, styleCompItem.Render(line))
		}
	}

	// Show overflow hint if list was trimmed.
	if len(m.comps) > maxCompletions {
		extra := len(m.comps) - maxCompletions
		rows = append(rows, styleCompDesc.Render(
			fmt.Sprintf("  … %d more (keep typing to narrow)", extra),
		))
	}

	content := strings.Join(rows, "\n")
	return styleCompBox.Width(m.width - 3).Render(content)
}

// ── misc helpers ──────────────────────────────────────────────────────────────

// formatElapsed formats a duration as a compact human-readable string for the
// streaming indicator: "0s", "12s", "1m04s", "2m30s", etc.
func formatElapsed(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%dm%02ds", m, s)
}
