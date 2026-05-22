package model

import "strings"

// Project holds top-level metadata.
type Project struct {
	Name    string `toml:"name"`
	Version string `toml:"version"`
}

// OllamaTarget holds Ollama-specific compile settings.
type OllamaTarget struct {
	Model       string  `toml:"model"`
	Temperature float64 `toml:"temperature"`
	NumCtx      int     `toml:"num_ctx"`
}

// KiroTarget holds Kiro-specific compile settings.
type KiroTarget struct {
	OutputDir string `toml:"output_dir"` // default: .kiro/steering
}

// Targets groups all supported compile targets.
type Targets struct {
	Ollama OllamaTarget `toml:"ollama"`
	Kiro   KiroTarget   `toml:"kiro"`
}

// Rule is a single steering rule definition.
type Rule struct {
	ID          string   `toml:"id"`
	Description string   `toml:"description"`
	AlwaysApply bool     `toml:"always_apply"`
	Globs       []string `toml:"globs"`
	Content     string   `toml:"content"`
}

// Backend holds the configuration for a runtime AI backend.
type Backend struct {
	ID      string `toml:"id"`
	Type    string `toml:"type"`     // "ollama" | "openai"
	Model   string `toml:"model"`
	BaseURL string `toml:"base_url"` // optional; defaults applied in backend package
	APIKey  string `toml:"api_key"`  // may contain ${ENV_VAR} references
}

// Manifest is the root structure of a steermesh.toml file.
type Manifest struct {
	Project  Project   `toml:"project"`
	Targets  Targets   `toml:"targets"`
	Rules    []Rule    `toml:"rule"`
	Backends []Backend `toml:"backend"`
}

// SystemPrompt concatenates all rule contents into a single system prompt string,
// separated by "\n---\n", matching the Ollama Modelfile SYSTEM block convention.
func (m *Manifest) SystemPrompt() string {
	parts := make([]string, 0, len(m.Rules))
	for _, r := range m.Rules {
		parts = append(parts, strings.TrimSpace(r.Content))
	}
	return strings.Join(parts, "\n---\n")
}

// BackendByID returns the Backend with the given id, or (nil, false).
func (m *Manifest) BackendByID(id string) (*Backend, bool) {
	for i := range m.Backends {
		if m.Backends[i].ID == id {
			return &m.Backends[i], true
		}
	}
	return nil, false
}

// DefaultBackend returns the first configured backend, or (nil, false).
func (m *Manifest) DefaultBackend() (*Backend, bool) {
	if len(m.Backends) == 0 {
		return nil, false
	}
	return &m.Backends[0], true
}
