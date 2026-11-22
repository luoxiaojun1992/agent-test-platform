package migrations

import (
	"strings"
	"testing"
)

func TestEmbeddedMigrationsSplit(t *testing.T) {
	t.Parallel()

	names, err := Names()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatal("no embedded migrations")
	}
	for _, name := range names {
		script, err := Files.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		statements, err := SplitStatements(string(script))
		if err != nil {
			t.Fatalf("split %s: %v", name, err)
		}
		if len(statements) == 0 {
			t.Errorf("%s contains no statements", name)
		}
	}
}

func TestSplitStatementsPreservesQuotedSemicolons(t *testing.T) {
	t.Parallel()

	script := "-- heading\nINSERT INTO `items` (`value`) VALUES ('a;b'); # tail\nSELECT \"c;d\";"
	statements, err := SplitStatements(script)
	if err != nil {
		t.Fatal(err)
	}
	if len(statements) != 2 {
		t.Fatalf("statements = %d, want 2: %q", len(statements), statements)
	}
	if !strings.Contains(statements[0], "'a;b'") || !strings.Contains(statements[1], "\"c;d\"") {
		t.Fatalf("quoted semicolons were not preserved: %q", statements)
	}
}

func TestSplitStatementsRejectsUnterminatedInput(t *testing.T) {
	t.Parallel()

	for _, script := range []string{"SELECT 'value", "SELECT 1 /* comment"} {
		if _, err := SplitStatements(script); err == nil {
			t.Errorf("SplitStatements(%q) succeeded", script)
		}
	}
}
