package chat

import (
	"context"

	"github.com/BrainBreaking/mesh/internal/backend"
)

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
	s.History = append(s.History, backend.Message{Role: "user", Content: userMsg})

	reply, err := s.Backend.Chat(ctx, s.System, s.History[:len(s.History)-1], userMsg, stream)
	if err != nil {
		// Remove the user message we just appended so the caller can retry.
		s.History = s.History[:len(s.History)-1]
		return "", err
	}

	s.History = append(s.History, backend.Message{Role: "assistant", Content: reply})
	return reply, nil
}
