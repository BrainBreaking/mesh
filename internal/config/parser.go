package config

import (
	"fmt"
	"os"

	"github.com/BrainBreaking/mesh/internal/model"
	"github.com/BurntSushi/toml"
)

// Load reads and parses a steermesh.toml file into a Manifest.
func Load(path string) (*model.Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var m model.Manifest
	if _, err := toml.Decode(string(data), &m); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	if err := validate(&m); err != nil {
		return nil, err
	}

	return &m, nil
}

var validBackendTypes = map[string]bool{
	"ollama":     true,
	"openai":     true,
	"anthropic":  true,
	"claude":     true,
	"claude-cli": true,
	"codex-cli":  true,
}

func validate(m *model.Manifest) error {
	if m.Project.Name == "" {
		return fmt.Errorf("validation: [project] name is required")
	}
	if len(m.Rules) == 0 {
		return fmt.Errorf("validation: at least one [[rule]] is required")
	}

	// ── rules ────────────────────────────────────────────────────────────────
	seenRules := map[string]bool{}
	for i, r := range m.Rules {
		if r.ID == "" {
			return fmt.Errorf("validation: [[rule]] #%d is missing an id", i+1)
		}
		if seenRules[r.ID] {
			return fmt.Errorf("validation: duplicate rule id %q", r.ID)
		}
		seenRules[r.ID] = true
		if r.Content == "" {
			return fmt.Errorf("validation: rule %q has empty content", r.ID)
		}
	}

	// ── backends ─────────────────────────────────────────────────────────────
	seenBackends := map[string]bool{}
	for i, b := range m.Backends {
		if b.ID == "" {
			return fmt.Errorf("validation: [[backend]] #%d is missing an id", i+1)
		}
		if seenBackends[b.ID] {
			return fmt.Errorf("validation: duplicate backend id %q", b.ID)
		}
		seenBackends[b.ID] = true

		if b.Type == "" {
			return fmt.Errorf("validation: backend %q is missing a type", b.ID)
		}
		if !validBackendTypes[b.Type] {
			return fmt.Errorf("validation: backend %q has unknown type %q (valid: ollama, openai, anthropic, claude-cli, codex-cli)", b.ID, b.Type)
		}

		switch b.Type {
		case "ollama":
			if b.Model == "" {
				return fmt.Errorf("validation: backend %q (ollama) is missing a model", b.ID)
			}
		case "openai", "anthropic", "claude":
			// model has defaults in the backend package, but warn if api_key is
			// completely absent (not even a ${VAR} placeholder)
			if b.APIKey == "" {
				return fmt.Errorf("validation: backend %q (%s) is missing api_key (use \"${ENV_VAR}\" as a placeholder)", b.ID, b.Type)
			}
		}
		// claude-cli and codex-cli require no fields beyond id+type
	}

	return nil
}
