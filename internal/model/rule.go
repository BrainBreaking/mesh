package model

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

// Manifest is the root structure of a steermesh.toml file.
type Manifest struct {
	Project Project `toml:"project"`
	Targets Targets `toml:"targets"`
	Rules   []Rule  `toml:"rule"`
}
