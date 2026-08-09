package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"agent_space/utils/mcp"
)

// Caps on what a schema load will do. The point is to remove work from the
// iteration budget, not to move an unbounded amount of it to boot.
const (
	maxSchemaDatabases = 2
	maxSchemaTables    = 12
	maxSchemaChars     = 4000
)

// systemDatabases are never application data, so describing them wastes both
// boot time and prompt space.
var systemDatabases = map[string]bool{
	"system":             true,
	"postgres":           true,
	"information_schema": true,
	"crdb_internal":      true,
	"pg_catalog":         true,
	"pg_extension":       true,
}

// internalTablePrefixes marks tables that belong to this application rather
// than to the service being diagnosed. The vector store's own tables are the
// agent's memory, not evidence about an incident.
var internalTablePrefixes = []string{"langchain_"}

// LoadClusterSchema reads the cluster's application tables once, so the model
// does not spend its iteration budget rediscovering a schema that does not
// change between runs.
//
// This is the single biggest cost in a run. Measured against the seeded demo
// cluster, four of six tool calls were list_databases, list_tables and two
// get_table_schema calls — the model only reached a question worth asking on
// call six, exactly when the iteration cap fired. Those four results are also
// re-sent on every later iteration, so they cost tokens repeatedly.
//
// Errors are not fatal and are not returned as failures of the agent: a
// cluster that cannot be described at boot simply leaves the model to discover
// it, which is the old behaviour. The caller logs and carries on.
//
// databases, when non-empty, skips discovery and describes exactly those.
func LoadClusterSchema(ctx context.Context, tools []*mcp.Tool, databases []string) (string, error) {
	byName := make(map[string]*mcp.Tool, len(tools))
	for _, t := range tools {
		byName[t.Name()] = t
	}

	listTables, haveList := byName["list_tables"]
	describe, haveDescribe := byName["get_table_schema"]
	if !haveList || !haveDescribe {
		return "", fmt.Errorf("agent: cluster schema needs list_tables and get_table_schema; server offers %s",
			strings.Join(toolNames(tools), ", "))
	}

	if len(databases) == 0 {
		discovered, err := discoverDatabases(ctx, byName["list_databases"])
		if err != nil {
			return "", err
		}
		databases = discovered
	}
	if len(databases) == 0 {
		return "", fmt.Errorf("agent: no application databases found")
	}

	var b strings.Builder
	described := 0

	for _, db := range databases {
		tables, err := tablesIn(ctx, listTables, db)
		if err != nil {
			// One unreadable database should not sink the whole description.
			continue
		}

		var rendered []string
		for _, table := range tables {
			if described >= maxSchemaTables {
				break
			}
			ddl, err := describeTable(ctx, describe, db, table)
			if err != nil || ddl == "" {
				continue
			}
			rendered = append(rendered, "  "+ddl)
			described++
		}

		if len(rendered) == 0 {
			continue
		}
		fmt.Fprintf(&b, "database %s\n%s\n", db, strings.Join(rendered, "\n"))
	}

	if described == 0 {
		return "", fmt.Errorf("agent: no application tables could be described")
	}
	return clip(strings.TrimSpace(b.String()), maxSchemaChars), nil
}

func discoverDatabases(ctx context.Context, list *mcp.Tool) ([]string, error) {
	if list == nil {
		return nil, fmt.Errorf("agent: list_databases is not offered")
	}

	res, err := list.InvokeResult(ctx, map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("agent: list databases: %w", err)
	}
	if res.IsError {
		return nil, fmt.Errorf("agent: list databases: %s", res.Text)
	}

	var names []string
	for _, row := range rowsOf(res.Text) {
		name := stringField(row, "database_name", "name", "database")
		if name == "" || systemDatabases[name] {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > maxSchemaDatabases {
		names = names[:maxSchemaDatabases]
	}
	return names, nil
}

func tablesIn(ctx context.Context, list *mcp.Tool, database string) ([]string, error) {
	res, err := list.InvokeResult(ctx, map[string]any{"database": database})
	if err != nil {
		return nil, err
	}
	if res.IsError {
		return nil, fmt.Errorf("%s", res.Text)
	}

	var names []string
	for _, row := range rowsOf(res.Text) {
		name := stringField(row, "table_name", "name", "table")
		if name == "" || isInternalTable(name) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func describeTable(ctx context.Context, describe *mcp.Tool, database, table string) (string, error) {
	res, err := describe.InvokeResult(ctx, map[string]any{"database": database, "table": table})
	if err != nil {
		return "", err
	}
	if res.IsError {
		return "", fmt.Errorf("%s", res.Text)
	}

	// Prefer the server's CREATE TABLE statement: it is exact, and it is the
	// form the model is most likely to have seen.
	for _, row := range rowsOf(res.Text) {
		if ddl := stringField(row, "create_statement", "ddl", "schema"); ddl != "" {
			return collapseWhitespace(ddl), nil
		}
	}

	// Some servers answer in prose rather than rows. Passing it through beats
	// discarding a description the model could have used.
	if text := collapseWhitespace(res.Text); text != "" {
		return table + ": " + text, nil
	}
	return "", nil
}

// rowsOf pulls the row objects out of an MCP result, which the Cloud server
// renders as {"rows":[{...}]}. A body in any other shape yields nothing, and
// callers fall back rather than failing.
func rowsOf(text string) []map[string]any {
	var payload struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return nil
	}
	return payload.Rows
}

// stringField returns the first key present as a non-empty string, so one
// reader copes with the naming differences between servers.
func stringField(row map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := row[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func isInternalTable(name string) bool {
	lower := strings.ToLower(name)
	for _, prefix := range internalTablePrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// collapseWhitespace puts a CREATE TABLE statement on one line. The schema
// block is prompt context, not something a human reads, and the newlines in a
// dozen DDL statements are pure token cost.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func toolNames(tools []*mcp.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name())
	}
	return names
}
