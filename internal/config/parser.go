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

func validate(m *model.Manifest) error {
	if m.Project.Name == "" {
		return fmt.Errorf("validation: [project] name is required")
	}
	if len(m.Rules) == 0 {
		return fmt.Errorf("validation: at least one [[rule]] is required")
	}
	seen := map[string]bool{}
	for i, r := range m.Rules {
		if r.ID == "" {
			return fmt.Errorf("validation: [[rule]] #%d is missing an id", i+1)
		}
		if seen[r.ID] {
			return fmt.Errorf("validation: duplicate rule id %q", r.ID)
		}
		seen[r.ID] = true
		if r.Content == "" {
			return fmt.Errorf("validation: rule %q has empty content", r.ID)
		}
	}
	return nil
}
