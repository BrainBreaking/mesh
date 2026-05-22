package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/BrainBreaking/mesh/internal/backend"
	"github.com/BrainBreaking/mesh/internal/model"
)

// MCPServer is a minimal Model Context Protocol stdio server (JSON-RPC 2.0).
type MCPServer struct {
	manifest *model.Manifest

	mu       sync.Mutex
	backends map[string]backend.Backend    // lazily populated on first use
	sessions map[string][]backend.Message  // session_id → history
}

// NewMCPServer creates a server from the manifest. Backends are instantiated
// lazily on first use so that missing API keys don't prevent startup.
func NewMCPServer(m *model.Manifest) (*MCPServer, error) {
	return &MCPServer{
		manifest: m,
		backends: make(map[string]backend.Backend, len(m.Backends)),
		sessions: make(map[string][]backend.Message),
	}, nil
}

// getBackend returns a cached backend by ID, instantiating it on first call.
func (s *MCPServer) getBackend(id string) (backend.Backend, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if b, ok := s.backends[id]; ok {
		return b, nil
	}

	cfg, ok := s.manifest.BackendByID(id)
	if !ok {
		return nil, fmt.Errorf("unknown backend: %s", id)
	}

	b, err := backend.New(cfg)
	if err != nil {
		return nil, err
	}
	s.backends[id] = b
	return b, nil
}

// defaultBackend returns (or lazily creates) the first configured backend.
func (s *MCPServer) defaultBackend() (backend.Backend, error) {
	cfg, ok := s.manifest.DefaultBackend()
	if !ok {
		return nil, fmt.Errorf("no backends configured in manifest")
	}
	return s.getBackend(cfg.ID)
}

// ── JSON-RPC types ────────────────────────────────────────────────────────────

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id,omitempty"`
	Result  any    `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ── Serve ─────────────────────────────────────────────────────────────────────

// Serve reads JSON-RPC messages from in and writes responses to out line-by-line.
func (s *MCPServer) Serve(in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)

	enc := json.NewEncoder(out)

	respond := func(id any, result any, rpcErr *rpcError) {
		r := rpcResponse{JSONRPC: "2.0", ID: id, Result: result, Error: rpcErr}
		_ = enc.Encode(r)
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			respond(nil, nil, &rpcError{Code: -32700, Message: "parse error"})
			continue
		}

		switch req.Method {
		case "initialize":
			respond(req.ID, s.handleInitialize(), nil)

		case "notifications/initialized":
			// Notification — no response needed.

		case "tools/list":
			respond(req.ID, map[string]any{"tools": s.toolList()}, nil)

		case "tools/call":
			result, rpcErr := s.handleToolCall(req.Params)
			respond(req.ID, result, rpcErr)

		// MCP clients expect these even when empty.
		case "resources/list":
			respond(req.ID, map[string]any{"resources": []any{}}, nil)

		case "resources/read":
			respond(req.ID, nil, &rpcError{Code: -32602, Message: "no resources available"})

		case "prompts/list":
			respond(req.ID, map[string]any{"prompts": []any{}}, nil)

		case "prompts/get":
			respond(req.ID, nil, &rpcError{Code: -32602, Message: "no prompts available"})

		default:
			respond(req.ID, nil, &rpcError{Code: -32601, Message: fmt.Sprintf("method not found: %s", req.Method)})
		}
	}

	return scanner.Err()
}

// ── Handlers ──────────────────────────────────────────────────────────────────

func (s *MCPServer) handleInitialize() map[string]any {
	return map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    "mesh",
			"version": "0.1.0",
		},
	}
}

func (s *MCPServer) toolList() []map[string]any {
	return []map[string]any{
		{
			"name":        "chat",
			"description": "Send a message to a backend and receive a response (stateful, maintains history per session_id)",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"backend":    map[string]any{"type": "string", "description": "Backend ID to use"},
					"message":    map[string]any{"type": "string", "description": "User message"},
					"session_id": map[string]any{"type": "string", "description": "Optional session ID for multi-turn history"},
				},
				"required": []string{"message"},
			},
		},
		{
			"name":        "prompt",
			"description": "Send a one-shot message to a backend (stateless, no history)",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"backend": map[string]any{"type": "string", "description": "Backend ID to use"},
					"message": map[string]any{"type": "string", "description": "User message"},
				},
				"required": []string{"message"},
			},
		},
		{
			"name":        "list_backends",
			"description": "List all configured backend IDs and their models",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

func (s *MCPServer) handleToolCall(params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid params"}
	}

	switch p.Name {
	case "chat":
		return s.toolChat(p.Arguments, true)
	case "prompt":
		return s.toolChat(p.Arguments, false)
	case "list_backends":
		return s.toolListBackends(), nil
	default:
		return nil, &rpcError{Code: -32601, Message: fmt.Sprintf("unknown tool: %s", p.Name)}
	}
}

func (s *MCPServer) toolChat(args json.RawMessage, stateful bool) (any, *rpcError) {
	var a struct {
		Backend   string `json:"backend"`
		Message   string `json:"message"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid arguments"}
	}
	if a.Message == "" {
		return nil, &rpcError{Code: -32602, Message: "message is required"}
	}

	var b backend.Backend
	var berr error
	if a.Backend != "" {
		b, berr = s.getBackend(a.Backend)
	} else {
		b, berr = s.defaultBackend()
	}
	if berr != nil {
		return nil, &rpcError{Code: -32602, Message: berr.Error()}
	}

	system := s.manifest.SystemPrompt()
	ctx, cancel := context.WithTimeout(context.Background(), 5*60*1e9) // 5-min cap per tool call
	defer cancel()

	var history []backend.Message
	if stateful && a.SessionID != "" {
		s.mu.Lock()
		history = append([]backend.Message(nil), s.sessions[a.SessionID]...)
		s.mu.Unlock()
	}

	reply, err := b.Chat(ctx, system, history, a.Message, nil)
	if err != nil {
		return nil, &rpcError{Code: -32000, Message: err.Error()}
	}

	if stateful && a.SessionID != "" {
		s.mu.Lock()
		s.sessions[a.SessionID] = append(history,
			backend.Message{Role: "user", Content: a.Message},
			backend.Message{Role: "assistant", Content: reply},
		)
		s.mu.Unlock()
	}

	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": reply},
		},
	}, nil
}

func (s *MCPServer) toolListBackends() map[string]any {
	list := make([]map[string]any, 0, len(s.manifest.Backends))
	for _, bc := range s.manifest.Backends {
		list = append(list, map[string]any{
			"id":    bc.ID,
			"type":  bc.Type,
			"model": bc.Model,
		})
	}
	return map[string]any{"backends": list}
}

// Startup prints a startup message to stderr (so it doesn't pollute the MCP channel).
func (s *MCPServer) Startup() {
	fmt.Fprintf(os.Stderr, "[mesh] MCP server ready · %d backends configured · %d rules loaded\n",
		len(s.manifest.Backends), len(s.manifest.Rules))
}
