package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
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

// ── delegation types ──────────────────────────────────────────────────────────

type delegationCall struct {
	WorkerID string
	Task     string
}

type delegationResult struct {
	WorkerID string
	Output   string
	Err      error
}

// ── valid strategies ──────────────────────────────────────────────────────────

var validStrategies = map[string]bool{
	"auto":        true, // full agentic loop — coordinator delegates, synthesizes
	"dynamic":     true, // single routing — coordinator picks one worker
	"capability":  true, // keyword match, no LLM
	"round-robin": true, // cycle, no LLM
	"fastest":     true, // latency-based, no LLM
}

// ── OrchestratorBackend ───────────────────────────────────────────────────────

// OrchestratorBackend implements Backend (and Commander).
//
//   - "auto" strategy: full agentic loop — coordinator issues <delegate> tags,
//     workers run in parallel, coordinator synthesizes final answer.
//   - "dynamic": single routing — coordinator picks one worker per message.
//   - "capability", "round-robin", "fastest": deterministic, no coordinator call.
type OrchestratorBackend struct {
	id          string
	cfg         *model.Orchestrator
	coordinator Backend
	workers     []*workerEntry
	workerMap   map[string]*workerEntry
	fallback    *workerEntry

	stratMu  sync.RWMutex
	strategy string

	rrMu      sync.Mutex
	rrCounter int
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

func (o *OrchestratorBackend) getStrategy() string {
	o.stratMu.RLock()
	defer o.stratMu.RUnlock()
	return o.strategy
}

func (o *OrchestratorBackend) setStrategy(s string) error {
	if !validStrategies[s] {
		return fmt.Errorf("unknown strategy %q — valid: auto, dynamic, capability, round-robin, fastest", s)
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
	switch o.getStrategy() {
	case "auto":
		// Full agentic loop: coordinator delegates, workers execute, coordinator synthesizes.
		return o.agentLoop(ctx, system, history, userMsg, stream)
	default:
		// Single routing for dynamic + all deterministic strategies.
		worker, note := o.routeSingle(ctx, o.getStrategy(), userMsg)
		stream(note)
		start := time.Now()
		result, err := worker.backend.Chat(ctx, system, history, userMsg, stream)
		worker.stats.record(float64(time.Since(start).Milliseconds()), err)
		return result, err
	}
}

// ── agentic loop ──────────────────────────────────────────────────────────────

const maxDelegationRounds = 5

// agentLoop is the core of the "auto" strategy.
//
// Protocol:
//  1. Call the coordinator with the task and a system prompt that explains
//     how to use <delegate> tags.
//  2. Parse any <delegate worker="id">task</delegate> blocks from the response.
//  3. Execute all delegations in parallel; collect results.
//  4. Feed results back to the coordinator.
//  5. Repeat until the coordinator returns a plain-text final answer (no tags).
//
// Workers always receive the original project system prompt so project rules
// are applied. The coordinator only gets the orchestration system prompt.
func (o *OrchestratorBackend) agentLoop(
	ctx context.Context,
	system string,
	history []Message,
	userMsg string,
	stream func(string),
) (string, error) {
	// Coordinator context: starts with original conversation history.
	coordHistory := make([]Message, len(history))
	copy(coordHistory, history)

	agentSystem := o.buildAgentSystemPrompt()

	for round := 0; round < maxDelegationRounds; round++ {
		// On the first round the task is the user message.
		// On subsequent rounds the task is already in coordHistory.
		input := userMsg
		if round > 0 {
			input = ""
		}

		// ── call coordinator (buffered — we need to check for delegation tags)
		stream(fmt.Sprintf("[mesh] planning (round %d)…\n", round+1))

		var coordBuf strings.Builder
		_, err := o.coordinator.Chat(ctx, agentSystem, coordHistory, input, func(chunk string) {
			coordBuf.WriteString(chunk)
		})
		if err != nil {
			if round == 0 {
				return "", fmt.Errorf("coordinator: %w", err)
			}
			// On later rounds, fall through to force-synthesis below.
			break
		}

		coordText := strings.TrimSpace(coordBuf.String())
		calls := parseDelegations(coordText)

		if len(calls) == 0 {
			// No delegation tags → this IS the final answer.
			if round > 0 {
				stream("\n[mesh] synthesis:\n\n")
			}
			stream(coordText)
			return coordText, nil
		}

		// ── show coordinator's reasoning (text that surrounds the delegate tags)
		reasoning := strings.TrimSpace(stripDelegationTags(coordText))
		if reasoning != "" {
			stream(fmt.Sprintf("[mesh] %s\n\n", reasoning))
		}

		// ── routing summary
		workerNames := make([]string, len(calls))
		for i, c := range calls {
			workerNames[i] = c.WorkerID
		}
		if len(calls) == 1 {
			stream(fmt.Sprintf("[mesh] → %s\n\n", workerNames[0]))
		} else {
			stream(fmt.Sprintf("[mesh] → %s (parallel)\n\n", strings.Join(workerNames, ", ")))
		}

		// ── execute all delegations in parallel, stream results sequentially
		results := o.executeParallel(ctx, system, calls, stream)

		// ── feed results back to coordinator
		coordHistory = append(coordHistory,
			Message{Role: "assistant", Content: coordText},
			Message{Role: "user", Content: formatDelegationResults(results)},
		)
	}

	// Max rounds hit — force a final synthesis.
	stream("\n[mesh] max delegation rounds — synthesizing…\n\n")
	var finalBuf strings.Builder
	_, err := o.coordinator.Chat(
		ctx, agentSystem, coordHistory,
		"Provide your final synthesized answer based on all results gathered so far.",
		func(chunk string) {
			stream(chunk)
			finalBuf.WriteString(chunk)
		},
	)
	return finalBuf.String(), err
}

// executeParallel runs all delegation calls concurrently.
// Workers buffer their output internally; results are streamed to the user
// sequentially in input order once all workers have finished.
// Running in parallel but displaying sequentially gives the best combination
// of throughput and readable output.
func (o *OrchestratorBackend) executeParallel(
	ctx context.Context,
	system string,
	calls []delegationCall,
	stream func(string),
) []delegationResult {
	type indexed struct {
		i int
		r delegationResult
	}

	ch := make(chan indexed, len(calls))

	for i, call := range calls {
		i, call := i, call
		we, ok := o.workerMap[call.WorkerID]
		if !ok {
			ch <- indexed{i, delegationResult{
				WorkerID: call.WorkerID,
				Output:   fmt.Sprintf("[error: worker %q not found]", call.WorkerID),
			}}
			continue
		}

		go func() {
			start := time.Now()
			var buf strings.Builder
			_, err := we.backend.Chat(ctx, system, nil, call.Task, func(chunk string) {
				buf.WriteString(chunk)
			})
			we.stats.record(float64(time.Since(start).Milliseconds()), err)
			ch <- indexed{i, delegationResult{
				WorkerID: call.WorkerID,
				Output:   buf.String(),
				Err:      err,
			}}
		}()
	}

	// Collect in any order, then sort by original index.
	results := make([]delegationResult, len(calls))
	for range calls {
		r := <-ch
		results[r.i] = r.r
	}

	// Stream in original order with clear worker separators.
	for _, r := range results {
		header := fmt.Sprintf("─── %s ───────────────────────────────────\n", r.WorkerID)
		if r.Err != nil {
			stream(header + fmt.Sprintf("[error: %v]\n\n", r.Err))
		} else {
			stream(header + r.Output + "\n\n")
		}
	}

	return results
}

// ── delegation parsing ────────────────────────────────────────────────────────

var delegateRe = regexp.MustCompile(`(?s)<delegate\s+worker="([^"]+)">(.*?)</delegate>`)

func parseDelegations(s string) []delegationCall {
	matches := delegateRe.FindAllStringSubmatch(s, -1)
	calls := make([]delegationCall, 0, len(matches))
	for _, m := range matches {
		workerID := strings.TrimSpace(m[1])
		task := strings.TrimSpace(m[2])
		if workerID != "" && task != "" {
			calls = append(calls, delegationCall{WorkerID: workerID, Task: task})
		}
	}
	return calls
}

func stripDelegationTags(s string) string {
	return delegateRe.ReplaceAllString(s, "")
}

func formatDelegationResults(results []delegationResult) string {
	var sb strings.Builder
	sb.WriteString("Worker results:\n\n")
	for _, r := range results {
		sb.WriteString(fmt.Sprintf("[Worker: %s]\n", r.WorkerID))
		if r.Err != nil {
			sb.WriteString(fmt.Sprintf("[error: %v]\n", r.Err))
		} else {
			sb.WriteString(r.Output)
		}
		sb.WriteString("\n\n")
	}
	return sb.String()
}

// ── single routing (dynamic + deterministic strategies) ───────────────────────

func (o *OrchestratorBackend) routeSingle(ctx context.Context, strategy, task string) (*workerEntry, string) {
	switch strategy {
	case "dynamic":
		w, reason, err := o.routeDynamic(ctx, task)
		if err != nil {
			w = o.fallbackWorker()
			return w, fmt.Sprintf("[mesh] routing failed (%v) — falling back to %s\n\n", err, w.cfg.ID)
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

// buildAgentSystemPrompt is used by the agentic loop ("auto" strategy).
// The coordinator receives this on every round; it explains the delegation
// protocol and lists available workers.
func (o *OrchestratorBackend) buildAgentSystemPrompt() string {
	workers := o.workerTable()
	return `You are the mesh orchestrator. You solve tasks by coordinating specialized AI workers.

Available workers:
` + workers + `
── Delegation protocol ───────────────────────────────────────────────────────

To delegate work to a specialized worker, use this exact XML syntax:

<delegate worker="WORKER_ID">
Task description for this worker — be specific and self-contained.
</delegate>

You may delegate to multiple workers at once; they execute in parallel:

<delegate worker="ar8-coder">Write the Go implementation</delegate>
<delegate worker="ar8">Write the explanation and docstring</delegate>

── Rules ─────────────────────────────────────────────────────────────────────

1. For tasks that need specialization, delegate to the best-suited worker.
2. For simple conversational questions, answer directly without delegating.
3. When you receive worker results, synthesize them into a coherent final answer.
4. If a worker result is incomplete, you may re-delegate to fix it.
5. Maximum 5 delegation rounds per request.
6. Your FINAL answer must be plain text — no <delegate> tags.
7. Worker IDs must exactly match one of the IDs listed above.
`
}

func (o *OrchestratorBackend) buildDynamicRouterPrompt() string {
	return "You are the mesh task router. Pick the best single worker for the task.\n\n" +
		"Workers:\n" + o.workerTable() +
		"\nRules:\n" +
		"- Respond with ONLY a JSON object. No preamble, no markdown.\n" +
		"- delegate_to must exactly match a worker ID above.\n" +
		"- Keep reason under 10 words.\n\n" +
		`Required format: {"delegate_to":"<worker_id>","reason":"<why>"}` + "\n"
}

// ── Commander interface ───────────────────────────────────────────────────────

func (o *OrchestratorBackend) Command(cmd string) (string, error) {
	parts := strings.Fields(strings.TrimPrefix(cmd, "/"))
	if len(parts) == 0 {
		return o.helpText(), nil
	}

	switch parts[0] {
	case "strategy":
		if len(parts) == 1 {
			return fmt.Sprintf("current strategy: %s\nvalid: auto, dynamic, capability, round-robin, fastest", o.getStrategy()), nil
		}
		if err := o.setStrategy(parts[1]); err != nil {
			return "", err
		}
		return fmt.Sprintf("strategy → %s", parts[1]), nil

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
	sb.WriteString(fmt.Sprintf("orchestrator: %s  coordinator: %s\n\n", o.id, o.cfg.Coordinator))
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
/strategy <name>       change strategy on the fly
/workers               list workers and their capabilities
/stats                 per-worker call counts and latency metrics
/help                  show this help

strategies:
  auto         full agentic loop — coordinator delegates to workers, synthesizes
               results. supports parallel delegation across multiple workers.
               (coordinator uses <delegate worker="id"> protocol)
  dynamic      single routing — coordinator picks one worker per message (fast)
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
