// Command seed applies a .sql file to DATABASE_URL, so seed-cluster.sql can reach CockroachDB Cloud without docker.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"agent_space/utils"
)

func main() {
	var (
		file        = flag.String("file", "", "SQL file to apply (default scripts/seed-cluster.sql next to this module)")
		assumeYes   = flag.Bool("yes", false, "skip the confirmation prompt")
		databaseURL = flag.String("database-url", "", "override DATABASE_URL")
		timeout     = flag.Duration("timeout", 5*time.Minute, "overall timeout")
	)
	flag.Parse()

	utils.LoadConfig()

	path := *file
	if path == "" {
		path = defaultSeedPath()
	}
	sql, err := os.ReadFile(path)
	if err != nil {
		fail(fmt.Errorf("read %s: %w", path, err))
	}

	connStr := *databaseURL
	if connStr == "" {
		connStr = os.Getenv("DATABASE_URL")
	}
	if connStr == "" {
		fail(errors.New("DATABASE_URL is not set in the environment or .env file"))
	}

	// the script starts with DROP TABLE, so show the target before doing it
	fmt.Printf("target:  %s\n", utils.RedactURL(connStr))
	fmt.Printf("file:    %s (%d bytes)\n", path, len(sql))
	fmt.Printf("note:    this DROPs and recreates the tables the script names\n\n")

	if !*assumeYes && !confirm() {
		fmt.Println("aborted")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if err := run(ctx, connStr, string(sql)); err != nil {
		fail(err)
	}
}

func run(ctx context.Context, connStr, sql string) error {
	cfg, err := pgx.ParseConfig(connStr)
	if err != nil {
		return fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	// safe to relax: the SQL comes from a file on disk, not from input
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)

	// one statement per Exec; batched they share an implicit transaction and CockroachDB will not drop and recreate a table inside one
	statements := splitStatements(sql)
	if len(statements) == 0 {
		return errors.New("no SQL statements found in the file")
	}

	for i, stmt := range statements {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("statement %d of %d (%s): %w", i+1, len(statements), firstLine(stmt), err)
		}
	}

	fmt.Printf("Applied %d statements.\n", len(statements))
	summarise(ctx, conn)
	return nil
}

// split on top-level semicolons, ignoring string literals and -- comments; handles this repo's scripts, not arbitrary SQL
func splitStatements(sql string) []string {
	var (
		out       []string
		current   strings.Builder
		inString  bool
		inComment bool
	)

	for i := 0; i < len(sql); i++ {
		c := sql[i]

		if inComment {
			if c == '\n' {
				inComment = false
				current.WriteByte(c)
			}
			continue
		}

		if !inString && c == '-' && i+1 < len(sql) && sql[i+1] == '-' {
			inComment = true
			i++
			continue
		}

		if c == '\'' {
			// '' inside a string is an escaped quote, not a terminator. Decided
			// by looking ahead rather than by remembering the previous
			// character: the latter cannot tell an escaped quote from the empty
			// literal '', and would leave the splitter stuck inside a string.
			if inString && i+1 < len(sql) && sql[i+1] == '\'' {
				current.WriteString("''")
				i++
				continue
			}
			inString = !inString
			current.WriteByte(c)
			continue
		}

		if c == ';' && !inString {
			if s := strings.TrimSpace(current.String()); s != "" {
				out = append(out, s)
			}
			current.Reset()
			continue
		}

		current.WriteByte(c)
	}

	if s := strings.TrimSpace(current.String()); s != "" {
		out = append(out, s)
	}
	return out
}

// firstLine keeps an error message short enough to read.
func firstLine(stmt string) string {
	line := strings.TrimSpace(strings.SplitN(stmt, "\n", 2)[0])
	if len(line) > 60 {
		return line[:60] + "…"
	}
	return line
}

// report what landed, so a silent success is distinguishable from seeding nothing
func summarise(ctx context.Context, conn *pgx.Conn) {
	rows, err := conn.Query(ctx, `
		SELECT table_name
		FROM [SHOW TABLES]
		ORDER BY table_name`)
	if err != nil {
		return
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err == nil {
			tables = append(tables, name)
		}
	}
	if len(tables) == 0 {
		return
	}

	fmt.Println("\nTables now present:")
	for _, t := range tables {
		var n int64
		// Table names come from SHOW TABLES, not from user input.
		if err := conn.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %q", t)).Scan(&n); err != nil {
			fmt.Printf("  %-28s ?\n", t)
			continue
		}
		fmt.Printf("  %-28s %d rows\n", t, n)
	}
}

// find scripts/seed-cluster.sql relative to the working directory so no flag is needed
func defaultSeedPath() string {
	candidates := []string{
		filepath.Join("scripts", "seed-cluster.sql"),
		filepath.Join("agent_space", "scripts", "seed-cluster.sql"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return candidates[0]
}

func confirm() bool {
	fmt.Print("Type 'seed' to proceed: ")
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return false
	}
	return strings.TrimSpace(scanner.Text()) == "seed"
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "seed: %v\n", err)
	os.Exit(1)
}
