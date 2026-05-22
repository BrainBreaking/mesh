package doctor

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/BrainBreaking/mesh/internal/model"
)

// ─── ANSI helpers ─────────────────────────────────────────────────────────────

const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiRed    = "\033[31m"
	ansiCyan   = "\033[36m"

	cursorUp    = "\033[%dA" // move cursor up N lines
	eraseLine   = "\033[2K"  // erase entire current line
	cursorStart = "\r"       // move to start of line
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// IsTTY reports whether stdout is a real terminal.
func IsTTY() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

func colored(color, s string) string { return color + s + ansiReset }
func bold(s string) string           { return ansiBold + s + ansiReset }
func dim(s string) string            { return ansiDim + s + ansiReset }

func levelColor(l Level) string {
	switch l {
	case OK:
		return ansiGreen
	case Warn:
		return ansiYellow
	default:
		return ansiRed
	}
}

func levelIcon(l Level) string {
	switch l {
	case OK:
		return colored(ansiGreen, "✓")
	case Warn:
		return colored(ansiYellow, "⚠")
	default:
		return colored(ansiRed, "✗")
	}
}

// ─── row state ────────────────────────────────────────────────────────────────

type rowState struct {
	id    string
	typ   string
	model string
	done  bool
	r     Result
}

func (rs *rowState) render(frame int, idW, typeW, modelW int) string {
	idPad := pad(rs.id, idW)
	typPad := pad(rs.typ, typeW)
	modPad := pad(rs.model, modelW)

	if !rs.done {
		spinner := colored(ansiCyan, spinnerFrames[frame%len(spinnerFrames)])
		return fmt.Sprintf("  %s  %s  %s  %s  %s",
			spinner,
			bold(idPad),
			dim(typPad),
			dim(modPad),
			dim("checking..."),
		)
	}

	detail := rs.r.Detail
	fix := ""
	if rs.r.Fix != "" {
		fix = "  " + dim("→  "+rs.r.Fix)
	}

	return fmt.Sprintf("  %s  %s  %s  %s  %s%s",
		levelIcon(rs.r.Level),
		colored(levelColor(rs.r.Level)+ansiBold, idPad),
		dim(typPad),
		dim(modPad),
		detail,
		fix,
	)
}

// ─── LivePrint ────────────────────────────────────────────────────────────────

// LivePrint runs all checks and renders a live animated UI to stdout.
// Falls back to plain Print when stdout is not a TTY.
func LivePrint(manifestPath string, m *model.Manifest) []Result {
	if !IsTTY() {
		results := Run(m)
		Print(os.Stdout, manifestPath, m, results)
		return results
	}
	return livePrintTTY(os.Stdout, manifestPath, m)
}

func livePrintTTY(w io.Writer, manifestPath string, m *model.Manifest) []Result {
	// ── build initial row states ───────────────────────────────────────────
	rows := make([]*rowState, len(m.Backends))
	maxID, maxType, maxModel := 0, 0, 0

	for i, bc := range m.Backends {
		mdl := bc.Model
		if mdl == "" {
			mdl = "—"
		}
		rows[i] = &rowState{id: bc.ID, typ: bc.Type, model: mdl}
		if len(bc.ID) > maxID {
			maxID = len(bc.ID)
		}
		if len(bc.Type) > maxType {
			maxType = len(bc.Type)
		}
		if len(mdl) > maxModel {
			maxModel = len(mdl)
		}
	}

	// ── print header (static, never redrawn) ─────────────────────────────
	fmt.Fprintf(w, "\n%s — %s\n\n",
		bold("mesh doctor"),
		dim("checking your setup"),
	)
	fmt.Fprintf(w, "  %s  %s — %d rule(s), %d backend(s)\n\n",
		colored(ansiGreen, "✓"),
		bold("Manifest    "+manifestPath),
		len(m.Rules), len(m.Backends),
	)
	fmt.Fprintf(w, "  %s\n", bold("Backends"))

	// ── print initial rows (all "checking...") ───────────────────────────
	for _, row := range rows {
		fmt.Fprintf(w, "%s\n", row.render(0, maxID, maxType, maxModel))
	}

	nRows := len(rows) // number of data lines to redraw

	// ── launch checks concurrently ────────────────────────────────────────
	type indexed struct {
		i int
		r Result
	}
	ch := make(chan indexed, len(rows))

	for i, bc := range m.Backends {
		i, bc := i, bc
		go func() {
			ch <- indexed{i, checkBackend(bc)}
		}()
	}

	// ── render loop ───────────────────────────────────────────────────────
	var mu sync.Mutex
	frame := 0
	done := 0
	total := len(rows)

	redraw := func() {
		// move cursor up to first data row
		fmt.Fprintf(w, cursorUp, nRows)
		for _, row := range rows {
			fmt.Fprintf(w, "%s%s%s\n", cursorStart, eraseLine,
				row.render(frame, maxID, maxType, maxModel))
		}
	}

	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()

	results := make([]Result, len(rows))

loop:
	for {
		select {
		case res := <-ch:
			mu.Lock()
			rows[res.i].done = true
			rows[res.i].r = res.r
			results[res.i] = res.r
			done++
			redraw()
			mu.Unlock()
			if done == total {
				break loop
			}

		case <-ticker.C:
			mu.Lock()
			frame++
			redraw()
			mu.Unlock()
		}
	}

	// ── summary ───────────────────────────────────────────────────────────
	errors, warns := 0, 0
	var fixes []string

	for _, r := range results {
		switch r.Level {
		case Fail:
			errors++
			if r.Fix != "" {
				fixes = append(fixes, fmt.Sprintf("  %s  %s", pad(r.BackendID, maxID), colored(ansiCyan, r.Fix)))
			}
		case Warn:
			warns++
			if r.Fix != "" {
				fixes = append(fixes, fmt.Sprintf("  %s  %s", pad(r.BackendID, maxID), colored(ansiYellow, r.Fix)))
			}
		}
	}

	fmt.Fprintln(w)

	if errors == 0 && warns == 0 {
		fmt.Fprintf(w, "  %s  All %d backends healthy\n",
			colored(ansiGreen, "✓"), total)
	} else {
		parts := []string{fmt.Sprintf("%d checked", total)}
		if errors > 0 {
			parts = append(parts, colored(ansiRed, fmt.Sprintf("%d error(s)", errors)))
		}
		if warns > 0 {
			parts = append(parts, colored(ansiYellow, fmt.Sprintf("%d warning(s)", warns)))
		}
		fmt.Fprintf(w, "  %s\n", strings.Join(parts, dim(" · ")))
	}

	if len(fixes) > 0 {
		fmt.Fprintf(w, "\n  %s\n", bold("Fixes"))
		for _, f := range fixes {
			fmt.Fprintf(w, "    %s\n", f)
		}
	}

	fmt.Fprintln(w)
	return results
}
