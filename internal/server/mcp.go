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
	backends map[string]backend.Backend

	mu       sync.Mutex
	sessions map[string][]backend.Message // session_id → history
}

// NewMCPServer creates a server from the manifest, instantiating all configured backends.
func NewMCPServer(m *model.Manifest) (*MCPServer, error) {
	backends := make(map[string]backend.Backend, len(m.Backends))
	for i := range m.Backends {
		b, err := backend.New(&m.Backends[i])
		if err != nil {
			return nil, fmt.Errorf("mcp: backend %q: %w", m.Backends[i].ID, err)
		}
		backends[m.Backends[i].ID] = b
	}
	return &MCPServer{
		manifest: m,
		backends: backends,
		sessions: make(map[string][]backend.Message),
	}, nil
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
			// No response needed for notifications.

		case "tools/list":
			respond(req.ID, map[string]any{"tools": s.toolList()}, nil)

		case "tools/call":
			result, rpcErr := s.handleToolCall(req.Params)
			respond(req.ID, result, rpcErr)

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
	if a.Backend != "" {
		var ok bool
		b, ok = s.backends[a.Backend]
		if !ok {
			return nil, &rpcError{Code: -32602, Message: fmt.Sprintf("unknown backend: %s", a.Backend)}
		}
	} else {
		// Use first available backend.
		for _, bk := range s.backends {
			b = bk
			break
		}
		if b == nil {
			return nil, &rpcError{Code: -32602, Message: "no backends configured"}
		}
	}

	system := s.manifest.SystemPrompt()
	ctx := context.Background()

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
	fmt.Fprintf(os.Stderr, "[mesh] MCP server ready · %d backends · %d rules\n",
		len(s.backends), len(s.manifest.Rules))
}
