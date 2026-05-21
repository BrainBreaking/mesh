package compiler

import (
	"fmt"
	"os"
	"strings"

	"github.com/BrainBreaking/mesh/internal/model"
)

// CompileOllama writes a Modelfile to the current directory.
// Returns the path of the file written.
func CompileOllama(m *model.Manifest) (string, error) {
	content := buildModelfile(m)
	path := "Modelfile"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("ollama: writing Modelfile: %w", err)
	}
	return path, nil
}

func buildModelfile(m *model.Manifest) string {
	var sb strings.Builder
	cfg := m.Targets.Ollama

	// Base model
	base := cfg.Model
	if base == "" {
		base = "gemma3:4b"
	}
	sb.WriteString(fmt.Sprintf("FROM %s\n\n", base))

	// Parameters
	if cfg.Temperature > 0 {
		sb.WriteString(fmt.Sprintf("PARAMETER temperature %.2f\n", cfg.Temperature))
	}
	if cfg.NumCtx > 0 {
		sb.WriteString(fmt.Sprintf("PARAMETER num_ctx %d\n", cfg.NumCtx))
	}
	sb.WriteString("\n")

	// System prompt — concatenate all rules
	sb.WriteString("SYSTEM \"\"\"\n")
	for i, rule := range m.Rules {
		if i > 0 {
			sb.WriteString("\n---\n\n")
		}
		if rule.Description != "" {
			sb.WriteString(fmt.Sprintf("# %s\n\n", rule.Description))
		}
		sb.WriteString(strings.TrimSpace(rule.Content))
		sb.WriteString("\n")
	}
	sb.WriteString("\"\"\"\n")

	return sb.String()
}
