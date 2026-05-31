package chat

import (
	"context"

	"github.com/BrainBreaking/mesh/internal/backend"
)

// HasCommander reports whether the underlying backend supports slash commands.
// The TUI uses this to decide which autocomplete entries to offer.
func (s *Session) HasCommander() bool {
	_, ok := s.Backend.(backend.Commander)
	return ok
}

// Command routes a slash command (e.g. "/strategy dynamic") to the backend
// if it implements the Commander interface.
// Returns (response, true) when the backend handled it, ("", false) otherwise.
func (s *Session) Command(cmd string) (string, bool) {
	c, ok := s.Backend.(backend.Commander)
	if !ok {
		return "", false
	}
	result, err := c.Command(cmd)
	if err != nil {
		return "error: " + err.Error(), true
	}
	return result, true
}

// Session manages a multi-turn conversation with a backend.
type Session struct {
	System  string
	History []backend.Message
	Backend backend.Backend
}

// New creates a new Session using the given backend and system prompt.
func New(b backend.Backend, system string) *Session {
	return &Session{
		System:  system,
		History: []backend.Message{},
		Backend: b,
	}
}

// Send appends the user message to history, calls the backend, appends the
// assistant response to history, and returns the full response string.
func (s *Session) Send(ctx context.Context, userMsg string, stream func(chunk string)) (string, error) {
	return s.SendWithContext(ctx, "", userMsg, stream)
}

// SendWithContext is like Send but prepends systemExtra to the system prompt
// for this turn only (the extra content is not stored in history). Used by
// the TUI to inject memory-retrieved context without polluting the session.
func (s *Session) SendWithContext(ctx context.Context, systemExtra, userMsg string, stream func(chunk string)) (string, error) {
	s.History = append(s.History, backend.Message{Role: "user", Content: userMsg})

	system := s.System
	if systemExtra != "" {
		system = systemExtra + "\n\n" + s.System
	}

	reply, err := s.Backend.Chat(ctx, system, s.History[:len(s.History)-1], userMsg, stream)
	if err != nil {
		s.History = s.History[:len(s.History)-1]
		return "", err
	}

	s.History = append(s.History, backend.Message{Role: "assistant", Content: reply})
	return reply, nil
}
