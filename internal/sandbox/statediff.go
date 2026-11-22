package sandbox

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	OperationInsert Operation = "INSERT"
	OperationUpdate Operation = "UPDATE"
	OperationDelete Operation = "DELETE"

	CellNull     CellType = "NULL"
	CellString   CellType = "STRING"
	CellInteger  CellType = "INTEGER"
	CellDecimal  CellType = "DECIMAL"
	CellBoolean  CellType = "BOOLEAN"
	CellDate     CellType = "DATE"
	CellDateTime CellType = "DATETIME"
	CellBinary   CellType = "BINARY"
	CellJSON     CellType = "JSON"
)

type Operation string
type CellType string

// Cell stores one already-canonicalized database scalar.
type Cell struct {
	Type  CellType
	Value string
}

type Row map[string]Cell
type TableSnapshot map[string]Row

// StateSnapshot is the logical state captured before or after Agent execution.
type StateSnapshot struct {
	SchemaDigest string
	Tables       map[string]TableSnapshot
}

// Change describes one primary-keyed row mutation.
type Change struct {
	Table     string
	Key       string
	Operation Operation
	Columns   []string
	Before    Row
	After     Row
}

type StateDiff struct {
	SchemaChanged bool
	Changes       []Change
}

type Predicate struct {
	Column   string
	Operator string
	Value    any
}

type MutationRule struct {
	ID              string
	Table           string
	Operation       Operation
	Columns         []string
	Predicate       Predicate
	MinAffectedRows int
	MaxAffectedRows int
}

type Violation struct {
	Code    string
	RuleID  string
	Table   string
	Key     string
	Message string
}

type MutationReport struct {
	Pass       bool
	RuleCounts map[string]int
	Violations []Violation
}

// NewCell validates and canonicalizes one database scalar.
func NewCell(cellType CellType, value any) (Cell, error) {
	canonical, err := canonicalValue(cellType, value)
	if err != nil {
		return Cell{}, err
	}
	return Cell{Type: cellType, Value: canonical}, nil
}

// DiffStates computes a stable row-level diff between two snapshots.
func DiffStates(before, after StateSnapshot) (StateDiff, error) {
	canonicalBefore, err := canonicalSnapshot(before)
	if err != nil {
		return StateDiff{}, fmt.Errorf("before snapshot: %w", err)
	}
	canonicalAfter, err := canonicalSnapshot(after)
	if err != nil {
		return StateDiff{}, fmt.Errorf("after snapshot: %w", err)
	}

	diff := StateDiff{SchemaChanged: canonicalBefore.SchemaDigest != canonicalAfter.SchemaDigest}
	tables := unionKeys(canonicalBefore.Tables, canonicalAfter.Tables)
	for _, table := range tables {
		beforeRows := canonicalBefore.Tables[table]
		afterRows := canonicalAfter.Tables[table]
		keys := unionKeys(beforeRows, afterRows)
		for _, key := range keys {
			beforeRow, beforeExists := beforeRows[key]
			afterRow, afterExists := afterRows[key]
			switch {
			case !beforeExists && afterExists:
				diff.Changes = append(diff.Changes, Change{
					Table: table, Key: key, Operation: OperationInsert,
					Columns: sortedRowColumns(afterRow), After: cloneRow(afterRow),
				})
			case beforeExists && !afterExists:
				diff.Changes = append(diff.Changes, Change{
					Table: table, Key: key, Operation: OperationDelete,
					Before: cloneRow(beforeRow),
				})
			case beforeExists && afterExists:
				columns := changedColumns(beforeRow, afterRow)
				if len(columns) > 0 {
					diff.Changes = append(diff.Changes, Change{
						Table: table, Key: key, Operation: OperationUpdate,
						Columns: columns, Before: cloneRow(beforeRow), After: cloneRow(afterRow),
					})
				}
			}
		}
	}
	return diff, nil
}

// EvaluateMutations enforces that every change uniquely matches one rule.
func EvaluateMutations(diff StateDiff, rules []MutationRule) (MutationReport, error) {
	canonicalRules := make([]MutationRule, len(rules))
	for index, rule := range rules {
		canonical, err := canonicalRule(rule)
		if err != nil {
			return MutationReport{}, fmt.Errorf("rule %d: %w", index, err)
		}
		canonicalRules[index] = canonical
	}

	report := MutationReport{Pass: true, RuleCounts: make(map[string]int, len(rules))}
	if diff.SchemaChanged {
		report.Violations = append(report.Violations, Violation{
			Code: "SCHEMA_CHANGED", Message: "database schema digest changed during Agent execution",
		})
	}

	for _, change := range diff.Changes {
		var matched []int
		for index, rule := range canonicalRules {
			ok, err := ruleMatches(rule, change)
			if err != nil {
				return MutationReport{}, fmt.Errorf("match rule %s: %w", rule.ID, err)
			}
			if ok {
				matched = append(matched, index)
			}
		}
		switch len(matched) {
		case 0:
			report.Violations = append(report.Violations, Violation{
				Code: "UNAPPROVED_MUTATION", Table: change.Table, Key: change.Key,
				Message: fmt.Sprintf("%s change does not match an allowed state-change rule", change.Operation),
			})
		case 1:
			report.RuleCounts[canonicalRules[matched[0]].ID]++
		default:
			report.Violations = append(report.Violations, Violation{
				Code: "AMBIGUOUS_MUTATION_RULE", Table: change.Table, Key: change.Key,
				Message: "change matches more than one allowed state-change rule",
			})
		}
	}

	for _, rule := range canonicalRules {
		count := report.RuleCounts[rule.ID]
		if count < rule.MinAffectedRows || count > rule.MaxAffectedRows {
			report.Violations = append(report.Violations, Violation{
				Code: "AFFECTED_ROWS_OUT_OF_RANGE", RuleID: rule.ID, Table: rule.Table,
				Message: fmt.Sprintf("matched rows %d outside [%d,%d]", count, rule.MinAffectedRows, rule.MaxAffectedRows),
			})
		}
	}
	report.Pass = len(report.Violations) == 0
	return report, nil
}

// RowDigest returns a stable SHA-256 over canonical column names, types, and values.
func RowDigest(row Row) (string, error) {
	canonical, err := canonicalRow(row)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	for _, column := range sortedRowColumns(canonical) {
		cell := canonical[column]
		writePart(hash, column)
		writePart(hash, string(cell.Type))
		writePart(hash, cell.Value)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func canonicalSnapshot(snapshot StateSnapshot) (StateSnapshot, error) {
	result := StateSnapshot{SchemaDigest: strings.ToLower(strings.TrimSpace(snapshot.SchemaDigest)), Tables: make(map[string]TableSnapshot, len(snapshot.Tables))}
	if len(result.SchemaDigest) != 64 {
		return StateSnapshot{}, fmt.Errorf("schema digest must be 64 hexadecimal characters")
	}
	if _, err := hex.DecodeString(result.SchemaDigest); err != nil {
		return StateSnapshot{}, fmt.Errorf("schema digest: %w", err)
	}
	for tableName, table := range snapshot.Tables {
		tableKey := strings.ToLower(strings.TrimSpace(tableName))
		if !identifierPattern.MatchString(tableKey) {
			return StateSnapshot{}, fmt.Errorf("invalid table name %q", tableName)
		}
		if _, exists := result.Tables[tableKey]; exists {
			return StateSnapshot{}, fmt.Errorf("duplicate normalized table %q", tableKey)
		}
		canonicalTable := make(TableSnapshot, len(table))
		for key, row := range table {
			if key == "" {
				return StateSnapshot{}, fmt.Errorf("table %s has empty primary-key encoding", tableKey)
			}
			canonical, err := canonicalRow(row)
			if err != nil {
				return StateSnapshot{}, fmt.Errorf("table %s row %s: %w", tableKey, key, err)
			}
			canonicalTable[key] = canonical
		}
		result.Tables[tableKey] = canonicalTable
	}
	return result, nil
}

func canonicalRow(row Row) (Row, error) {
	result := make(Row, len(row))
	for columnName, cell := range row {
		column := strings.ToLower(strings.TrimSpace(columnName))
		if !identifierPattern.MatchString(column) {
			return nil, fmt.Errorf("invalid column name %q", columnName)
		}
		if _, exists := result[column]; exists {
			return nil, fmt.Errorf("duplicate normalized column %q", column)
		}
		canonical, err := NewCell(cell.Type, cell.Value)
		if err != nil {
			return nil, fmt.Errorf("column %s: %w", column, err)
		}
		result[column] = canonical
	}
	return result, nil
}

func canonicalRule(rule MutationRule) (MutationRule, error) {
	rule.ID = strings.TrimSpace(rule.ID)
	rule.Table = strings.ToLower(strings.TrimSpace(rule.Table))
	rule.Predicate.Column = strings.ToLower(strings.TrimSpace(rule.Predicate.Column))
	rule.Predicate.Operator = strings.ToUpper(strings.TrimSpace(rule.Predicate.Operator))
	if rule.ID == "" {
		return MutationRule{}, fmt.Errorf("ID is required")
	}
	if !identifierPattern.MatchString(rule.Table) || !identifierPattern.MatchString(rule.Predicate.Column) {
		return MutationRule{}, fmt.Errorf("invalid table or predicate column")
	}
	if rule.Operation != OperationInsert && rule.Operation != OperationUpdate && rule.Operation != OperationDelete {
		return MutationRule{}, fmt.Errorf("unsupported operation %q", rule.Operation)
	}
	if rule.MinAffectedRows < 0 || rule.MaxAffectedRows < rule.MinAffectedRows {
		return MutationRule{}, fmt.Errorf("invalid affected-row range")
	}
	columns := make([]string, 0, len(rule.Columns))
	seen := make(map[string]struct{}, len(rule.Columns))
	for _, columnName := range rule.Columns {
		column := strings.ToLower(strings.TrimSpace(columnName))
		if !identifierPattern.MatchString(column) {
			return MutationRule{}, fmt.Errorf("invalid allowed column %q", columnName)
		}
		if _, exists := seen[column]; exists {
			return MutationRule{}, fmt.Errorf("duplicate allowed column %q", column)
		}
		seen[column] = struct{}{}
		columns = append(columns, column)
	}
	sort.Strings(columns)
	rule.Columns = columns
	if rule.Operation == OperationDelete && len(rule.Columns) != 0 {
		return MutationRule{}, fmt.Errorf("DELETE rule must have no columns")
	}
	if rule.Operation != OperationDelete && len(rule.Columns) == 0 {
		return MutationRule{}, fmt.Errorf("%s rule must allow at least one column", rule.Operation)
	}
	switch rule.Predicate.Operator {
	case "=", "!=", "<", "<=", ">", ">=", "IN", "IS_NULL", "IS_NOT_NULL":
	default:
		return MutationRule{}, fmt.Errorf("unsupported predicate operator %q", rule.Predicate.Operator)
	}
	return rule, nil
}

func ruleMatches(rule MutationRule, change Change) (bool, error) {
	if rule.Table != change.Table || rule.Operation != change.Operation {
		return false, nil
	}
	allowed := make(map[string]struct{}, len(rule.Columns))
	for _, column := range rule.Columns {
		allowed[column] = struct{}{}
	}
	for _, column := range change.Columns {
		if _, ok := allowed[column]; !ok {
			return false, nil
		}
	}
	row := change.After
	if change.Operation == OperationDelete {
		row = change.Before
	}
	cell, exists := row[rule.Predicate.Column]
	if !exists {
		return false, nil
	}
	return evaluatePredicate(cell, rule.Predicate)
}

func evaluatePredicate(cell Cell, predicate Predicate) (bool, error) {
	switch predicate.Operator {
	case "IS_NULL":
		return cell.Type == CellNull, nil
	case "IS_NOT_NULL":
		return cell.Type != CellNull, nil
	case "IN":
		values, ok := predicate.Value.([]any)
		if !ok {
			return false, fmt.Errorf("IN predicate value must be an array")
		}
		for _, value := range values {
			equal, err := compareCell(cell, value, "=")
			if err != nil {
				return false, err
			}
			if equal {
				return true, nil
			}
		}
		return false, nil
	default:
		return compareCell(cell, predicate.Value, predicate.Operator)
	}
}

func compareCell(cell Cell, expected any, operator string) (bool, error) {
	expectedValue, err := canonicalValue(cell.Type, expected)
	if err != nil {
		return false, err
	}
	comparison, err := compareCanonical(cell, expectedValue)
	if err != nil {
		return false, err
	}
	switch operator {
	case "=":
		return comparison == 0, nil
	case "!=":
		return comparison != 0, nil
	case "<":
		return comparison < 0, nil
	case "<=":
		return comparison <= 0, nil
	case ">":
		return comparison > 0, nil
	case ">=":
		return comparison >= 0, nil
	default:
		return false, fmt.Errorf("unsupported comparison operator %q", operator)
	}
}

func compareCanonical(actual Cell, expected string) (int, error) {
	switch actual.Type {
	case CellInteger, CellDecimal:
		left, ok := new(big.Rat).SetString(actual.Value)
		if !ok {
			return 0, fmt.Errorf("invalid actual number %q", actual.Value)
		}
		right, ok := new(big.Rat).SetString(expected)
		if !ok {
			return 0, fmt.Errorf("invalid expected number %q", expected)
		}
		return left.Cmp(right), nil
	case CellJSON:
		if actual.Value == expected {
			return 0, nil
		}
		return strings.Compare(actual.Value, expected), nil
	default:
		return strings.Compare(actual.Value, expected), nil
	}
}

func canonicalValue(cellType CellType, value any) (string, error) {
	if cellType == CellNull {
		if value != nil && value != "" {
			return "", fmt.Errorf("NULL cell must have no value")
		}
		return "", nil
	}
	if value == nil {
		return "", fmt.Errorf("%s cell cannot be nil", cellType)
	}

	text, textOK := value.(string)
	switch cellType {
	case CellString:
		if !textOK || !utf8.ValidString(text) {
			return "", fmt.Errorf("STRING value must be valid UTF-8")
		}
		return text, nil
	case CellInteger:
		text = scalarText(value)
		integer := new(big.Int)
		if _, ok := integer.SetString(text, 10); !ok || integer.String() != text {
			return "", fmt.Errorf("INTEGER value %q is not canonical", text)
		}
		return text, nil
	case CellDecimal:
		text = scalarText(value)
		if _, ok := new(big.Rat).SetString(text); !ok || strings.ContainsAny(text, "eE") {
			return "", fmt.Errorf("DECIMAL value %q is invalid", text)
		}
		return text, nil
	case CellBoolean:
		switch typed := value.(type) {
		case bool:
			if typed {
				return "true", nil
			}
			return "false", nil
		case string:
			if typed == "true" || typed == "false" {
				return typed, nil
			}
		}
		return "", fmt.Errorf("BOOLEAN value must be true or false")
	case CellDate:
		if !textOK {
			return "", fmt.Errorf("DATE value must be a string")
		}
		if _, err := time.Parse("2006-01-02", text); err != nil {
			return "", fmt.Errorf("DATE value: %w", err)
		}
		return text, nil
	case CellDateTime:
		if !textOK {
			return "", fmt.Errorf("DATETIME value must be a string")
		}
		parsed, err := time.Parse(time.RFC3339Nano, text)
		if err != nil || parsed.Location() != time.UTC {
			return "", fmt.Errorf("DATETIME value must be RFC3339 UTC")
		}
		return parsed.UTC().Format(time.RFC3339Nano), nil
	case CellBinary:
		if !textOK {
			return "", fmt.Errorf("BINARY value must be unpadded base64")
		}
		decoded, err := base64.RawStdEncoding.DecodeString(text)
		if err != nil || base64.RawStdEncoding.EncodeToString(decoded) != text {
			return "", fmt.Errorf("BINARY value must be canonical unpadded base64")
		}
		return text, nil
	case CellJSON:
		var document any
		if textOK {
			decoder := json.NewDecoder(strings.NewReader(text))
			decoder.UseNumber()
			if err := decoder.Decode(&document); err != nil {
				return "", fmt.Errorf("JSON value: %w", err)
			}
			var trailing any
			if err := decoder.Decode(&trailing); err != io.EOF {
				return "", fmt.Errorf("JSON value has trailing data")
			}
		} else {
			document = value
		}
		encoded, err := json.Marshal(document)
		if err != nil {
			return "", fmt.Errorf("JSON value: %w", err)
		}
		return string(encoded), nil
	default:
		return "", fmt.Errorf("unsupported cell type %q", cellType)
	}
}

func scalarText(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	if number, ok := value.(json.Number); ok {
		return number.String()
	}
	return fmt.Sprint(value)
}

func changedColumns(before, after Row) []string {
	columns := unionKeys(before, after)
	changed := columns[:0]
	for _, column := range columns {
		beforeCell, beforeExists := before[column]
		afterCell, afterExists := after[column]
		if beforeExists != afterExists || beforeCell != afterCell {
			changed = append(changed, column)
		}
	}
	return changed
}

func sortedRowColumns(row Row) []string {
	columns := make([]string, 0, len(row))
	for column := range row {
		columns = append(columns, column)
	}
	sort.Strings(columns)
	return columns
}

func cloneRow(row Row) Row {
	if row == nil {
		return nil
	}
	clone := make(Row, len(row))
	for column, cell := range row {
		clone[column] = cell
	}
	return clone
}

func unionKeys[V any](left, right map[string]V) []string {
	keys := make(map[string]struct{}, len(left)+len(right))
	for key := range left {
		keys[key] = struct{}{}
	}
	for key := range right {
		keys[key] = struct{}{}
	}
	result := make([]string, 0, len(keys))
	for key := range keys {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

type byteWriter interface {
	Write([]byte) (int, error)
}

func writePart(writer byteWriter, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = writer.Write(length[:])
	_, _ = writer.Write([]byte(value))
}
