package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"agent_space/utils/mcp"
)

// caps on schema
const (
	maxSchemaDatabases = 2
	maxSchemaTables    = 12
	maxSchemaChars     = 4000
)

var systemDatabases = map[string]bool{
	"system":             true,
	"postgres":           true,
	"information_schema": true,
	"crdb_internal":      true,
	"pg_catalog":         true,
	"pg_extension":       true,
}

var internalTablePrefixes = []string{"langchain_"}

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

	for _, row := range rowsOf(res.Text) {
		if ddl := stringField(row, "create_statement", "ddl", "schema"); ddl != "" {
			return collapseWhitespace(ddl), nil
		}
	}

	if text := collapseWhitespace(res.Text); text != "" {
		return table + ": " + text, nil
	}
	return "", nil
}

func rowsOf(text string) []map[string]any {
	var payload struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return nil
	}
	return payload.Rows
}

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
