package evaluation

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"agent-test-platform/internal/validation"
)

const maxAssertionRows = 1_000

type Queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type SQLAssertion struct {
	ID               string
	Query            string
	Expected         SQLResult
	Critical         bool
	Weight           *float64
	DecimalTolerance string
}

type AssertionResult struct {
	ID         string
	Critical   bool
	Weight     *float64
	Pass       bool
	Expected   SQLResult
	Actual     SQLResult
	Mismatches []ResultMismatch
}

// ExecuteSQLAssertion validates, executes, normalizes, and compares one read-only assertion.
func ExecuteSQLAssertion(ctx context.Context, database Queryer, assertion SQLAssertion) (AssertionResult, error) {
	if strings.TrimSpace(assertion.ID) == "" {
		return AssertionResult{}, fmt.Errorf("assertion ID is required")
	}
	if err := ValidateSQLResult(assertion.Expected); err != nil {
		return AssertionResult{}, fmt.Errorf("assertion %s expected result: %w", assertion.ID, err)
	}
	info, err := validation.ValidateSQL(assertion.Query, validation.SQLAssertion)
	if err != nil {
		return AssertionResult{}, fmt.Errorf("assertion %s SQL: %w", assertion.ID, err)
	}
	if len(assertion.Expected.Rows) > 1 && !info.Ordered {
		return AssertionResult{}, fmt.Errorf("assertion %s expects multiple rows but has no ORDER BY", assertion.ID)
	}

	rows, err := database.QueryContext(ctx, info.Canonical)
	if err != nil {
		return AssertionResult{}, fmt.Errorf("execute assertion %s: %w", assertion.ID, err)
	}
	defer rows.Close()
	actual, err := scanAssertionRows(rows, assertion.Expected.Columns, info.Ordered)
	if err != nil {
		return AssertionResult{}, fmt.Errorf("scan assertion %s: %w", assertion.ID, err)
	}
	comparison, err := CompareSQLResults(assertion.Expected, actual, assertion.DecimalTolerance)
	if err != nil {
		return AssertionResult{}, fmt.Errorf("compare assertion %s: %w", assertion.ID, err)
	}
	return AssertionResult{
		ID: assertion.ID, Critical: assertion.Critical, Weight: assertion.Weight,
		Pass: comparison.Pass, Expected: assertion.Expected, Actual: actual,
		Mismatches: comparison.Mismatches,
	}, nil
}

func scanAssertionRows(rows *sql.Rows, expectedColumns []SQLColumn, ordered bool) (SQLResult, error) {
	actualNames, err := rows.Columns()
	if err != nil {
		return SQLResult{}, err
	}
	if len(actualNames) != len(expectedColumns) {
		return SQLResult{}, fmt.Errorf("column count %d does not match expected %d", len(actualNames), len(expectedColumns))
	}
	result := SQLResult{Columns: make([]SQLColumn, len(expectedColumns)), Ordered: ordered}
	for index, expected := range expectedColumns {
		result.Columns[index] = SQLColumn{Name: actualNames[index], Type: expected.Type}
	}

	for rows.Next() {
		if len(result.Rows) >= maxAssertionRows {
			return SQLResult{}, fmt.Errorf("row limit %d exceeded", maxAssertionRows)
		}
		raw := make([]sql.RawBytes, len(expectedColumns))
		targets := make([]any, len(raw))
		for index := range raw {
			targets[index] = &raw[index]
		}
		if err := rows.Scan(targets...); err != nil {
			return SQLResult{}, err
		}
		row := make([]SQLScalar, len(raw))
		for index, value := range raw {
			scalar, err := scalarFromRaw(expectedColumns[index].Type, value)
			if err != nil {
				return SQLResult{}, fmt.Errorf("column %s: %w", expectedColumns[index].Name, err)
			}
			row[index] = scalar
		}
		result.Rows = append(result.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return SQLResult{}, err
	}
	return result, nil
}

func scalarFromRaw(scalarType ScalarType, raw sql.RawBytes) (SQLScalar, error) {
	if raw == nil {
		return SQLScalar{Type: ScalarNull, Value: nil}, nil
	}
	text := string(raw)
	scalar := SQLScalar{Type: scalarType}
	switch scalarType {
	case ScalarString, ScalarInteger, ScalarDecimal, ScalarDate:
		scalar.Value = text
	case ScalarBoolean:
		switch strings.ToLower(text) {
		case "1", "true":
			scalar.Value = true
		case "0", "false":
			scalar.Value = false
		default:
			return SQLScalar{}, fmt.Errorf("invalid BOOLEAN value %q", text)
		}
	case ScalarDateTime:
		parsed, err := parseDatabaseDateTime(text)
		if err != nil {
			return SQLScalar{}, err
		}
		scalar.Value = parsed.UTC().Format(time.RFC3339Nano)
	case ScalarBinary:
		scalar.Value = base64.RawStdEncoding.EncodeToString(raw)
	case ScalarJSON:
		decoder := json.NewDecoder(strings.NewReader(text))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return SQLScalar{}, fmt.Errorf("invalid JSON value: %w", err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			return SQLScalar{}, fmt.Errorf("invalid JSON value: trailing data")
		}
		scalar.Value = value
	default:
		return SQLScalar{}, fmt.Errorf("unsupported scalar type %q", scalarType)
	}
	if _, err := canonicalScalar(scalar); err != nil {
		return SQLScalar{}, err
	}
	return scalar, nil
}

func parseDatabaseDateTime(value string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02 15:04:05.999999", "2006-01-02 15:04:05", time.RFC3339Nano} {
		parsed, err := time.ParseInLocation(layout, value, time.UTC)
		if err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid UTC DATETIME value %q", value)
}
