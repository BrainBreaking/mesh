package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/BrainBreaking/mesh/internal/model"
)

// ── per-backend metrics ───────────────────────────────────────────────────────

// backendStats tracks performance metrics for a single worker backend.
// All exported fields are read-only after construction; Record() is the
// only mutator and is safe for concurrent use.
type backendStats struct {
	mu           sync.Mutex
	TotalCalls   int64
	TotalErrors  int64
	AvgLatencyMs float64  // exponential moving average (α = 0.3)
	LastUsedAt   time.Time
}

func (s *backendStats) record(latencyMs float64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.TotalCalls++
	s.LastUsedAt = time.Now()
	if err != nil {
		s.TotalErrors++
		return
	}
	const alpha = 0.3
	if s.AvgLatencyMs == 0 {
		s.AvgLatencyMs = latencyMs
	} else {
		s.AvgLatencyMs = alpha*latencyMs + (1-alpha)*s.AvgLatencyMs
	}
}

func (s *backendStats) avgLatency() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.AvgLatencyMs
}

// ── worker entry ──────────────────────────────────────────────────────────────

// workerEntry bundles a resolved backend with its manifest config.
type workerEntry struct {
	cfg     *model.Backend
	backend Backend
	stats   *backendStats
}

// desc returns a human-readable description: "ollama/qwen2.5-coder:14b"
func (we *workerEntry) desc() string {
	if we.cfg.Model != "" {
		return we.cfg.Type + "/" + we.cfg.Model
	}
	return we.cfg.Type
}

// ── OrchestratorBackend ───────────────────────────────────────────────────────

// OrchestratorBackend implements Backend and routes every Chat call to one of
// its worker backends using a coordinator model or a deterministic strategy.
type OrchestratorBackend struct {
	id          string
	cfg         *model.Orchestrator
	coordinator Backend
	workers     []*workerEntry          // ordered slice (preserves toml order)
	workerMap   map[string]*workerEntry // id → entry for O(1) lookup
	fallback    *workerEntry            // may be nil
	rrCounter   int                     // round-robin state
	manifest    *model.Manifest         // for system prompt pass-through
}

// NewOrchestrator constructs an OrchestratorBackend from a manifest.
// All referenced backends must already be present in the manifest.
func NewOrchestrator(cfg *model.Orchestrator, m *model.Manifest) (Backend, error) {
	// ── coordinator
	coordCfg, ok := m.BackendByID(cfg.Coordinator)
	if !ok {
		return nil, fmt.Errorf("orchestrator %q: coordinator %q not found", cfg.ID, cfg.Coordinator)
	}
	coord, err := New(coordCfg)
	if err != nil {
		return nil, fmt.Errorf("orchestrator %q: coordinator: %w", cfg.ID, err)
	}

	// ── workers
	workers := make([]*workerEntry, 0, len(cfg.Workers))
	workerMap := make(map[string]*workerEntry, len(cfg.Workers))
	for _, wid := range cfg.Workers {
		wcfg, ok := m.BackendByID(wid)
		if !ok {
			return nil, fmt.Errorf("orchestrator %q: worker %q not found", cfg.ID, wid)
		}
		wb, err := New(wcfg)
		if err != nil {
			return nil, fmt.Errorf("orchestrator %q: worker %q: %w", cfg.ID, wid, err)
		}
		we := &workerEntry{cfg: wcfg, backend: wb, stats: &backendStats{}}
		workers = append(workers, we)
		workerMap[wid] = we
	}

	// ── fallback (optional)
	var fb *workerEntry
	if cfg.Fallback != "" {
		fbCfg, ok := m.BackendByID(cfg.Fallback)
		if !ok {
			return nil, fmt.Errorf("orchestrator %q: fallback %q not found", cfg.ID, cfg.Fallback)
		}
		fbb, err := New(fbCfg)
		if err != nil {
			return nil, fmt.Errorf("orchestrator %q: fallback: %w", cfg.ID, err)
		}
		fb = &workerEntry{cfg: fbCfg, backend: fbb, stats: &backendStats{}}
	}

	return &OrchestratorBackend{
		id:          cfg.ID,
		cfg:         cfg,
		coordinator: coord,
		workers:     workers,
		workerMap:   workerMap,
		fallback:    fb,
		manifest:    m,
	}, nil
}

func (o *OrchestratorBackend) ID() string { return o.id }

// Chat routes the request to a worker backend and streams its response.
// A routing annotation line is streamed first so the caller can display
// which model handled the request.
func (o *OrchestratorBackend) Chat(
	ctx context.Context,
	system string,
	history []Message,
	userMsg string,
	stream func(string),
) (string, error) {
	strategy := o.cfg.Strategy
	if strategy == "" {
		strategy = "dynamic"
	}

	worker, note := o.route(ctx, strategy, userMsg)

	// Stream the routing annotation before the actual response.
	stream(note)

	start := time.Now()
	result, err := worker.backend.Chat(ctx, system, history, userMsg, stream)
	worker.stats.record(float64(time.Since(start).Milliseconds()), err)

	return result, err
}

// route picks a worker and returns it together with a routing annotation string.
func (o *OrchestratorBackend) route(ctx context.Context, strategy, task string) (*workerEntry, string) {
	switch strategy {

	case "dynamic":
		w, reason, err := o.routeDynamic(ctx, task)
		if err != nil {
			w = o.fallbackWorker()
			return w, fmt.Sprintf(
				"[mesh] routing failed (%v) — falling back to %s\n\n", err, w.cfg.ID)
		}
		return w, fmt.Sprintf("[mesh] → %s (%s) — %s\n\n", w.cfg.ID, w.desc(), reason)

	case "capability":
		w := o.routeByCapability(task)
		return w, fmt.Sprintf("[mesh] → %s (%s) — capability match\n\n", w.cfg.ID, w.desc())

	case "round-robin":
		w := o.routeRoundRobin()
		return w, fmt.Sprintf("[mesh] → %s (%s) — round-robin\n\n", w.cfg.ID, w.desc())

	case "fastest":
		w, avg := o.routeFastest()
		if avg > 0 {
			return w, fmt.Sprintf("[mesh] → %s (%s) — fastest (avg %.0fms)\n\n", w.cfg.ID, w.desc(), avg)
		}
		return w, fmt.Sprintf("[mesh] → %s (%s) — fastest (no data yet)\n\n", w.cfg.ID, w.desc())

	default:
		w := o.fallbackWorker()
		return w, fmt.Sprintf("[mesh] → %s — unknown strategy %q, using first worker\n\n", w.cfg.ID, strategy)
	}
}

// ── routing strategies ────────────────────────────────────────────────────────

// routeDynamic asks the coordinator model to choose a worker.
func (o *OrchestratorBackend) routeDynamic(ctx context.Context, task string) (*workerEntry, string, error) {
	routerSystem := o.buildRouterSystemPrompt()

	var respBuf strings.Builder
	_, err := o.coordinator.Chat(ctx, routerSystem, nil, task, func(chunk string) {
		respBuf.WriteString(chunk)
	})
	if err != nil {
		return nil, "", fmt.Errorf("coordinator: %w", err)
	}

	raw := extractJSON(strings.TrimSpace(respBuf.String()))

	var decision struct {
		DelegateTo string `json:"delegate_to"`
		Reason     string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(raw), &decision); err != nil {
		return nil, "", fmt.Errorf("coordinator returned invalid JSON (%q): %w", raw, err)
	}
	if decision.DelegateTo == "" {
		return nil, "", fmt.Errorf("coordinator response missing delegate_to field")
	}

	we, ok := o.workerMap[decision.DelegateTo]
	if !ok {
		return nil, "", fmt.Errorf("coordinator chose unknown worker %q", decision.DelegateTo)
	}

	reason := decision.Reason
	if reason == "" {
		reason = "coordinator decision"
	}
	return we, reason, nil
}

// routeByCapability picks the worker whose capabilities best match the task keywords.
func (o *OrchestratorBackend) routeByCapability(task string) *workerEntry {
	taskLower := strings.ToLower(task)

	bestIdx := 0
	bestScore := -1
	for i, we := range o.workers {
		score := 0
		for _, cap := range we.cfg.Capabilities {
			if strings.Contains(taskLower, strings.ToLower(cap)) {
				score++
			}
		}
		if score > bestScore {
			bestScore = score
			bestIdx = i
		}
	}
	return o.workers[bestIdx]
}

// routeRoundRobin cycles through workers in order.
func (o *OrchestratorBackend) routeRoundRobin() *workerEntry {
	w := o.workers[o.rrCounter%len(o.workers)]
	o.rrCounter++
	return w
}

// routeFastest picks the worker with the lowest average latency.
// Workers with no data yet are tried first (latency = 0 means unobserved).
func (o *OrchestratorBackend) routeFastest() (*workerEntry, float64) {
	var best *workerEntry
	bestLatency := math.MaxFloat64

	for _, we := range o.workers {
		avg := we.stats.avgLatency()
		if avg == 0 {
			// Never used — prefer it to collect a data point.
			return we, 0
		}
		if avg < bestLatency {
			bestLatency = avg
			best = we
		}
	}
	if best == nil {
		return o.workers[0], 0
	}
	return best, bestLatency
}

// fallbackWorker returns the explicit fallback if configured, otherwise the
// first worker in the list.
func (o *OrchestratorBackend) fallbackWorker() *workerEntry {
	if o.fallback != nil {
		return o.fallback
	}
	return o.workers[0]
}

// ── coordinator prompt ────────────────────────────────────────────────────────

func (o *OrchestratorBackend) buildRouterSystemPrompt() string {
	var sb strings.Builder
	sb.WriteString("You are the mesh task router. Your only job is to select the best worker for the task.\n\n")
	sb.WriteString("Available workers:\n")
	for _, we := range o.workers {
		caps := strings.Join(we.cfg.Capabilities, ", ")
		if caps == "" {
			caps = "general"
		}
		sb.WriteString(fmt.Sprintf("  %-22s (%s) — %s\n", we.cfg.ID, we.desc(), caps))
	}
	sb.WriteString("\nRules:\n")
	sb.WriteString("- Respond with ONLY a JSON object. No preamble, no explanation, no markdown fences.\n")
	sb.WriteString("- The delegate_to field must exactly match one of the worker IDs above.\n")
	sb.WriteString("- Keep reason under 10 words.\n\n")
	sb.WriteString("Required format:\n")
	sb.WriteString(`{"delegate_to":"<worker_id>","reason":"<why in ≤10 words>"}`)
	sb.WriteString("\n")
	return sb.String()
}

// ── helpers ───────────────────────────────────────────────────────────────────

// extractJSON extracts the first complete JSON object from a string.
// This handles coordinators that wrap their response in extra text.
func extractJSON(s string) string {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start == -1 || end == -1 || end <= start {
		return s
	}
	return s[start : end+1]
}
