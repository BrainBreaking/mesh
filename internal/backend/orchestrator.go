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

type backendStats struct {
	mu           sync.Mutex
	TotalCalls   int64
	TotalErrors  int64
	AvgLatencyMs float64 // exponential moving average (α = 0.3)
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

func (s *backendStats) snapshot() (calls, errors int64, avgMs float64, last time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.TotalCalls, s.TotalErrors, s.AvgLatencyMs, s.LastUsedAt
}

// ── worker entry ──────────────────────────────────────────────────────────────

type workerEntry struct {
	cfg     *model.Backend
	backend Backend
	stats   *backendStats
}

func (we *workerEntry) desc() string {
	if we.cfg.Model != "" {
		return we.cfg.Type + "/" + we.cfg.Model
	}
	return we.cfg.Type
}

// ── valid strategies ──────────────────────────────────────────────────────────

var validStrategies = map[string]bool{
	"dynamic":     true,
	"capability":  true,
	"round-robin": true,
	"fastest":     true,
	"auto":        true, // coordinator chooses strategy + worker in one call
}

// ── OrchestratorBackend ───────────────────────────────────────────────────────

// OrchestratorBackend implements Backend (and Commander) and routes every Chat
// call to one of its worker backends using a coordinator model or a
// deterministic strategy.  The strategy can be changed at runtime via Command.
type OrchestratorBackend struct {
	id          string
	cfg         *model.Orchestrator
	coordinator Backend
	workers     []*workerEntry
	workerMap   map[string]*workerEntry
	fallback    *workerEntry // may be nil

	// mutable strategy — protected by stratMu so Command() and Chat() are safe
	stratMu  sync.RWMutex
	strategy string

	rrMu      sync.Mutex
	rrCounter int // round-robin state
}

// NewOrchestrator constructs an OrchestratorBackend from a manifest.
func NewOrchestrator(cfg *model.Orchestrator, m *model.Manifest) (Backend, error) {
	coordCfg, ok := m.BackendByID(cfg.Coordinator)
	if !ok {
		return nil, fmt.Errorf("orchestrator %q: coordinator %q not found", cfg.ID, cfg.Coordinator)
	}
	coord, err := New(coordCfg)
	if err != nil {
		return nil, fmt.Errorf("orchestrator %q: coordinator: %w", cfg.ID, err)
	}

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

	strategy := cfg.Strategy
	if strategy == "" {
		strategy = "dynamic"
	}

	return &OrchestratorBackend{
		id:          cfg.ID,
		cfg:         cfg,
		coordinator: coord,
		workers:     workers,
		workerMap:   workerMap,
		fallback:    fb,
		strategy:    strategy,
	}, nil
}

func (o *OrchestratorBackend) ID() string { return o.id }

// getStrategy reads the current strategy (thread-safe).
func (o *OrchestratorBackend) getStrategy() string {
	o.stratMu.RLock()
	defer o.stratMu.RUnlock()
	return o.strategy
}

// setStrategy validates and updates the current strategy (thread-safe).
func (o *OrchestratorBackend) setStrategy(s string) error {
	if !validStrategies[s] {
		return fmt.Errorf("unknown strategy %q — valid: dynamic, capability, round-robin, fastest, auto", s)
	}
	o.stratMu.Lock()
	defer o.stratMu.Unlock()
	o.strategy = s
	return nil
}

// ── Backend.Chat ──────────────────────────────────────────────────────────────

func (o *OrchestratorBackend) Chat(
	ctx context.Context,
	system string,
	history []Message,
	userMsg string,
	stream func(string),
) (string, error) {
	worker, note := o.route(ctx, o.getStrategy(), userMsg)
	stream(note)

	start := time.Now()
	result, err := worker.backend.Chat(ctx, system, history, userMsg, stream)
	worker.stats.record(float64(time.Since(start).Milliseconds()), err)
	return result, err
}

// ── routing dispatcher ────────────────────────────────────────────────────────

func (o *OrchestratorBackend) route(ctx context.Context, strategy, task string) (*workerEntry, string) {
	switch strategy {

	case "auto":
		w, chosenStrategy, reason, err := o.routeAuto(ctx, task)
		if err != nil {
			w = o.fallbackWorker()
			return w, fmt.Sprintf("[mesh] auto-routing failed (%v) — falling back to %s\n\n", err, w.cfg.ID)
		}
		return w, fmt.Sprintf("[mesh] auto → %s via %s (%s) — %s\n\n",
			w.cfg.ID, chosenStrategy, w.desc(), reason)

	case "dynamic":
		w, reason, err := o.routeDynamic(ctx, task)
		if err != nil {
			w = o.fallbackWorker()
			return w, fmt.Sprintf("[mesh] dynamic routing failed (%v) — falling back to %s\n\n", err, w.cfg.ID)
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
		return w, fmt.Sprintf("[mesh] unknown strategy %q — falling back to %s\n\n", strategy, w.cfg.ID)
	}
}

// ── auto strategy ─────────────────────────────────────────────────────────────

// routeAuto asks the coordinator to pick both the strategy and (for dynamic)
// the target worker in a single LLM call.  Returns the chosen worker, the
// strategy name the coordinator selected, and the reason.
func (o *OrchestratorBackend) routeAuto(ctx context.Context, task string) (*workerEntry, string, string, error) {
	sysPrompt := o.buildAutoRouterPrompt()

	var buf strings.Builder
	_, err := o.coordinator.Chat(ctx, sysPrompt, nil, task, func(chunk string) {
		buf.WriteString(chunk)
	})
	if err != nil {
		return nil, "", "", fmt.Errorf("coordinator: %w", err)
	}

	raw := extractJSON(strings.TrimSpace(buf.String()))

	var decision struct {
		Strategy   string `json:"strategy"`
		DelegateTo string `json:"delegate_to"`
		Reason     string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(raw), &decision); err != nil {
		return nil, "", "", fmt.Errorf("coordinator returned invalid JSON (%q): %w", raw, err)
	}
	if decision.Strategy == "" {
		return nil, "", "", fmt.Errorf("coordinator response missing strategy field")
	}

	reason := decision.Reason
	if reason == "" {
		reason = "coordinator decision"
	}

	switch decision.Strategy {
	case "dynamic":
		// Coordinator already picked the worker — no second LLM call needed.
		if decision.DelegateTo == "" {
			return nil, "", "", fmt.Errorf("coordinator chose dynamic but omitted delegate_to")
		}
		we, ok := o.workerMap[decision.DelegateTo]
		if !ok {
			return nil, "", "", fmt.Errorf("coordinator chose unknown worker %q", decision.DelegateTo)
		}
		return we, "dynamic", reason, nil

	case "capability":
		return o.routeByCapability(task), "capability", reason, nil

	case "round-robin":
		return o.routeRoundRobin(), "round-robin", reason, nil

	case "fastest":
		w, _ := o.routeFastest()
		return w, "fastest", reason, nil

	default:
		return nil, "", "", fmt.Errorf("coordinator returned unknown strategy %q", decision.Strategy)
	}
}

// ── dynamic strategy ──────────────────────────────────────────────────────────

func (o *OrchestratorBackend) routeDynamic(ctx context.Context, task string) (*workerEntry, string, error) {
	var buf strings.Builder
	_, err := o.coordinator.Chat(ctx, o.buildDynamicRouterPrompt(), nil, task, func(chunk string) {
		buf.WriteString(chunk)
	})
	if err != nil {
		return nil, "", fmt.Errorf("coordinator: %w", err)
	}

	raw := extractJSON(strings.TrimSpace(buf.String()))
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

// ── deterministic strategies ──────────────────────────────────────────────────

func (o *OrchestratorBackend) routeByCapability(task string) *workerEntry {
	taskLower := strings.ToLower(task)
	bestIdx, bestScore := 0, -1
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

func (o *OrchestratorBackend) routeRoundRobin() *workerEntry {
	o.rrMu.Lock()
	defer o.rrMu.Unlock()
	w := o.workers[o.rrCounter%len(o.workers)]
	o.rrCounter++
	return w
}

func (o *OrchestratorBackend) routeFastest() (*workerEntry, float64) {
	var best *workerEntry
	bestLatency := math.MaxFloat64
	for _, we := range o.workers {
		_, _, avg, _ := we.stats.snapshot()
		if avg == 0 {
			return we, 0 // never used — prefer to collect a data point
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

func (o *OrchestratorBackend) fallbackWorker() *workerEntry {
	if o.fallback != nil {
		return o.fallback
	}
	return o.workers[0]
}

// ── coordinator prompts ───────────────────────────────────────────────────────

func (o *OrchestratorBackend) workerTable() string {
	var sb strings.Builder
	for _, we := range o.workers {
		caps := strings.Join(we.cfg.Capabilities, ", ")
		if caps == "" {
			caps = "general"
		}
		sb.WriteString(fmt.Sprintf("  %-22s (%s) — %s\n", we.cfg.ID, we.desc(), caps))
	}
	return sb.String()
}

func (o *OrchestratorBackend) buildDynamicRouterPrompt() string {
	return "You are the mesh task router. Pick the best worker for the task.\n\n" +
		"Workers:\n" + o.workerTable() +
		"\nRules:\n" +
		"- Respond with ONLY a JSON object. No preamble, no markdown.\n" +
		"- delegate_to must exactly match a worker ID above.\n" +
		"- Keep reason under 10 words.\n\n" +
		`Required format: {"delegate_to":"<worker_id>","reason":"<why>"}` + "\n"
}

func (o *OrchestratorBackend) buildAutoRouterPrompt() string {
	return "You are the mesh meta-router. Decide the routing strategy AND the target worker.\n\n" +
		"Strategies:\n" +
		"  dynamic      pick a specific worker based on task semantics (you choose delegate_to)\n" +
		"  capability   keyword matching — good for clear-cut task types\n" +
		"  round-robin  distribute evenly — good when any worker would do\n" +
		"  fastest      pick by latency — good for trivial/time-sensitive tasks\n\n" +
		"Workers:\n" + o.workerTable() +
		"\nRules:\n" +
		"- Respond with ONLY a JSON object. No preamble, no markdown.\n" +
		"- When strategy is 'dynamic', delegate_to must be set to a valid worker ID.\n" +
		"- For other strategies, omit delegate_to or set it to null.\n" +
		"- Keep reason under 10 words.\n\n" +
		`Required format: {"strategy":"<strategy>","delegate_to":"<worker_id_or_null>","reason":"<why>"}` + "\n"
}

// ── Commander interface ───────────────────────────────────────────────────────

// Command handles slash commands for runtime orchestrator control.
// Implements the Commander interface from the backend package.
func (o *OrchestratorBackend) Command(cmd string) (string, error) {
	parts := strings.Fields(strings.TrimPrefix(strings.TrimPrefix(cmd, "/"), "/"))
	if len(parts) == 0 {
		return o.helpText(), nil
	}

	switch parts[0] {

	case "strategy":
		if len(parts) == 1 {
			// show current
			return fmt.Sprintf("current strategy: %s\nvalid: dynamic, capability, round-robin, fastest, auto", o.getStrategy()), nil
		}
		newStrat := parts[1]
		if err := o.setStrategy(newStrat); err != nil {
			return "", err
		}
		return fmt.Sprintf("strategy → %s", newStrat), nil

	case "workers":
		return o.workersText(), nil

	case "stats":
		return o.statsText(), nil

	case "help":
		return o.helpText(), nil

	default:
		return "", fmt.Errorf("unknown command %q — try /help", parts[0])
	}
}

func (o *OrchestratorBackend) workersText() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("orchestrator: %s  (coordinator: %s)\n\n", o.id, o.cfg.Coordinator))
	sb.WriteString(fmt.Sprintf("%-22s  %-30s  %s\n", "worker", "model", "capabilities"))
	sb.WriteString(strings.Repeat("─", 80) + "\n")
	for _, we := range o.workers {
		caps := strings.Join(we.cfg.Capabilities, ", ")
		if caps == "" {
			caps = "—"
		}
		if len(caps) > 40 {
			caps = caps[:37] + "…"
		}
		sb.WriteString(fmt.Sprintf("%-22s  %-30s  %s\n", we.cfg.ID, we.desc(), caps))
	}
	if o.fallback != nil {
		sb.WriteString(fmt.Sprintf("\nfallback: %s\n", o.fallback.cfg.ID))
	}
	return sb.String()
}

func (o *OrchestratorBackend) statsText() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("orchestrator: %s  strategy: %s\n\n", o.id, o.getStrategy()))
	sb.WriteString(fmt.Sprintf("%-22s  %8s  %8s  %12s  %s\n", "worker", "calls", "errors", "avg latency", "last used"))
	sb.WriteString(strings.Repeat("─", 80) + "\n")
	for _, we := range o.workers {
		calls, errors, avg, last := we.stats.snapshot()
		avgStr := "—"
		if avg > 0 {
			avgStr = fmt.Sprintf("%.0fms", avg)
		}
		lastStr := "—"
		if !last.IsZero() {
			lastStr = last.Format("15:04:05")
		}
		sb.WriteString(fmt.Sprintf("%-22s  %8d  %8d  %12s  %s\n",
			we.cfg.ID, calls, errors, avgStr, lastStr))
	}
	return sb.String()
}

func (o *OrchestratorBackend) helpText() string {
	return fmt.Sprintf(`orchestrator: %s  strategy: %s

/strategy              show current strategy
/strategy <name>       change strategy (dynamic | capability | round-robin | fastest | auto)
/workers               list workers and their capabilities
/stats                 per-worker call counts and latency metrics
/help                  show this help

strategies:
  auto         coordinator picks strategy + worker in one call (recommended)
  dynamic      coordinator picks the worker (1 LLM call per message)
  capability   keyword match against capability tags (no LLM call)
  round-robin  cycle through workers in order (no LLM call)
  fastest      pick worker with lowest avg latency (no LLM call)`, o.id, o.getStrategy())
}

// ── helpers ───────────────────────────────────────────────────────────────────

func extractJSON(s string) string {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start == -1 || end == -1 || end <= start {
		return s
	}
	return s[start : end+1]
}
