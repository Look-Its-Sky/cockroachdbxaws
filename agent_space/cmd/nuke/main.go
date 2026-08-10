// Command nuke empties or drops the vector store tables; talks to the database directly so it needs no LLM credentials.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"agent_space/utils"
	"agent_space/utils/crdbvector"
)

// children first: langchain_pg_embedding has a foreign key onto langchain_pg_collection
var tables = []string{
	crdbvector.DefaultEmbeddingStoreTableName,
	crdbvector.DefaultCollectionStoreTableName,
}

func main() {
	var (
		mode        = flag.String("mode", "truncate", "truncate (empty the tables) or drop (remove them)")
		assumeYes   = flag.Bool("yes", false, "skip the confirmation prompt")
		databaseURL = flag.String("database-url", "", "override DATABASE_URL")
		timeout     = flag.Duration("timeout", 60*time.Second, "overall timeout")
	)
	flag.Parse()

	if *mode != "truncate" && *mode != "drop" {
		fail(fmt.Errorf("unknown -mode %q: want truncate or drop", *mode))
	}

	utils.LoadConfig()
	connStr := *databaseURL
	if connStr == "" {
		connStr = os.Getenv("DATABASE_URL")
	}
	if connStr == "" {
		fail(errors.New("DATABASE_URL is not set in the environment or .env file"))
	}

	// print the target first so an accidental run against production is visible before it happens
	fmt.Printf("target:  %s\n", utils.RedactURL(connStr))
	fmt.Printf("mode:    %s\n", *mode)
	fmt.Printf("tables:  %s\n\n", strings.Join(tables, ", "))

	if !*assumeYes && !confirm() {
		fmt.Println("aborted")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if err := run(ctx, connStr, *mode); err != nil {
		fail(err)
	}
}

func run(ctx context.Context, connStr, mode string) error {
	conn, err := pgx.Connect(ctx, connStr)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)

	for _, table := range tables {
		before, err := rowCount(ctx, conn, table)
		if err != nil {
			// A table that was never created is already in the desired state.
			fmt.Printf("  %-28s absent, nothing to do\n", table)
			continue
		}

		stmt, action := fmt.Sprintf("DELETE FROM %s", table), "emptied"
		if mode == "drop" {
			stmt, action = fmt.Sprintf("DROP TABLE IF EXISTS %s CASCADE", table), "dropped"
		}

		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
		fmt.Printf("  %-28s %s (%d rows)\n", table, action, before)
	}

	fmt.Println("\nDone.")
	if mode == "drop" {
		fmt.Printf("The tables are recreated on the next server start, sized to %d.\n", utils.VectorDimensions())
	}
	return nil
}

// rowCount doubles as an existence check: an error means the table is not there.
func rowCount(ctx context.Context, conn *pgx.Conn, table string) (int64, error) {
	var n int64
	// The table names are package constants, not user input.
	err := conn.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s", table)).Scan(&n)
	return n, err
}

func confirm() bool {
	fmt.Print("Type 'nuke' to proceed: ")
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return false
	}
	return strings.TrimSpace(scanner.Text()) == "nuke"
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "nuke: %v\n", err)
	os.Exit(1)
}
