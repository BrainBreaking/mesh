// Package doctor implements the "mesh doctor" health-check command.
//
// Each configured backend is checked concurrently. Results are printed
// in manifest order with ✓ / ⚠ / ✗ symbols and one-line fix hints.
package doctor

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BrainBreaking/mesh/internal/model"
)

// ─── result types ─────────────────────────────────────────────────────────────

// Level classifies a check outcome.
type Level int

const (
	OK   Level = iota // ✓
	Warn              // ⚠
	Fail              // ✗
)

// Result holds the outcome of a single backend check.
type Result struct {
	BackendID string
	Type      string
	Model     string
	Level     Level
	Detail    string // human-readable status
	Fix       string // one-line remediation hint (empty if OK/Warn with no action)
}

func (r Result) icon() string {
	switch r.Level {
	case OK:
		return "✓"
	case Warn:
		return "⚠"
	default:
		return "✗"
	}
}

// ─── Run ─────────────────────────────────────────────────────────────────────

// Run checks every backend in the manifest concurrently and returns results
// in the original manifest order.
func Run(m *model.Manifest) []Result {
	results := make([]Result, len(m.Backends))
	var wg sync.WaitGroup

	for i, bc := range m.Backends {
		i, bc := i, bc
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = checkBackend(bc)
		}()
	}

	wg.Wait()
	return results
}

// ─── Print ────────────────────────────────────────────────────────────────────

// Print writes a formatted doctor report to w.
func Print(w io.Writer, manifestPath string, m *model.Manifest, results []Result) {
	fmt.Fprintf(w, "\nmesh doctor — checking your setup\n\n")

	// manifest line
	fmt.Fprintf(w, "  ✓  Manifest      %s — %d rule(s), %d backend(s)\n\n",
		manifestPath, len(m.Rules), len(m.Backends))

	// column widths
	maxID, maxType, maxModel := 0, 0, 0
	for _, r := range results {
		if len(r.BackendID) > maxID {
			maxID = len(r.BackendID)
		}
		if len(r.Type) > maxType {
			maxType = len(r.Type)
		}
		if len(r.Model) > maxModel {
			maxModel = len(r.Model)
		}
	}

	fmt.Fprintf(w, "  Backends\n")
	errors, warns := 0, 0
	var fixes []string

	for _, r := range results {
		idPad := pad(r.BackendID, maxID)
		typePad := pad(r.Type, maxType)
		modelPad := pad(r.Model, maxModel)

		fmt.Fprintf(w, "  %s  %s  %s  %s  %s\n",
			r.icon(), idPad, typePad, modelPad, r.Detail)

		if r.Fix != "" {
			fixes = append(fixes, fmt.Sprintf("  %s: %s", r.BackendID, r.Fix))
		}
		switch r.Level {
		case Fail:
			errors++
		case Warn:
			warns++
		}
	}

	fmt.Fprintln(w)

	if errors == 0 && warns == 0 {
		fmt.Fprintf(w, "  All %d backends healthy\n", len(results))
	} else {
		parts := []string{fmt.Sprintf("%d checked", len(results))}
		if errors > 0 {
			parts = append(parts, fmt.Sprintf("%d error(s)", errors))
		}
		if warns > 0 {
			parts = append(parts, fmt.Sprintf("%d warning(s)", warns))
		}
		fmt.Fprintf(w, "  %s\n", strings.Join(parts, " · "))
	}

	if len(fixes) > 0 {
		fmt.Fprintf(w, "\n  Fixes:\n")
		for _, f := range fixes {
			fmt.Fprintf(w, "    %s\n", f)
		}
	}

	fmt.Fprintln(w)
}

// ─── per-backend checks ───────────────────────────────────────────────────────

func checkBackend(bc model.Backend) Result {
	model := bc.Model
	if model == "" {
		model = "—" // placeholder for CLI backends with no model field
	}
	r := Result{
		BackendID: bc.ID,
		Type:      bc.Type,
		Model:     model,
	}

	switch bc.Type {
	case "ollama":
		checkOllama(&r, bc)
	case "openai":
		checkAPIKey(&r, bc, "OPENAI_API_KEY")
	case "anthropic", "claude":
		checkAPIKey(&r, bc, "ANTHROPIC_API_KEY")
	case "claude-cli":
		checkClaudeCLI(&r)
	case "codex-cli":
		checkCodexCLI(&r)
	default:
		r.Level = Warn
		r.Detail = fmt.Sprintf("unknown type %q — skipped", bc.Type)
	}

	return r
}

// ── ollama ────────────────────────────────────────────────────────────────────

func checkOllama(r *Result, bc model.Backend) {
	baseURL := bc.BaseURL
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	baseURL = strings.TrimRight(baseURL, "/")

	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get(baseURL + "/api/tags")
	if err != nil {
		r.Level = Fail
		r.Detail = fmt.Sprintf("unreachable (%s)", shortErr(err))
		r.Fix = fmt.Sprintf("start Ollama or check base_url %q", baseURL)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		r.Level = Fail
		r.Detail = fmt.Sprintf("HTTP %d from /api/tags", resp.StatusCode)
		return
	}

	// parse model list
	var body struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		r.Level = Warn
		r.Detail = "reachable, but couldn't parse /api/tags"
		return
	}

	found := false
	for _, m := range body.Models {
		if m.Name == bc.Model || strings.HasPrefix(m.Name, bc.Model+":") {
			found = true
			break
		}
	}

	if !found {
		r.Level = Warn
		r.Detail = fmt.Sprintf("reachable, model %q not found", bc.Model)
		r.Fix = fmt.Sprintf("ollama pull %s", bc.Model)
		return
	}

	r.Level = OK
	r.Detail = fmt.Sprintf("reachable, model found (%s)", shortHost(baseURL))
}

// ── openai / anthropic ────────────────────────────────────────────────────────

func checkAPIKey(r *Result, bc model.Backend, envFallback string) {
	key := os.ExpandEnv(bc.APIKey)
	if key == "" {
		key = os.Getenv(envFallback)
	}

	if key == "" {
		r.Level = Fail
		r.Detail = fmt.Sprintf("%s not set", envFallback)
		r.Fix = fmt.Sprintf("export %s=<your-key>", envFallback)
		return
	}

	// Mask all but first 8 chars for display
	masked := key
	if len(key) > 8 {
		masked = key[:8] + strings.Repeat("*", len(key)-8)
	}
	r.Level = OK
	r.Detail = fmt.Sprintf("%s set (%s)", envFallback, masked)
}

// ── claude-cli ────────────────────────────────────────────────────────────────

func checkClaudeCLI(r *Result) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		r.Level = Fail
		r.Detail = "claude not found in PATH"
		r.Fix = "install Claude Code: https://claude.ai/download"
		return
	}

	// Run `claude --version` — fast, no API call, exits 0 only if auth is usable.
	// Under --bare mode it would fail; use default mode with a tight timeout.
	cmd := exec.Command(bin, "--version")
	cmd.Stderr = nil
	out, err := runWithTimeout(cmd, 5*time.Second)
	if err != nil {
		r.Level = Warn
		r.Detail = fmt.Sprintf("found at %s, but --version failed: %s", bin, shortErr(err))
		r.Fix = "run: claude login"
		return
	}

	version := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])

	// Check keychain / OAuth: try a no-op `claude --print "" --output-format text`
	// that will immediately fail with "Not logged in" if unauthenticated.
	// Too slow for doctor; instead check for any session file as a heuristic.
	home, _ := os.UserHomeDir()
	sessDir := filepath.Join(home, ".claude", "projects")
	if _, err := os.Stat(sessDir); os.IsNotExist(err) {
		r.Level = Warn
		r.Detail = fmt.Sprintf("found (%s), no prior sessions — may need login", version)
		r.Fix = "run: claude login"
		return
	}

	r.Level = OK
	r.Detail = fmt.Sprintf("found at %s, %s", bin, version)
}

// ── codex-cli ────────────────────────────────────────────────────────────────

func checkCodexCLI(r *Result) {
	bin, err := exec.LookPath("codex")
	if err != nil {
		r.Level = Fail
		r.Detail = "codex not found in PATH"
		r.Fix = "install Codex CLI: npm install -g @openai/codex"
		return
	}

	// Read ~/.codex/auth.json — present and has tokens = authenticated.
	home, _ := os.UserHomeDir()
	authPath := filepath.Join(home, ".codex", "auth.json")
	data, err := os.ReadFile(authPath)
	if err != nil {
		r.Level = Warn
		r.Detail = fmt.Sprintf("found at %s, auth.json missing", bin)
		r.Fix = "run: codex login"
		return
	}

	var auth struct {
		AuthMode string `json:"auth_mode"`
		Tokens   any    `json:"tokens"`
	}
	if err := json.Unmarshal(data, &auth); err != nil || auth.Tokens == nil {
		r.Level = Warn
		r.Detail = fmt.Sprintf("found at %s, not authenticated", bin)
		r.Fix = "run: codex login"
		return
	}

	mode := auth.AuthMode
	if mode == "" {
		mode = "unknown"
	}
	r.Level = OK
	r.Detail = fmt.Sprintf("found at %s, authenticated (%s)", bin, mode)
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func pad(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

func shortHost(baseURL string) string {
	s := strings.TrimPrefix(baseURL, "http://")
	s = strings.TrimPrefix(s, "https://")
	return s
}

func shortErr(err error) string {
	msg := err.Error()
	// trim noisy "dial tcp" prefix
	if i := strings.Index(msg, "connect: "); i != -1 {
		return msg[i+9:]
	}
	if i := strings.Index(msg, ": "); i != -1 {
		return msg[i+2:]
	}
	return msg
}

// runWithTimeout runs cmd and returns stdout, killing it after d if it hasn't finished.
func runWithTimeout(cmd *exec.Cmd, d time.Duration) (string, error) {
	done := make(chan struct{})
	var out []byte
	var runErr error

	go func() {
		out, runErr = cmd.Output()
		close(done)
	}()

	select {
	case <-done:
		return string(out), runErr
	case <-time.After(d):
		cmd.Process.Kill()
		return "", fmt.Errorf("timed out after %s", d)
	}
}
