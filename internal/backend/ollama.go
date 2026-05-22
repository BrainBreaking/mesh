package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/BrainBreaking/mesh/internal/model"
)

const defaultOllamaBaseURL = "http://localhost:11434"

// OllamaBackend implements Backend using the Ollama REST API.
type OllamaBackend struct {
	id      string
	model   string
	baseURL string
	client  *http.Client
}

func newOllamaBackend(cfg *model.Backend) *OllamaBackend {
	base := cfg.BaseURL
	if base == "" {
		base = defaultOllamaBaseURL
	}
	base = strings.TrimRight(base, "/")
	return &OllamaBackend{
		id:      cfg.ID,
		model:   cfg.Model,
		baseURL: base,
		client:  &http.Client{},
	}
}

func (o *OllamaBackend) ID() string { return o.id }

// ollamaMessage is the wire format for Ollama's /api/chat messages array.
type ollamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ollamaChatRequest is the POST body for /api/chat.
type ollamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Stream   bool            `json:"stream"`
}

// ollamaChatChunk is a single NDJSON line from the streaming response.
type ollamaChatChunk struct {
	Message ollamaMessage `json:"message"`
	Done    bool          `json:"done"`
}

// Chat implements Backend.Chat using Ollama's streaming /api/chat endpoint.
func (o *OllamaBackend) Chat(ctx context.Context, system string, history []Message, userMsg string, stream func(chunk string)) (string, error) {
	msgs := make([]ollamaMessage, 0, len(history)+2)
	if system != "" {
		msgs = append(msgs, ollamaMessage{Role: "system", Content: system})
	}
	for _, h := range history {
		msgs = append(msgs, ollamaMessage{Role: h.Role, Content: h.Content})
	}
	msgs = append(msgs, ollamaMessage{Role: "user", Content: userMsg})

	body, err := json.Marshal(ollamaChatRequest{
		Model:    o.model,
		Messages: msgs,
		Stream:   true,
	})
	if err != nil {
		return "", fmt.Errorf("ollama: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("ollama: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama: HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ollama: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	var sb strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var chunk ollamaChatChunk
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue // skip malformed lines
		}
		if chunk.Message.Content != "" {
			sb.WriteString(chunk.Message.Content)
			if stream != nil {
				stream(chunk.Message.Content)
			}
		}
		if chunk.Done {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("ollama: reading response: %w", err)
	}

	return sb.String(), nil
}
