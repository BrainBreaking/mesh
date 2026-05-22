package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/BrainBreaking/mesh/internal/model"
)

const defaultOpenAIBaseURL = "https://api.openai.com/v1"

// OpenAIBackend implements Backend using the OpenAI-compatible REST API (SSE streaming).
type OpenAIBackend struct {
	id      string
	model   string
	baseURL string
	apiKey  string
	client  *http.Client
}

func newOpenAIBackend(cfg *model.Backend) *OpenAIBackend {
	base := cfg.BaseURL
	if base == "" {
		base = defaultOpenAIBaseURL
	}
	base = strings.TrimRight(base, "/")
	return &OpenAIBackend{
		id:      cfg.ID,
		model:   cfg.Model,
		baseURL: base,
		apiKey:  os.ExpandEnv(cfg.APIKey),
		client:  &http.Client{},
	}
}

func (o *OpenAIBackend) ID() string { return o.id }

// openAIMessage is the wire format for OpenAI chat messages.
type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// openAIChatRequest is the POST body for /v1/chat/completions.
type openAIChatRequest struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`
	Stream   bool            `json:"stream"`
}

// openAIStreamChunk is a single SSE data payload (delta-style streaming).
type openAIStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
}

// Chat implements Backend.Chat using OpenAI's SSE /v1/chat/completions endpoint.
func (o *OpenAIBackend) Chat(ctx context.Context, system string, history []Message, userMsg string, stream func(chunk string)) (string, error) {
	msgs := make([]openAIMessage, 0, len(history)+2)
	if system != "" {
		msgs = append(msgs, openAIMessage{Role: "system", Content: system})
	}
	for _, h := range history {
		msgs = append(msgs, openAIMessage{Role: h.Role, Content: h.Content})
	}
	msgs = append(msgs, openAIMessage{Role: "user", Content: userMsg})

	body, err := json.Marshal(openAIChatRequest{
		Model:    o.model,
		Messages: msgs,
		Stream:   true,
	})
	if err != nil {
		return "", fmt.Errorf("openai: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("openai: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if o.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
	}

	resp, err := o.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("openai: HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("openai: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	const prefix = "data: "
	var sb strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		payload := strings.TrimPrefix(line, prefix)
		if payload == "[DONE]" {
			break
		}
		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue // skip malformed lines
		}
		if len(chunk.Choices) > 0 {
			content := chunk.Choices[0].Delta.Content
			if content != "" {
				sb.WriteString(content)
				if stream != nil {
					stream(content)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("openai: reading response: %w", err)
	}

	return sb.String(), nil
}
