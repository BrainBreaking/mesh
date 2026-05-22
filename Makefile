BINARY     := mesh
MODULE     := github.com/BrainBreaking/mesh
CMD        := ./cmd/mesh
VERSION    := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS    := -ldflags "-X main.version=$(VERSION)"

MANIFEST   ?= steermesh.toml
TARGET     ?= all

# ── Build ─────────────────────────────────────────────────────────────────────

.PHONY: build
build: ## Build the mesh binary
	go build $(LDFLAGS) -o $(BINARY) $(CMD)

.PHONY: install
install: ## Install mesh to $GOPATH/bin
	go install $(LDFLAGS) $(CMD)

.PHONY: clean
clean: ## Remove built binary
	rm -f $(BINARY)

# ── Dev ───────────────────────────────────────────────────────────────────────

.PHONY: run
run: build ## Build and run: make run ARGS="compile --target kiro"
	./$(BINARY) $(ARGS)

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum
	go mod tidy

.PHONY: fmt
fmt: ## Format all Go source files
	go fmt ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint (install: brew install golangci-lint)
	golangci-lint run ./...

# ── Test ──────────────────────────────────────────────────────────────────────

.PHONY: test
test: ## Run all tests
	go test ./... -v

.PHONY: test-cover
test-cover: ## Run tests with coverage report
	go test ./... -coverprofile=coverage.out
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

# ── mesh commands ─────────────────────────────────────────────────────────────

.PHONY: validate
validate: build ## Validate the manifest (MANIFEST=steermesh.toml)
	./$(BINARY) validate $(MANIFEST)

.PHONY: compile
compile: build ## Compile manifest to all targets (TARGET=all|kiro|ollama)
	./$(BINARY) compile $(MANIFEST) --target $(TARGET)

.PHONY: compile-kiro
compile-kiro: build ## Compile manifest → .kiro/steering/
	./$(BINARY) compile $(MANIFEST) --target kiro

.PHONY: compile-ollama
compile-ollama: build ## Compile manifest → Modelfile
	./$(BINARY) compile $(MANIFEST) --target ollama

# ── Chat / Prompt / Serve ────────────────────────────────────────────────────

BACKEND    ?=
MSG        ?= hello

.PHONY: chat
chat: build ## Start interactive chat: make chat BACKEND=local MANIFEST=steermesh.toml
	./$(BINARY) chat $(if $(BACKEND),--backend $(BACKEND),) --manifest $(MANIFEST) $(ARGS)

.PHONY: prompt
prompt: build ## Send a single prompt: make prompt MSG="your message" BACKEND=local
	./$(BINARY) prompt --manifest $(MANIFEST) $(if $(BACKEND),--backend $(BACKEND),) "$(MSG)"

.PHONY: serve
serve: build ## Start MCP stdio server: make serve MANIFEST=steermesh.toml
	./$(BINARY) serve --manifest $(MANIFEST) $(ARGS)

# ── Ollama integration ────────────────────────────────────────────────────────

OLLAMA_MODEL ?= $(shell grep 'name' steermesh.toml 2>/dev/null | head -1 | sed 's/.*= *"\(.*\)"/\1/' || echo "mesh-model")

.PHONY: ollama-create
ollama-create: compile-ollama ## Compile → Modelfile and load into Ollama
	ollama create $(OLLAMA_MODEL) -f Modelfile
	@echo "✓  Model '$(OLLAMA_MODEL)' ready — run: ollama run $(OLLAMA_MODEL)"

.PHONY: ollama-run
ollama-run: ## Run the compiled Ollama model interactively
	ollama run $(OLLAMA_MODEL)

.PHONY: ollama-rm
ollama-rm: ## Remove the compiled Ollama model
	ollama rm $(OLLAMA_MODEL)

# ── CI ────────────────────────────────────────────────────────────────────────

.PHONY: ci
ci: fmt vet test build ## Full CI pipeline: fmt + vet + test + build

# ── Help ──────────────────────────────────────────────────────────────────────

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

.DEFAULT_GOAL := help
