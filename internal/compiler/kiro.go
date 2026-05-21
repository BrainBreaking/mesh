package compiler

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BrainBreaking/mesh/internal/model"
)

// CompileKiro writes one .md file per rule into .kiro/steering/ (or the
// configured output_dir). Returns the list of files written.
func CompileKiro(m *model.Manifest) ([]string, error) {
	outDir := m.Targets.Kiro.OutputDir
	if outDir == "" {
		outDir = ".kiro/steering"
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("kiro: creating output dir: %w", err)
	}

	var written []string
	for _, rule := range m.Rules {
		path := filepath.Join(outDir, rule.ID+".md")
		content := buildKiroFile(rule)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return nil, fmt.Errorf("kiro: writing %s: %w", path, err)
		}
		written = append(written, path)
	}
	return written, nil
}

func buildKiroFile(r model.Rule) string {
	var sb strings.Builder

	// Frontmatter
	sb.WriteString("---\n")
	if r.Description != "" {
		sb.WriteString(fmt.Sprintf("description: %s\n", r.Description))
	}
	if r.AlwaysApply {
		sb.WriteString("alwaysApply: true\n")
	}
	if len(r.Globs) > 0 {
		sb.WriteString("globs:\n")
		for _, g := range r.Globs {
			sb.WriteString(fmt.Sprintf("  - %s\n", g))
		}
	}
	sb.WriteString("---\n\n")

	// Body
	sb.WriteString(strings.TrimSpace(r.Content))
	sb.WriteString("\n")

	return sb.String()
}
