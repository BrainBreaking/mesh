package tui

import "strings"

// suggestion is a single autocomplete entry shown in the popup.
type suggestion struct {
	full string // text to insert when accepted
	desc string // short description shown alongside
}

// cmdDef describes one slash command available for completion.
type cmdDef struct {
	cmd      string
	desc     string
	orchOnly bool // only offered when the backend implements Commander
}

// allCmdDefs is the canonical command list, in display order.
var allCmdDefs = []cmdDef{
	{"/help", "list available commands", false},
	{"/clear", "clear the conversation", false},
	{"/strategy", "show or change routing strategy", true},
	{"/strategy dynamic", "route based on per-request analysis", true},
	{"/strategy capability", "match keywords to capability tags", true},
	{"/strategy round-robin", "distribute load evenly across workers", true},
	{"/strategy fastest", "prefer lowest-latency worker", true},
	{"/strategy auto", "coordinator picks strategy + worker", true},
	{"/workers", "show worker pool table", true},
	{"/stats", "show per-backend latency metrics", true},
}

// buildSuggestions returns completions for the current textarea input.
// Returns nil when the popup should not be shown (empty, no "/" prefix,
// exact match with nothing further to offer, or no matches).
func buildSuggestions(input string, hasCommander bool) []suggestion {
	if input == "" || !strings.HasPrefix(input, "/") {
		return nil
	}
	lower := strings.ToLower(input)
	var out []suggestion
	for _, c := range allCmdDefs {
		if c.orchOnly && !hasCommander {
			continue
		}
		cmdLower := strings.ToLower(c.cmd)
		// Include commands that extend the typed prefix but are not identical.
		if strings.HasPrefix(cmdLower, lower) && cmdLower != lower {
			out = append(out, suggestion{full: c.cmd, desc: c.desc})
		}
	}
	return out
}
