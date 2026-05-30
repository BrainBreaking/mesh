package backend

import (
	"context"
	"fmt"

	"github.com/BrainBreaking/mesh/internal/model"
)

// Message represents a single chat message with a role and content.
type Message struct {
	Role    string // "user", "assistant", "system"
	Content string
}

// Backend is the interface every AI backend must implement.
type Backend interface {
	// ID returns the backend's configured id string.
	ID() string
	// Chat sends a conversation to the backend, streaming chunks via the stream
	// callback, and returns the full concatenated response.
	Chat(ctx context.Context, system string, history []Message, userMsg string, stream func(chunk string)) (string, error)
}

// Commander is an optional interface for backends that support runtime slash
// commands (e.g. "/strategy dynamic", "/workers").  The TUI detects whether
// the active backend implements this interface and routes "/" prefixed input
// here instead of sending it to the model.
type Commander interface {
	// Command handles a slash command and returns a human-readable response.
	// cmd includes the leading slash, e.g. "/strategy round-robin".
	Command(cmd string) (string, error)
}

// New constructs the correct Backend implementation for the given config.
func New(cfg *model.Backend) (Backend, error) {
	switch cfg.Type {
	case "ollama":
		return newOllamaBackend(cfg), nil
	case "openai":
		return newOpenAIBackend(cfg), nil
	case "anthropic", "claude":
		return newAnthropicBackend(cfg)
	case "claude-cli", "codex-cli":
		return newCLIBackend(cfg)
	default:
		return nil, fmt.Errorf("unknown backend type %q (supported: ollama, openai, anthropic, claude-cli, codex-cli)", cfg.Type)
	}
}
