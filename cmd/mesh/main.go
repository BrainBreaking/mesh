package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/BrainBreaking/mesh/internal/backend"
	"github.com/BrainBreaking/mesh/internal/chat"
	"github.com/BrainBreaking/mesh/internal/compiler"
	"github.com/BrainBreaking/mesh/internal/config"
	"github.com/BrainBreaking/mesh/internal/doctor"
	"github.com/BrainBreaking/mesh/internal/model"
	"github.com/BrainBreaking/mesh/internal/server"
	"github.com/BrainBreaking/mesh/internal/tui"
)

var rootCmd = &cobra.Command{
	Use:   "mesh",
	Short: "SteerMesh compiler — define AI steering rules once, compile everywhere",
	Long: `mesh compiles a steermesh.toml manifest into target-specific rule formats.

Supported targets: kiro, ollama`,
}

// ── compile ───────────────────────────────────────────────────────────────────

var compileTarget string

var compileCmd = &cobra.Command{
	Use:   "compile [manifest]",
	Short: "Compile steermesh.toml to target formats",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		manifestPath := "steermesh.toml"
		if len(args) > 0 {
			manifestPath = args[0]
		}

		m, err := config.Load(manifestPath)
		if err != nil {
			return fmt.Errorf("load: %w", err)
		}

		runKiro := compileTarget == "kiro" || compileTarget == "all"
		runOllama := compileTarget == "ollama" || compileTarget == "all"

		if runKiro {
			files, err := compiler.CompileKiro(m)
			if err != nil {
				return fmt.Errorf("kiro: %w", err)
			}
			fmt.Printf("✓  kiro    — %d rule(s) written\n", len(files))
			for _, f := range files {
				fmt.Printf("   %s\n", f)
			}
		}

		if runOllama {
			path, err := compiler.CompileOllama(m)
			if err != nil {
				return fmt.Errorf("ollama: %w", err)
			}
			fmt.Printf("✓  ollama  — %s written\n", path)
		}

		if !runKiro && !runOllama {
			return fmt.Errorf("unknown target %q — valid: kiro, ollama, all", compileTarget)
		}

		return nil
	},
}

// ── validate ──────────────────────────────────────────────────────────────────

var validateCmd = &cobra.Command{
	Use:   "validate [manifest]",
	Short: "Validate a steermesh.toml without writing any files",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		manifestPath := "steermesh.toml"
		if len(args) > 0 {
			manifestPath = args[0]
		}

		m, err := config.Load(manifestPath)
		if err != nil {
			return err
		}

		fmt.Printf("✓  %s is valid — %d rule(s), project: %s %s\n",
			manifestPath, len(m.Rules), m.Project.Name, m.Project.Version)
		return nil
	},
}

// ── init ──────────────────────────────────────────────────────────────────────

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Scaffold a new steermesh.toml in the current directory",
	RunE: func(cmd *cobra.Command, args []string) error {
		const path = "steermesh.toml"
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists", path)
		}

		if err := os.WriteFile(path, []byte(exampleManifest), 0o644); err != nil {
			return err
		}

		fmt.Printf("✓  %s created — edit it, then run: mesh compile\n", path)
		return nil
	},
}

const exampleManifest = `[project]
name    = "my-project"
version = "1.0.0"

[targets.ollama]
model       = "gemma3:4b"
temperature = 0.7
num_ctx     = 8192

[targets.kiro]
output_dir = ".kiro/steering"

[[backend]]
id       = "local"
type     = "ollama"
model    = "llama3.2:3b"
base_url = "http://localhost:11434"

[[backend]]
id       = "cloud"
type     = "openai"
model    = "gpt-4o"
base_url = "https://api.openai.com/v1"
api_key  = "${OPENAI_API_KEY}"

[[rule]]
id          = "golden-rules"
description = "Core development standards"
always_apply = true
content = """
# Golden Rules

1. Never break backward compatibility.
2. Test coverage >= 90%.
3. No secrets in code — use environment variables.
4. Structured logging with context on all important events.
5. Change only what's necessary — one story, one PR.
"""

[[rule]]
id          = "code-style"
description = "Language-specific coding conventions"
globs       = ["**/*.go"]
content = """
# Code Style

- Use slog for structured logging.
- Return errors, don't panic.
- Table-driven tests with t.Run.
- Keep functions small and focused.
"""
`

// ── doctor ────────────────────────────────────────────────────────────────────

var doctorManifest string

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check all configured backends for reachability and auth",
	RunE: func(cmd *cobra.Command, args []string) error {
		m, err := config.Load(doctorManifest)
		if err != nil {
			return err
		}

		// LivePrint uses animated spinner + ANSI colors when stdout is a TTY,
		// and falls back to plain text output when piped.
		results := doctor.LivePrint(doctorManifest, m)

		for _, r := range results {
			if r.Level == doctor.Fail {
				os.Exit(1)
			}
		}
		return nil
	},
}

// ── helpers ───────────────────────────────────────────────────────────────────

// resolveBackend loads the manifest and returns the backend plus a display
// config (id/type/model strings). Transparently handles both [[backend]] and
// [[orchestrator]] entries — the caller doesn't need to distinguish.
func resolveBackend(manifestPath, backendID string) (*model.Manifest, backend.Backend, *model.Backend, error) {
	m, err := config.Load(manifestPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load manifest: %w", err)
	}

	if backendID != "" {
		// ── try regular backend first
		if bc, ok := m.BackendByID(backendID); ok {
			b, err := backend.New(bc)
			if err != nil {
				return nil, nil, nil, err
			}
			return m, b, bc, nil
		}
		// ── try orchestrator
		if oc, ok := m.OrchestratorByID(backendID); ok {
			b, err := backend.NewOrchestrator(oc, m)
			if err != nil {
				return nil, nil, nil, err
			}
			strategy := oc.Strategy
			if strategy == "" {
				strategy = "dynamic"
			}
			// Synthetic display config so callers can show type/model uniformly.
			display := &model.Backend{
				ID:    oc.ID,
				Type:  "orchestrator",
				Model: strategy,
			}
			return m, b, display, nil
		}
		return nil, nil, nil, fmt.Errorf("backend or orchestrator %q not found in manifest", backendID)
	}

	// ── default: first [[backend]] entry
	bc, ok := m.DefaultBackend()
	if !ok {
		return nil, nil, nil, fmt.Errorf("no [[backend]] entries in manifest")
	}
	b, err := backend.New(bc)
	if err != nil {
		return nil, nil, nil, err
	}
	return m, b, bc, nil
}

// ── chat ──────────────────────────────────────────────────────────────────────

var chatBackendID string
var chatManifest string

var chatCmd = &cobra.Command{
	Use:   "chat",
	Short: "Start an interactive chat session using a configured backend",
	RunE: func(cmd *cobra.Command, args []string) error {
		manifest, b, bcfg, err := resolveBackend(chatManifest, chatBackendID)
		if err != nil {
			return err
		}

		system := manifest.SystemPrompt()
		sess := chat.New(b, system)

		return tui.RunChat(
			sess,
			bcfg.ID,
			bcfg.Type,
			bcfg.Model,
			len(manifest.Rules),
		)
	},
}

// ── prompt ────────────────────────────────────────────────────────────────────

var promptBackendID string
var promptManifest string

var promptCmd = &cobra.Command{
	Use:   "prompt <message>",
	Short: "Send a single message to a configured backend and print the response",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		m, b, _, err := resolveBackend(promptManifest, promptBackendID)
		if err != nil {
			return err
		}

		system := m.SystemPrompt()
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()

		_, err = b.Chat(ctx, system, nil, args[0], func(chunk string) {
			fmt.Print(chunk)
		})
		if err != nil {
			return err
		}
		fmt.Println()
		return nil
	},
}

// ── serve ─────────────────────────────────────────────────────────────────────

var serveProtocol string
var serveManifest string

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start a MCP stdio server exposing chat and prompt tools",
	RunE: func(cmd *cobra.Command, args []string) error {
		if serveProtocol != "mcp" {
			return fmt.Errorf("unsupported protocol %q — only 'mcp' is supported", serveProtocol)
		}

		m, err := config.Load(serveManifest)
		if err != nil {
			return fmt.Errorf("load manifest: %w", err)
		}

		srv, err := server.NewMCPServer(m)
		if err != nil {
			return err
		}

		srv.Startup()

		return srv.Serve(os.Stdin, os.Stdout)
	},
}

// ── wiring ────────────────────────────────────────────────────────────────────

func init() {
	compileCmd.Flags().StringVarP(&compileTarget, "target", "t", "all",
		"compile target: kiro, ollama, or all")

	chatCmd.Flags().StringVarP(&chatBackendID, "backend", "b", "", "backend id to use (default: first configured)")
	chatCmd.Flags().StringVarP(&chatManifest, "manifest", "m", "steermesh.toml", "path to steermesh.toml")

	promptCmd.Flags().StringVarP(&promptBackendID, "backend", "b", "", "backend id to use (default: first configured)")
	promptCmd.Flags().StringVarP(&promptManifest, "manifest", "m", "steermesh.toml", "path to steermesh.toml")

	serveCmd.Flags().StringVar(&serveProtocol, "protocol", "mcp", "server protocol (mcp)")
	serveCmd.Flags().StringVarP(&serveManifest, "manifest", "m", "steermesh.toml", "path to steermesh.toml")

	doctorCmd.Flags().StringVarP(&doctorManifest, "manifest", "m", "steermesh.toml", "path to steermesh.toml")

	rootCmd.AddCommand(compileCmd)
	rootCmd.AddCommand(validateCmd)
	rootCmd.AddCommand(initCmd)
	rootCmd.AddCommand(chatCmd)
	rootCmd.AddCommand(promptCmd)
	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(doctorCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
