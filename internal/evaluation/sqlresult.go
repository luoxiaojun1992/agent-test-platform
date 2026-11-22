package evaluation

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"strings"
	"time"
)

const (
	ScalarNull     ScalarType = "NULL"
	ScalarString   ScalarType = "STRING"
	ScalarInteger  ScalarType = "INTEGER"
	ScalarDecimal  ScalarType = "DECIMAL"
	ScalarBoolean  ScalarType = "BOOLEAN"
	ScalarDate     ScalarType = "DATE"
	ScalarDateTime ScalarType = "DATETIME"
	ScalarBinary   ScalarType = "BINARY"
	ScalarJSON     ScalarType = "JSON"
)

type ScalarType string

type SQLColumn struct {
	Name string     `json:"name"`
	Type ScalarType `json:"type"`
}

type SQLScalar struct {
	Type  ScalarType `json:"type"`
	Value any        `json:"value"`
}

type SQLResult struct {
	Columns   []SQLColumn   `json:"columns"`
	Rows      [][]SQLScalar `json:"rows"`
	Ordered   bool          `json:"ordered"`
	Truncated bool          `json:"truncated"`
}

type ResultMismatch struct {
	Path    string
	Message string
}

type ResultComparison struct {
	Pass       bool
	Mismatches []ResultMismatch
}

// DecodeSQLResult decodes one canonical SQL result and rejects trailing JSON.
func DecodeSQLResult(data []byte) (SQLResult, error) {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	var result SQLResult
	if err := decoder.Decode(&result); err != nil {
		return SQLResult{}, fmt.Errorf("decode SQL result: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return SQLResult{}, fmt.Errorf("decode SQL result: trailing JSON value")
		}
		return SQLResult{}, fmt.Errorf("decode SQL result: trailing data: %w", err)
	}
	if err := ValidateSQLResult(result); err != nil {
		return SQLResult{}, err
	}
	return result, nil
}

// ValidateSQLResult enforces the canonical representation independently of JSON Schema.
func ValidateSQLResult(result SQLResult) error {
	if result.Truncated {
		return fmt.Errorf("truncated SQL result is not valid assertion evidence")
	}
	if len(result.Rows) > 1 && !result.Ordered {
		return fmt.Errorf("multi-row SQL result must be ordered")
	}
	seenColumns := make(map[string]struct{}, len(result.Columns))
	for columnIndex, column := range result.Columns {
		if strings.TrimSpace(column.Name) == "" {
			return fmt.Errorf("column %d has no name", columnIndex)
		}
		canonicalName := strings.ToLower(strings.TrimSpace(column.Name))
		if _, exists := seenColumns[canonicalName]; exists {
			return fmt.Errorf("column %d duplicates name %q", columnIndex, column.Name)
		}
		seenColumns[canonicalName] = struct{}{}
		if !knownScalarType(column.Type) || column.Type == ScalarNull {
			return fmt.Errorf("column %s has invalid type %q", column.Name, column.Type)
		}
	}
	for rowIndex, row := range result.Rows {
		if len(row) != len(result.Columns) {
			return fmt.Errorf("row %d has %d cells, want %d", rowIndex, len(row), len(result.Columns))
		}
		for columnIndex, scalar := range row {
			column := result.Columns[columnIndex]
			if scalar.Type != ScalarNull && scalar.Type != column.Type {
				return fmt.Errorf("row %d column %s type %s does not match %s", rowIndex, column.Name, scalar.Type, column.Type)
			}
			if _, err := canonicalScalar(scalar); err != nil {
				return fmt.Errorf("row %d column %s: %w", rowIndex, column.Name, err)
			}
		}
	}
	return nil
}

// CompareSQLResults compares typed rows exactly, with optional absolute Decimal tolerance.
func CompareSQLResults(expected, actual SQLResult, decimalTolerance string) (ResultComparison, error) {
	if err := ValidateSQLResult(expected); err != nil {
		return ResultComparison{}, fmt.Errorf("expected result: %w", err)
	}
	if err := ValidateSQLResult(actual); err != nil {
		return ResultComparison{Pass: false, Mismatches: []ResultMismatch{{Path: "$", Message: err.Error()}}}, nil
	}
	tolerance, err := parseTolerance(decimalTolerance)
	if err != nil {
		return ResultComparison{}, err
	}

	comparison := ResultComparison{Pass: true}
	if len(expected.Columns) != len(actual.Columns) {
		comparison.add("$.columns", fmt.Sprintf("count %d does not match %d", len(actual.Columns), len(expected.Columns)))
		return comparison, nil
	}
	for index, expectedColumn := range expected.Columns {
		actualColumn := actual.Columns[index]
		if !strings.EqualFold(expectedColumn.Name, actualColumn.Name) {
			comparison.add(fmt.Sprintf("$.columns[%d].name", index), fmt.Sprintf("%q does not match %q", actualColumn.Name, expectedColumn.Name))
		}
		if expectedColumn.Type != actualColumn.Type {
			comparison.add(fmt.Sprintf("$.columns[%d].type", index), fmt.Sprintf("%s does not match %s", actualColumn.Type, expectedColumn.Type))
		}
	}
	if len(expected.Rows) != len(actual.Rows) {
		comparison.add("$.rows", fmt.Sprintf("count %d does not match %d", len(actual.Rows), len(expected.Rows)))
		return comparison, nil
	}
	for rowIndex := range expected.Rows {
		for columnIndex := range expected.Rows[rowIndex] {
			path := fmt.Sprintf("$.rows[%d][%d]", rowIndex, columnIndex)
			equal, err := scalarEqual(expected.Rows[rowIndex][columnIndex], actual.Rows[rowIndex][columnIndex], tolerance)
			if err != nil {
				return ResultComparison{}, fmt.Errorf("%s: %w", path, err)
			}
			if !equal {
				comparison.add(path, "actual scalar does not match expected scalar")
			}
		}
	}
	return comparison, nil
}

func (comparison *ResultComparison) add(path, message string) {
	comparison.Pass = false
	comparison.Mismatches = append(comparison.Mismatches, ResultMismatch{Path: path, Message: message})
}

func scalarEqual(expected, actual SQLScalar, tolerance *big.Rat) (bool, error) {
	if expected.Type != actual.Type {
		return false, nil
	}
	expectedValue, err := canonicalScalar(expected)
	if err != nil {
		return false, err
	}
	actualValue, err := canonicalScalar(actual)
	if err != nil {
		return false, err
	}
	if expected.Type != ScalarDecimal || tolerance == nil {
		return expectedValue == actualValue, nil
	}
	left, _ := new(big.Rat).SetString(expectedValue)
	right, _ := new(big.Rat).SetString(actualValue)
	difference := new(big.Rat).Sub(left, right)
	if difference.Sign() < 0 {
		difference.Neg(difference)
	}
	return difference.Cmp(tolerance) <= 0, nil
}

func canonicalScalar(scalar SQLScalar) (string, error) {
	if !knownScalarType(scalar.Type) {
		return "", fmt.Errorf("unknown scalar type %q", scalar.Type)
	}
	if scalar.Type == ScalarNull {
		if scalar.Value != nil {
			return "", fmt.Errorf("NULL scalar must have null value")
		}
		return "null", nil
	}
	if scalar.Value == nil {
		return "", fmt.Errorf("%s scalar cannot have null value", scalar.Type)
	}

	switch scalar.Type {
	case ScalarString:
		value, ok := scalar.Value.(string)
		if !ok {
			return "", fmt.Errorf("STRING value must be a string")
		}
		return value, nil
	case ScalarInteger:
		value, ok := scalar.Value.(string)
		if !ok {
			return "", fmt.Errorf("INTEGER value must be a base-10 string")
		}
		integer := new(big.Int)
		if _, ok := integer.SetString(value, 10); !ok || integer.String() != value {
			return "", fmt.Errorf("INTEGER value %q is not canonical", value)
		}
		return value, nil
	case ScalarDecimal:
		value, ok := scalar.Value.(string)
		if !ok || strings.ContainsAny(value, "eE") {
			return "", fmt.Errorf("DECIMAL value must be a base-10 string")
		}
		if _, ok := new(big.Rat).SetString(value); !ok {
			return "", fmt.Errorf("DECIMAL value %q is invalid", value)
		}
		return value, nil
	case ScalarBoolean:
		value, ok := scalar.Value.(bool)
		if !ok {
			return "", fmt.Errorf("BOOLEAN value must be a boolean")
		}
		if value {
			return "true", nil
		}
		return "false", nil
	case ScalarDate:
		value, ok := scalar.Value.(string)
		if !ok {
			return "", fmt.Errorf("DATE value must be a string")
		}
		if _, err := time.Parse("2006-01-02", value); err != nil {
			return "", fmt.Errorf("DATE value: %w", err)
		}
		return value, nil
	case ScalarDateTime:
		value, ok := scalar.Value.(string)
		if !ok {
			return "", fmt.Errorf("DATETIME value must be a string")
		}
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return "", fmt.Errorf("DATETIME value must be RFC3339 UTC")
		}
		_, offset := parsed.Zone()
		if offset != 0 {
			return "", fmt.Errorf("DATETIME value must be RFC3339 UTC")
		}
		return parsed.UTC().Format(time.RFC3339Nano), nil
	case ScalarBinary:
		value, ok := scalar.Value.(string)
		if !ok {
			return "", fmt.Errorf("BINARY value must be unpadded base64")
		}
		decoded, err := base64.RawStdEncoding.DecodeString(value)
		if err != nil || base64.RawStdEncoding.EncodeToString(decoded) != value {
			return "", fmt.Errorf("BINARY value is not canonical unpadded base64")
		}
		return value, nil
	case ScalarJSON:
		encoded, err := json.Marshal(scalar.Value)
		if err != nil {
			return "", fmt.Errorf("JSON value: %w", err)
		}
		return string(encoded), nil
	default:
		return "", fmt.Errorf("unsupported scalar type %q", scalar.Type)
	}
}

func parseTolerance(value string) (*big.Rat, error) {
	if value == "" {
		return nil, nil
	}
	tolerance, ok := new(big.Rat).SetString(value)
	if !ok || tolerance.Sign() < 0 || strings.ContainsAny(value, "eE") {
		return nil, fmt.Errorf("decimal tolerance %q is invalid", value)
	}
	return tolerance, nil
}

func knownScalarType(scalarType ScalarType) bool {
	switch scalarType {
	case ScalarNull, ScalarString, ScalarInteger, ScalarDecimal, ScalarBoolean,
		ScalarDate, ScalarDateTime, ScalarBinary, ScalarJSON:
		return true
	default:
		return false
	}
}
