package mcp

import (
	"log"
	"sort"
	"strings"
)

// writeVerbs names the tool prefixes that mutate a cluster. This is only the
// fallback path — see ReadOnly.
var writeVerbs = []string{
	"create_", "drop_", "delete_", "insert_", "update_", "alter_", "truncate_",
	"grant_", "revoke_", "set_", "add_", "remove_", "restore_", "import_",
}

// ReadOnly returns the subset of tools that cannot modify the cluster.
//
// The agent's job is diagnosis: read the cluster, decide rollback or hotfix,
// and report. Handing it create_table, delete_rows or update_rows offers it no
// capability it needs while making a wrong tool call destructive. A model that
// misreads "roll back the deploy" as "roll back the data" should not be able to
// act on that.
//
// The filter prefers the server's own readOnlyHint annotation, which is
// authoritative. It is deliberately fail-closed: a tool that declares nothing
// is treated as a write, because on cockroachdb-mcp-server 0.1.0 exactly the
// undeclared tools — create_database, create_table, insert_rows — are the
// dangerous ones.
//
// If no tool declares the hint at all, the server does not publish annotations
// and fail-closed would leave the agent with nothing. That case falls back to
// matching names against writeVerbs, and says so in the log, because a silent
// downgrade from "the server told us" to "we guessed" is worth seeing.
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

// DefaultExcluded names read-only tools the SRE agent should not be offered.
//
// These are safe — they cannot modify anything — but they are cluster
// introspection, and the system prompt already tells the model to leave that
// alone: it returns large results that crowd out the incident context and
// rarely bears on whether to roll a commit back. Telling the model was not
// enough. It reached for show_statement anyway, and against a CockroachDB Cloud
// Basic cluster that call does not return inside the client's 90s ceiling, so a
// run that was moments from an answer died on a tool it had been told not to
// use. Removing them makes the instruction structural rather than advisory.
//
// This is a policy about this agent, not about the tools: nothing here is
// dangerous, and a different agent might reasonably want all of them.
var DefaultExcluded = []string{
	"show_statement",
	"show_running_queries",
}

// Exclude returns the tools whose names are not in the list. Names are matched
// exactly, because a prefix match here would silently take out neighbours.
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
