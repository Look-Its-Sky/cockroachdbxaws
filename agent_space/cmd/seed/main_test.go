package main

import (
	"strings"
	"testing"
)

func TestSplitStatementsEmptyStringLiteral(t *testing.T) {
	// the case that broke repos.sql: misreading '' as an escaped quote leaves
	// the splitter inside a string for the rest of the file
	sql := `UPSERT INTO t (a, b) VALUES ('x', '');
UPSERT INTO t (a, b) VALUES ('y', 'z');`

	got := splitStatements(sql)
	if len(got) != 2 {
		t.Fatalf("got %d statements, want 2:\n%#v", len(got), got)
	}
	if !strings.HasSuffix(got[0], "('x', '')") {
		t.Errorf("first statement = %q", got[0])
	}
	if !strings.Contains(got[1], "'y'") {
		t.Errorf("second statement = %q", got[1])
	}
}

func TestSplitStatementsEscapedQuote(t *testing.T) {
	// and the case the empty-literal fix must not break
	sql := `INSERT INTO t VALUES ('it''s fine; really');
INSERT INTO t VALUES ('second');`

	got := splitStatements(sql)
	if len(got) != 2 {
		t.Fatalf("got %d statements, want 2:\n%#v", len(got), got)
	}
	// the semicolon inside the literal must not have split anything
	if !strings.Contains(got[0], "it''s fine; really") {
		t.Errorf("escaped quote was mangled: %q", got[0])
	}
}

func TestSplitStatementsIgnoresComments(t *testing.T) {
	sql := `-- a comment with a semicolon; and a quote '
INSERT INTO t VALUES ('a');
-- trailing comment
INSERT INTO t VALUES ('b');`

	got := splitStatements(sql)
	if len(got) != 2 {
		t.Fatalf("got %d statements, want 2:\n%#v", len(got), got)
	}
	for _, stmt := range got {
		if strings.Contains(stmt, "comment") {
			t.Errorf("a comment survived into a statement: %q", stmt)
		}
	}
}

func TestSplitStatementsSkipsBlanks(t *testing.T) {
	if got := splitStatements(";\n\n;  ;\n"); len(got) != 0 {
		t.Errorf("got %d statements from separators alone: %#v", len(got), got)
	}
}
