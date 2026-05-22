package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/BrainBreaking/mesh/internal/model"
)

// AnthropicBackend calls the Anthropic Messages API with SSE streaming.
type AnthropicBackend struct {
	id      string
	model   string
	baseURL string
	apiKey  string
}

func newAnthropicBackend(cfg *model.Backend) (*AnthropicBackend, error) {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	apiKey := os.ExpandEnv(cfg.APIKey)
	if apiKey == "" {
		apiKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("anthropic: no api_key — set ANTHROPIC_API_KEY or add api_key to [[backend]]")
	}
	m := cfg.Model
	if m == "" {
		m = "claude-opus-4-7"
	}
	return &AnthropicBackend{id: cfg.ID, model: m, baseURL: baseURL, apiKey: apiKey}, nil
}

func (b *AnthropicBackend) ID() string { return b.id }

func (b *AnthropicBackend) Chat(ctx context.Context, system string, history []Message, userMsg string, stream func(string)) (string, error) {
	// Build messages array (no system role — Anthropic uses top-level system param)
	msgs := make([]map[string]string, 0, len(history)+1)
	for _, h := range history {
		if h.Role == "system" {
			continue
		}
		msgs = append(msgs, map[string]string{"role": h.Role, "content": h.Content})
	}
	msgs = append(msgs, map[string]string{"role": "user", "content": userMsg})

	body := map[string]any{
		"model":      b.model,
		"max_tokens": 4096,
		"system":     system,
		"messages":   msgs,
		"stream":     true,
	}

	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		b.baseURL+"/v1/messages", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", b.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "interleaved-thinking-2025-05-14")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("anthropic: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errBody map[string]any
		json.NewDecoder(resp.Body).Decode(&errBody)
		return "", fmt.Errorf("anthropic: HTTP %d: %v", resp.StatusCode, errBody)
	}

	var sb strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}

		var evt map[string]any
		if err := json.Unmarshal([]byte(payload), &evt); err != nil {
			continue
		}

		// content_block_delta carries text chunks
		if evt["type"] == "content_block_delta" {
			if delta, ok := evt["delta"].(map[string]any); ok {
				if text, ok := delta["text"].(string); ok {
					stream(text)
					sb.WriteString(text)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return sb.String(), fmt.Errorf("anthropic: reading stream: %w", err)
	}
	return sb.String(), nil
}
