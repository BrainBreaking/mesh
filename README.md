# mesh

> Define AI steering rules once. Compile everywhere.

`mesh` is the SteerMesh config compiler — a CLI that takes a single `steermesh.toml` manifest and compiles it into target-specific rule formats for AI coding tools.

## Supported targets

| Target | Output |
|--------|--------|
| `kiro` | `.kiro/steering/*.md` — one file per rule with frontmatter |
| `ollama` | `Modelfile` — `FROM`, `PARAMETER`, and `SYSTEM` blocks |

## Install

```bash
go install github.com/BrainBreaking/mesh/cmd/mesh@latest
```

Or build from source:

```bash
git clone https://github.com/BrainBreaking/mesh
cd mesh
go build -o mesh ./cmd/mesh/
```

## Quick start

```bash
# Scaffold a new manifest
mesh init

# Validate without writing files
mesh validate steermesh.toml

# Compile to all targets
mesh compile

# Compile to a specific target
mesh compile --target kiro
mesh compile --target ollama

# Compile a non-default manifest path
mesh compile path/to/rules.toml --target ollama
```

## steermesh.toml format

```toml
[project]
name    = "my-project"
version = "1.0.0"

[targets.ollama]
model       = "gemma3:4b"   # any Ollama model tag
temperature = 0.7
num_ctx     = 8192

[targets.kiro]
output_dir = ".kiro/steering"  # default

[[rule]]
id           = "golden-rules"
description  = "Core development standards"
always_apply = true
content = """
# Golden Rules
Never break backward compatibility.
Test coverage >= 90%.
"""

[[rule]]
id          = "go-style"
description = "Go-specific coding conventions"
globs       = ["**/*.go"]
content = """
- Use slog for structured logging.
- Return errors, don't panic.
"""
```

## Output examples

**Kiro** (`.kiro/steering/golden-rules.md`):
```markdown
---
description: Core development standards
alwaysApply: true
---

# Golden Rules
Never break backward compatibility.
```

**Ollama** (`Modelfile`):
```
FROM gemma3:4b

PARAMETER temperature 0.70
PARAMETER num_ctx 8192

SYSTEM """
# Golden Rules
...
"""
```

## Roadmap

- [ ] `mesh compile --target cursor` → `.cursor/rules/*.mdc`
- [ ] `mesh compile --target claude` → `CLAUDE.md`
- [ ] `mesh compile --target amazonq` → `.amazonq/rules/*.md`
- [ ] `mesh watch` — recompile on file change
- [ ] Rule inheritance / `extends`

---

Built by [Brain Breaking LLC](https://brainbreaking.com) · Part of the [SteerMesh](https://steermesh.dev) ecosystem.
