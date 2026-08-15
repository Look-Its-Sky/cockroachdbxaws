package mcp

import (
	"log"
	"sort"
	"strings"
)

// tool prefixes that mutate a cluster, used only on the fallback path
var writeVerbs = []string{
	"create_", "drop_", "delete_", "insert_", "update_", "alter_", "truncate_",
	"grant_", "revoke_", "set_", "add_", "remove_", "restore_", "import_",
}

// the subset of tools that cannot modify the cluster. Prefers the server's readOnlyHint and is fail-closed, since the undeclared tools are the dangerous ones; falls back to name matching when no tool declares the hint, and logs that it guessed.
func ReadOnly(tools []*Tool) []*Tool {
	annotated := 0
	for _, t := range tools {
		if a := t.Annotations(); a != nil && a.ReadOnlyHint {
			annotated++
		}
	}

	kept := make([]*Tool, 0, len(tools))
	var dropped []string

	for _, t := range tools {
		var safe bool
		if annotated > 0 {
			a := t.Annotations()
			safe = a != nil && a.ReadOnlyHint
		} else {
			safe = !looksLikeWrite(t.Name())
		}

		if safe {
			kept = append(kept, t)
		} else {
			dropped = append(dropped, t.Name())
		}
	}

	if annotated == 0 && len(tools) > 0 {
		log.Printf("MCP: no tool declares readOnlyHint; falling back to name matching to exclude write tools")
	}
	if len(dropped) > 0 {
		sort.Strings(dropped)
		log.Printf("MCP: withholding %d write %s from the agent: %s",
			len(dropped), plural(len(dropped), "tool", "tools"), strings.Join(dropped, ", "))
	}

	return kept
}

func looksLikeWrite(name string) bool {
	lower := strings.ToLower(name)
	for _, verb := range writeVerbs {
		if strings.HasPrefix(lower, verb) {
			return true
		}
	}
	return false
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// read-only tools the agent is still not offered: cluster introspection the prompt already discourages, and show_statement does not return inside the 90s ceiling on a Basic cluster
var DefaultExcluded = []string{
	"show_statement",
	"show_running_queries",
}

// tools whose names are not in the list, matched exactly so a prefix cannot take out neighbours
func Exclude(tools []*Tool, names []string) []*Tool {
	if len(names) == 0 {
		return tools
	}

	blocked := make(map[string]bool, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			blocked[n] = true
		}
	}

	kept := make([]*Tool, 0, len(tools))
	var dropped []string
	for _, t := range tools {
		if blocked[t.Name()] {
			dropped = append(dropped, t.Name())
			continue
		}
		kept = append(kept, t)
	}

	if len(dropped) > 0 {
		sort.Strings(dropped)
		log.Printf("MCP: withholding %d introspection %s from the agent: %s",
			len(dropped), plural(len(dropped), "tool", "tools"), strings.Join(dropped, ", "))
	}
	return kept
}
