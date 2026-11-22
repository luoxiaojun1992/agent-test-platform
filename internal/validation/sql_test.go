package validation

import (
	"reflect"
	"strings"
	"testing"
)

func TestAllowedSQLCorpus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		query     string
		context   SQLContext
		operation string
		tables    []string
	}{
		{"simple assertion", "SELECT status FROM orders WHERE id = 'O10001'", SQLAssertion, "SELECT", []string{"orders"}},
		{"join assertion", "SELECT o.id FROM orders o JOIN customer c ON c.id = o.customer_id WHERE c.id = 'C1' ORDER BY o.id", SQLAssertion, "SELECT", []string{"customer", "orders"}},
		{"cte query", "WITH paid AS (SELECT id FROM orders WHERE status = 'PAID') SELECT id FROM paid ORDER BY id", SQLQuery, "SELECT", []string{"orders", "paid"}},
		{"deterministic function", "SELECT COALESCE(status, 'UNKNOWN') FROM orders WHERE id = 'O1'", SQLQuery, "SELECT", []string{"orders"}},
		{"insert", "INSERT INTO audit_log (id, action) VALUES ('A1', 'cancel')", SQLExecute, "INSERT", []string{"audit_log"}},
		{"update", "UPDATE orders SET status = 'CANCELLED' WHERE id = 'O1'", SQLExecute, "UPDATE", []string{"orders"}},
		{"delete", "DELETE FROM pending_jobs WHERE id = 'J1'", SQLExecute, "DELETE", []string{"pending_jobs"}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			info, err := ValidateSQL(test.query, test.context)
			if err != nil {
				t.Fatal(err)
			}
			if info.Operation != test.operation || !reflect.DeepEqual(info.Tables, test.tables) {
				t.Fatalf("info = %#v, want operation %s tables %v", info, test.operation, test.tables)
			}
			if strings.TrimSpace(info.Canonical) == "" {
				t.Fatal("canonical SQL is empty")
			}
		})
	}
}

func TestForbiddenSQLCorpus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		query   string
		context SQLContext
	}{
		{"multiple statements", "SELECT 1; DELETE FROM orders", SQLQuery},
		{"line comment", "SELECT 1 -- comment", SQLQuery},
		{"hash comment", "SELECT 1 # comment", SQLQuery},
		{"block comment", "SELECT /* comment */ 1", SQLQuery},
		{"versioned comment", "SELECT /*!80000 SQL_NO_CACHE */ * FROM orders", SQLQuery},
		{"cross database", "SELECT * FROM platform.agent", SQLQuery},
		{"locking read", "SELECT * FROM orders FOR UPDATE", SQLQuery},
		{"share lock", "SELECT * FROM orders FOR SHARE", SQLQuery},
		{"outfile", "SELECT * FROM orders INTO OUTFILE '/tmp/orders'", SQLQuery},
		{"variable read", "SELECT @secret", SQLQuery},
		{"system variable", "SELECT @@version", SQLQuery},
		{"variable assignment", "SELECT @value := id FROM orders", SQLQuery},
		{"current time", "SELECT NOW()", SQLQuery},
		{"current timestamp", "SELECT CURRENT_TIMESTAMP", SQLQuery},
		{"random", "SELECT RAND()", SQLQuery},
		{"uuid", "SELECT UUID()", SQLQuery},
		{"sleep", "SELECT SLEEP(1)", SQLQuery},
		{"write through query", "UPDATE orders SET status = 'X'", SQLQuery},
		{"read through execute", "SELECT * FROM orders", SQLExecute},
		{"replace", "REPLACE INTO orders (id) VALUES ('O1')", SQLExecute},
		{"ddl", "CREATE TABLE test (id INT)", SQLExecute},
		{"truncate", "TRUNCATE TABLE orders", SQLExecute},
		{"transaction", "START TRANSACTION", SQLExecute},
		{"set", "SET time_zone = '+00:00'", SQLExecute},
		{"load data", "LOAD DATA INFILE '/tmp/x' INTO TABLE orders", SQLExecute},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ValidateSQL(test.query, test.context); err == nil {
				t.Fatalf("forbidden SQL passed: %s", test.query)
			}
		})
	}
}

func TestCommentsInsideLiteralsAreAllowed(t *testing.T) {
	t.Parallel()

	for _, query := range []string{
		"SELECT '-- not a comment'",
		"SELECT '# not a comment'",
		"SELECT '/* not a comment */'",
	} {
		if _, err := ValidateSQL(query, SQLQuery); err != nil {
			t.Errorf("ValidateSQL(%q): %v", query, err)
		}
	}
}

func TestValidateSQLLimitsAndContext(t *testing.T) {
	t.Parallel()

	if _, err := ValidateSQL("", SQLQuery); err == nil {
		t.Fatal("empty SQL passed")
	}
	if _, err := ValidateSQL("SELECT '"+strings.Repeat("x", 4096)+"'", SQLQuery); err == nil {
		t.Fatal("oversized SQL passed")
	}
	if _, err := ValidateSQL("SELECT 1", "UNKNOWN"); err == nil {
		t.Fatal("unknown context passed")
	}
}
