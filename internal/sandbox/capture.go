package sandbox

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"vitess.io/vitess/go/vt/sqlparser"
)

const maxSnapshotRows = 10_000

type ColumnSpec struct {
	Name string
	Type CellType
}

type TableSpec struct {
	Name       string
	Columns    []ColumnSpec
	PrimaryKey []string
}

// CaptureState reads the current schema digest and a deterministic data snapshot.
func CaptureState(ctx context.Context, database *sql.DB, tables []TableSpec) (StateSnapshot, error) {
	schemaDigest, err := ComputeSchemaDigest(ctx, database)
	if err != nil {
		return StateSnapshot{}, err
	}
	return captureStateWithDigest(ctx, database, schemaDigest, tables)
}

func captureStateWithDigest(ctx context.Context, database *sql.DB, schemaDigest string, tables []TableSpec) (StateSnapshot, error) {
	if len(tables) == 0 || len(tables) > 20 {
		return StateSnapshot{}, fmt.Errorf("table count %d outside [1,20]", len(tables))
	}
	snapshot := StateSnapshot{SchemaDigest: schemaDigest, Tables: make(map[string]TableSnapshot, len(tables))}
	totalRows := 0
	for _, table := range tables {
		canonical, err := canonicalTableSpec(table)
		if err != nil {
			return StateSnapshot{}, err
		}
		if _, exists := snapshot.Tables[canonical.Name]; exists {
			return StateSnapshot{}, fmt.Errorf("duplicate table %q", canonical.Name)
		}
		query, err := BuildSnapshotQuery(canonical)
		if err != nil {
			return StateSnapshot{}, err
		}
		rows, err := database.QueryContext(ctx, query)
		if err != nil {
			return StateSnapshot{}, fmt.Errorf("capture table %s: %w", canonical.Name, err)
		}
		tableSnapshot, count, err := scanTable(rows, canonical, maxSnapshotRows-totalRows)
		closeErr := rows.Close()
		if err != nil {
			return StateSnapshot{}, fmt.Errorf("capture table %s: %w", canonical.Name, err)
		}
		if closeErr != nil {
			return StateSnapshot{}, fmt.Errorf("close table %s rows: %w", canonical.Name, closeErr)
		}
		totalRows += count
		snapshot.Tables[canonical.Name] = tableSnapshot
	}
	return canonicalSnapshot(snapshot)
}

// ComputeSchemaDigest hashes ordered SHOW CREATE TABLE output for all base tables.
func ComputeSchemaDigest(ctx context.Context, database *sql.DB) (string, error) {
	rows, err := database.QueryContext(ctx, `
		SELECT table_name
		FROM information_schema.tables
		WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE'
		ORDER BY table_name
	`)
	if err != nil {
		return "", fmt.Errorf("list schema tables: %w", err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			rows.Close()
			return "", fmt.Errorf("scan schema table: %w", err)
		}
		table = strings.ToLower(strings.TrimSpace(table))
		if !identifierPattern.MatchString(table) {
			rows.Close()
			return "", fmt.Errorf("database contains unsupported table name %q", table)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", fmt.Errorf("iterate schema tables: %w", err)
	}
	if err := rows.Close(); err != nil {
		return "", fmt.Errorf("close schema tables: %w", err)
	}
	if len(tables) == 0 {
		return "", fmt.Errorf("database contains no base tables")
	}

	hash := sha256.New()
	for _, table := range tables {
		quotedTable, _ := QuoteIdentifier(table)
		var returnedTable, createSQL string
		if err := database.QueryRowContext(ctx, "SHOW CREATE TABLE "+quotedTable).Scan(&returnedTable, &createSQL); err != nil {
			return "", fmt.Errorf("show create table %s: %w", table, err)
		}
		createSQL, err = normalizeCreateTable(createSQL)
		if err != nil {
			return "", fmt.Errorf("normalize create table %s: %w", table, err)
		}
		writeDigestPart(hash, table)
		writeDigestPart(hash, createSQL)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeDigestPart(hash interface{ Write([]byte) (int, error) }, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = hash.Write(length[:])
	_, _ = hash.Write([]byte(value))
}

// BuildSnapshotQuery creates an explicit-column, primary-key-ordered SELECT.
func BuildSnapshotQuery(table TableSpec) (string, error) {
	canonical, err := canonicalTableSpec(table)
	if err != nil {
		return "", err
	}
	quotedTable, _ := QuoteIdentifier(canonical.Name)
	columns := make([]string, len(canonical.Columns))
	for index, column := range canonical.Columns {
		columns[index], _ = QuoteIdentifier(column.Name)
	}
	primaryKey := make([]string, len(canonical.PrimaryKey))
	for index, column := range canonical.PrimaryKey {
		primaryKey[index], _ = QuoteIdentifier(column)
	}
	return "SELECT " + strings.Join(columns, ", ") + " FROM " + quotedTable + " ORDER BY " + strings.Join(primaryKey, ", "), nil
}

func canonicalTableSpec(table TableSpec) (TableSpec, error) {
	table.Name = strings.ToLower(strings.TrimSpace(table.Name))
	if !identifierPattern.MatchString(table.Name) {
		return TableSpec{}, fmt.Errorf("invalid table name %q", table.Name)
	}
	if len(table.Columns) == 0 || len(table.Columns) > 128 {
		return TableSpec{}, fmt.Errorf("table %s column count outside [1,128]", table.Name)
	}
	columns := make(map[string]CellType, len(table.Columns))
	for index := range table.Columns {
		column := &table.Columns[index]
		column.Name = strings.ToLower(strings.TrimSpace(column.Name))
		if !identifierPattern.MatchString(column.Name) || !knownCellType(column.Type) || column.Type == CellNull {
			return TableSpec{}, fmt.Errorf("table %s has invalid column %q", table.Name, column.Name)
		}
		if _, exists := columns[column.Name]; exists {
			return TableSpec{}, fmt.Errorf("table %s duplicates column %s", table.Name, column.Name)
		}
		columns[column.Name] = column.Type
	}
	if len(table.PrimaryKey) == 0 {
		return TableSpec{}, fmt.Errorf("table %s requires a primary key", table.Name)
	}
	seenKeys := make(map[string]struct{}, len(table.PrimaryKey))
	for index, keyName := range table.PrimaryKey {
		key := strings.ToLower(strings.TrimSpace(keyName))
		if _, exists := columns[key]; !exists {
			return TableSpec{}, fmt.Errorf("table %s primary key column %s is missing", table.Name, key)
		}
		if _, exists := seenKeys[key]; exists {
			return TableSpec{}, fmt.Errorf("table %s duplicates primary key column %s", table.Name, key)
		}
		seenKeys[key] = struct{}{}
		table.PrimaryKey[index] = key
	}
	return table, nil
}

func scanTable(rows *sql.Rows, table TableSpec, remainingRows int) (TableSnapshot, int, error) {
	if remainingRows < 0 {
		return nil, 0, fmt.Errorf("snapshot row limit exceeded")
	}
	result := make(TableSnapshot)
	count := 0
	for rows.Next() {
		if count >= remainingRows {
			return nil, count, fmt.Errorf("snapshot row limit %d exceeded", maxSnapshotRows)
		}
		raw := make([]sql.RawBytes, len(table.Columns))
		targets := make([]any, len(raw))
		for index := range raw {
			targets[index] = &raw[index]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, count, err
		}
		row := make(Row, len(table.Columns))
		for index, column := range table.Columns {
			cell, err := cellFromRaw(column.Type, raw[index])
			if err != nil {
				return nil, count, fmt.Errorf("column %s: %w", column.Name, err)
			}
			row[column.Name] = cell
		}
		keyRow := make(Row, len(table.PrimaryKey))
		for _, keyColumn := range table.PrimaryKey {
			cell := row[keyColumn]
			if cell.Type == CellNull {
				return nil, count, fmt.Errorf("primary key column %s is NULL", keyColumn)
			}
			keyRow[keyColumn] = cell
		}
		key, err := RowDigest(keyRow)
		if err != nil {
			return nil, count, err
		}
		if _, exists := result[key]; exists {
			return nil, count, fmt.Errorf("duplicate primary key digest %s", key)
		}
		result[key] = row
		count++
	}
	if err := rows.Err(); err != nil {
		return nil, count, err
	}
	return result, count, nil
}

func cellFromRaw(cellType CellType, raw sql.RawBytes) (Cell, error) {
	if raw == nil {
		return NewCell(CellNull, nil)
	}
	value := string(raw)
	switch cellType {
	case CellBinary:
		return NewCell(CellBinary, base64.RawStdEncoding.EncodeToString(raw))
	case CellBoolean:
		switch strings.ToLower(value) {
		case "1", "true":
			return NewCell(CellBoolean, true)
		case "0", "false":
			return NewCell(CellBoolean, false)
		default:
			return Cell{}, fmt.Errorf("invalid BOOLEAN value %q", value)
		}
	case CellDateTime:
		parsed, err := parseMySQLDateTime(value)
		if err != nil {
			return Cell{}, err
		}
		return NewCell(CellDateTime, parsed.UTC().Format(time.RFC3339Nano))
	}
	return NewCell(cellType, value)
}

func normalizeCreateTable(createSQL string) (string, error) {
	parser := sqlparser.NewTestParser()
	statement, err := parser.Parse(createSQL)
	if err != nil {
		return "", err
	}
	createTable, ok := statement.(*sqlparser.CreateTable)
	if !ok || !createTable.FullyParsed || createTable.TableSpec == nil {
		return "", fmt.Errorf("SHOW CREATE TABLE did not produce a fully parsed CREATE TABLE")
	}
	options := createTable.TableSpec.Options[:0]
	for _, option := range createTable.TableSpec.Options {
		if strings.EqualFold(strings.TrimSpace(option.Name), "auto_increment") {
			continue
		}
		options = append(options, option)
	}
	createTable.TableSpec.Options = options
	return sqlparser.String(createTable), nil
}

func parseMySQLDateTime(value string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02 15:04:05.999999", "2006-01-02 15:04:05", time.RFC3339Nano} {
		parsed, err := time.ParseInLocation(layout, value, time.UTC)
		if err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid UTC DATETIME value %q", value)
}

func knownCellType(cellType CellType) bool {
	switch cellType {
	case CellNull, CellString, CellInteger, CellDecimal, CellBoolean,
		CellDate, CellDateTime, CellBinary, CellJSON:
		return true
	default:
		return false
	}
}
