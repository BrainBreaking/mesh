package backend

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/BrainBreaking/mesh/internal/model"
)

// ─── binary discovery (cached per process) ─────────────────────────────────

var (
	claudeBinOnce sync.Once
	claudeBin     string

	codexBinOnce sync.Once
	codexBin     string
)

func findCLI(name string) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s not found in PATH — install it first", name)
	}
	return p, nil
}

// ─── CLIBackend ────────────────────────────────────────────────────────────

// CLIBackend delegates inference to a locally installed CLI tool
// (claude Code or codex) by spawning it as a subprocess.
//
// Supported types: "claude-cli", "codex-cli"
type CLIBackend struct {
	id      string
	cliType string // "claude-cli" | "codex-cli"
	model   string // optional model override passed to the CLI
}

func newCLIBackend(cfg *model.Backend) (*CLIBackend, error) {
	switch cfg.Type {
	case "claude-cli":
		claudeBinOnce.Do(func() {
			claudeBin, _ = findCLI("claude")
		})
		if claudeBin == "" {
			return nil, fmt.Errorf("claude-cli: claude binary not found in PATH")
		}
	case "codex-cli":
		codexBinOnce.Do(func() {
			codexBin, _ = findCLI("codex")
		})
		if codexBin == "" {
			return nil, fmt.Errorf("codex-cli: codex binary not found in PATH")
		}
	default:
		return nil, fmt.Errorf("cliBackend: unknown type %q", cfg.Type)
	}
	return &CLIBackend{id: cfg.ID, cliType: cfg.Type, model: cfg.Model}, nil
}

func (b *CLIBackend) ID() string { return b.id }

// Chat spawns the CLI subprocess, streams text chunks via the callback,
// and returns the full response.
func (b *CLIBackend) Chat(
	ctx context.Context,
	system string,
	history []Message,
	userMsg string,
	stream func(string),
) (string, error) {
	switch b.cliType {
	case "claude-cli":
		return b.runClaude(ctx, system, history, userMsg, stream)
	case "codex-cli":
		return b.runCodex(ctx, system, history, userMsg, stream)
	default:
		return "", fmt.Errorf("cliBackend: unknown type %q", b.cliType)
	}
}

// ─── claude Code ───────────────────────────────────────────────────────────
//
// Invocation:
//
//	claude -p "<userMsg>" \
//	       --output-format stream-json \
//	       --append-system-prompt "<rules>" \
//	       [--model <model>]
//
// Stream-JSON line shapes we care about:
//
//	{"type":"assistant","message":{"content":[{"type":"text","text":"..."}],...}}
//	{"type":"result","subtype":"success","result":"<full text>",...}

func (b *CLIBackend) runClaude(
	ctx context.Context,
	system string,
	history []Message,
	userMsg string,
	stream func(string),
) (string, error) {
	// Build the prompt: prepend history as a lightweight context block,
	// then the actual user message.
	prompt := buildPromptWithHistory(history, userMsg)

	args := []string{
		"-p", prompt,
		"--output-format", "stream-json",
		"--verbose",                // required for stream-json in --print mode
		"--no-session-persistence", // don't write session files for one-shot calls
	}
	if system != "" {
		args = append(args, "--append-system-prompt", system)
	}
	if b.model != "" {
		args = append(args, "--model", b.model)
	}

	cmd := exec.CommandContext(ctx, claudeBin, args...)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("claude-cli: stdout pipe: %w", err)
	}
	// Discard stderr — claude writes AVX warnings and interactive hints there
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("claude-cli: start: %w", err)
	}

	var sb strings.Builder
	var finalResult string

	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 1<<20), 1<<20) // 1 MB lines

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var evt map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}

		evtType := strings.Trim(string(evt["type"]), `"`)

		switch evtType {
		case "assistant":
			// {"type":"assistant","message":{"content":[{"type":"text","text":"..."}]}}
			text := extractClaudeText(evt["message"])
			if text != "" {
				stream(text)
				sb.WriteString(text)
			}

		case "result":
			// {"type":"result","subtype":"success","result":"full response"}
			// Use this as the canonical full response (deduplicated by claude)
			var result string
			if err := json.Unmarshal(evt["result"], &result); err == nil {
				finalResult = result
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return sb.String(), fmt.Errorf("claude-cli: reading output: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		// context cancelled is expected on timeout/interrupt
		if ctx.Err() != nil {
			return sb.String(), ctx.Err()
		}
		return sb.String(), fmt.Errorf("claude-cli: process: %w", err)
	}

	if finalResult != "" {
		return finalResult, nil
	}
	return sb.String(), nil
}

// extractClaudeText pulls the first text chunk from a claude "message" JSON blob:
// {"content":[{"type":"text","text":"..."}], ...}
func extractClaudeText(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var msg struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, c := range msg.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}

// ─── Codex CLI ─────────────────────────────────────────────────────────────
//
// Invocation:
//
//	codex exec "<prompt>" --json [--model <model>]
//
// JSONL event shapes (--json flag):
//
//	{"type":"message","role":"assistant","content":"chunk..."}
//	{"type":"message","role":"assistant","content":"","stop_reason":"stop"}
//
// If no delta events, falls back to reading plain text lines.

func (b *CLIBackend) runCodex(
	ctx context.Context,
	system string,
	history []Message,
	userMsg string,
	stream func(string),
) (string, error) {
	prompt := buildPromptWithHistory(history, userMsg)
	if system != "" {
		prompt = "# Context\n" + system + "\n\n---\n\n" + prompt
	}

	args := []string{"exec", prompt, "--json"}
	if b.model != "" {
		args = append(args, "--model", b.model)
	}

	cmd := exec.CommandContext(ctx, codexBin, args...)

	// Close stdin immediately — codex will otherwise wait for piped input.
	cmd.Stdin = strings.NewReader("")

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("codex-cli: stdout pipe: %w", err)
	}
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("codex-cli: start: %w", err)
	}

	var sb strings.Builder

	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		// codex --json JSONL event shapes:
		//   {"type":"item.completed","item":{"type":"agent_message","text":"..."}}
		//   {"type":"thread.started"}, {"type":"turn.started"}, {"type":"turn.completed",...}
		var evt struct {
			Type string `json:"type"`
			Item *struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}

		if evt.Type == "item.completed" && evt.Item != nil && evt.Item.Type == "agent_message" {
			text := evt.Item.Text
			if text != "" {
				stream(text)
				sb.WriteString(text)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return sb.String(), fmt.Errorf("codex-cli: reading output: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return sb.String(), ctx.Err()
		}
		return sb.String(), fmt.Errorf("codex-cli: process: %w", err)
	}

	return sb.String(), nil
}

// ─── helpers ───────────────────────────────────────────────────────────────

// buildPromptWithHistory assembles prior conversation turns into a plain-text
// prefix so CLIs that don't have a history parameter still have context.
func buildPromptWithHistory(history []Message, userMsg string) string {
	if len(history) == 0 {
		return userMsg
	}
	var sb strings.Builder
	sb.WriteString("# Prior conversation\n\n")
	for _, m := range history {
		role := strings.ToUpper(m.Role)
		sb.WriteString(role + ": " + m.Content + "\n\n")
	}
	sb.WriteString("# Current message\n\n")
	sb.WriteString(userMsg)
	return sb.String()
}
