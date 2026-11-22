package evaluation

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

type fakeQueryer struct {
	err error
}

func (fake *fakeQueryer) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, fake.err
}

func TestExecuteSQLAssertionRejectsUnsafeSQLBeforeQuery(t *testing.T) {
	t.Parallel()

	fake := &fakeQueryer{err: errors.New("query should not run")}
	assertion := SQLAssertion{
		ID: "unsafe", Query: "SELECT 1; DELETE FROM orders", Critical: true,
		Expected: SQLResult{Columns: []SQLColumn{{Name: "value", Type: ScalarInteger}}, Ordered: true},
	}
	if _, err := ExecuteSQLAssertion(context.Background(), fake, assertion); err == nil || err.Error() == "query should not run" {
		t.Fatalf("unsafe assertion error = %v", err)
	}
}

func TestExecuteSQLAssertionRequiresOrderForMultipleExpectedRows(t *testing.T) {
	t.Parallel()

	fake := &fakeQueryer{err: errors.New("query should not run")}
	assertion := SQLAssertion{
		ID: "unordered", Query: "SELECT id FROM orders", Critical: true,
		Expected: SQLResult{
			Columns: []SQLColumn{{Name: "id", Type: ScalarString}},
			Rows:    [][]SQLScalar{{{Type: ScalarString, Value: "O1"}}, {{Type: ScalarString, Value: "O2"}}},
			Ordered: true,
		},
	}
	if _, err := ExecuteSQLAssertion(context.Background(), fake, assertion); err == nil {
		t.Fatal("multi-row assertion without ORDER BY was accepted")
	}
}

func TestScalarFromRaw(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		scalarType ScalarType
		raw        sql.RawBytes
	}{
		{"null", ScalarString, nil},
		{"integer", ScalarInteger, []byte("42")},
		{"decimal", ScalarDecimal, []byte("10.00")},
		{"boolean", ScalarBoolean, []byte("1")},
		{"datetime", ScalarDateTime, []byte("2026-09-11 10:00:00.123456")},
		{"json", ScalarJSON, []byte(`{"b":true,"a":1}`)},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			scalar, err := scalarFromRaw(test.scalarType, test.raw)
			if err != nil {
				t.Fatal(err)
			}
			if canonical, _ := canonicalScalar(scalar); canonical == "" && test.raw != nil {
				t.Fatalf("empty canonical scalar: %#v", scalar)
			}
		})
	}
}

func TestExecuteSQLAssertion(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	canonicalQuery := "select `status`, amount from orders where id = 'O10001'"
	mock.ExpectQuery(regexp.QuoteMeta(canonicalQuery)).WillReturnRows(
		sqlmock.NewRows([]string{"status", "amount"}).AddRow("CANCELLED", "10.01"),
	)
	assertion := SQLAssertion{
		ID: "order-is-cancelled", Query: "SELECT status, amount FROM orders WHERE id = 'O10001'",
		Critical: true, DecimalTolerance: "0.01",
		Expected: SQLResult{
			Columns: []SQLColumn{{Name: "status", Type: ScalarString}, {Name: "amount", Type: ScalarDecimal}},
			Rows:    [][]SQLScalar{{{Type: ScalarString, Value: "CANCELLED"}, {Type: ScalarDecimal, Value: "10.00"}}},
			Ordered: true,
		},
	}
	result, err := ExecuteSQLAssertion(context.Background(), database, assertion)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Pass || !result.Critical {
		t.Fatalf("result = %#v", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestExecuteSQLAssertionReportsMismatch(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectQuery(regexp.QuoteMeta("select `status` from orders where id = 'O10001'")).WillReturnRows(
		sqlmock.NewRows([]string{"status"}).AddRow("PAID"),
	)
	result, err := ExecuteSQLAssertion(context.Background(), database, SQLAssertion{
		ID: "status", Query: "SELECT status FROM orders WHERE id = 'O10001'",
		Expected: SQLResult{
			Columns: []SQLColumn{{Name: "status", Type: ScalarString}},
			Rows:    [][]SQLScalar{{{Type: ScalarString, Value: "CANCELLED"}}}, Ordered: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Pass || len(result.Mismatches) != 1 {
		t.Fatalf("mismatch result = %#v", result)
	}
}

func TestScalarFromRawRejectsTrailingJSON(t *testing.T) {
	t.Parallel()

	if _, err := scalarFromRaw(ScalarJSON, []byte(`{"ok":true} trailing`)); err == nil {
		t.Fatal("JSON with trailing data was accepted")
	}
}
