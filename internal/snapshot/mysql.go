// Package snapshot reads a Database Profile's reference schema from validation MySQL and turns it into
// Dataset SQL (docs/technical/06-sandbox-and-security.md section 1). It is the only package that
// speaks to a reference database; the Generation Pipeline sees only generation.DatasetSnapshot.
package snapshot

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"agent-test-platform/internal/generation"
	"agent-test-platform/internal/sandbox"

	"github.com/go-sql-driver/mysql"
)

// SecretResolver turns a Secret Reference into its value (satisfied by secrets.Service).
type SecretResolver interface {
	Resolve(ctx context.Context, reference string) (string, error)
}

// MySQLSource reads one Database Profile's reference schema inside a single read-only, repeatable
// read transaction, so every statement sees the same data.
type MySQLSource struct {
	resolver SecretResolver
}

// NewMySQLSource builds the source. Each profile carries its own optional CA certificate, so the source
// holds no trust configuration of its own.
func NewMySQLSource(resolver SecretResolver) (*MySQLSource, error) {
	if resolver == nil {
		return nil, fmt.Errorf("secret resolver is required")
	}
	return &MySQLSource{resolver: resolver}, nil
}

// statementTimeoutMilliseconds bounds every SELECT, as docs/technical/01-storage-and-workflows.md
// section 3 requires for individual metadata and query statements.
const statementTimeoutMilliseconds = 5000

// Snapshot returns the reference schema (and, for SCHEMA_AND_DATA, its rows) as Dataset SQL. The
// whole read runs under snapshot_timeout_seconds; a timeout surfaces as context.DeadlineExceeded.
func (source *MySQLSource) Snapshot(ctx context.Context, profile generation.DatabaseProfileConnection) (generation.DatasetSnapshot, error) {
	if profile.SnapshotTimeoutSeconds < 1 || profile.SnapshotTimeoutSeconds > 300 {
		return generation.DatasetSnapshot{}, fmt.Errorf("%w: snapshot timeout must be 1-300 seconds", generation.ErrDatasetSourceConfig)
	}
	if profile.SnapshotMode != "SCHEMA_ONLY" && profile.SnapshotMode != "SCHEMA_AND_DATA" {
		return generation.DatasetSnapshot{}, fmt.Errorf("%w: unsupported snapshot_mode %q", generation.ErrDatasetSourceConfig, profile.SnapshotMode)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(profile.SnapshotTimeoutSeconds)*time.Second)
	defer cancel()

	database, err := source.open(ctx, profile)
	if err != nil {
		return generation.DatasetSnapshot{}, err
	}
	defer database.Close()

	if _, err := database.ExecContext(ctx, fmt.Sprintf(
		"SET SESSION time_zone = '+00:00', SESSION max_execution_time = %d", statementTimeoutMilliseconds,
	)); err != nil {
		return generation.DatasetSnapshot{}, unavailable(ctx, err)
	}
	transaction, err := database.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return generation.DatasetSnapshot{}, unavailable(ctx, err)
	}
	defer transaction.Rollback()

	return readSnapshot(ctx, transaction, profile.SnapshotMode)
}

func (source *MySQLSource) open(ctx context.Context, profile generation.DatabaseProfileConnection) (*sql.DB, error) {
	password, err := source.resolver.Resolve(ctx, profile.PasswordSecretRef)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve Database Profile password reference: %v", generation.ErrDatasetSourceConfig, err)
	}
	config := mysql.NewConfig()
	config.User = profile.Username
	config.Passwd = password
	config.Net = "tcp"
	config.Addr = net.JoinHostPort(profile.Host, strconv.Itoa(profile.Port))
	config.DBName = profile.Database
	config.Loc = time.UTC
	config.Timeout = 10 * time.Second
	if err := sandbox.ApplyTLS(config, profile.TLSMode, profile.Host, profile.CACertificatePEM); err != nil {
		return nil, fmt.Errorf("%w: %v", generation.ErrDatasetSourceConfig, err)
	}
	connector, err := mysql.NewConnector(config)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", generation.ErrDatasetSourceConfig, err)
	}
	database := sql.OpenDB(connector)
	// One connection carries the whole snapshot, so the session settings above apply to it.
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	return database, nil
}

func readSnapshot(ctx context.Context, transaction *sql.Tx, mode string) (generation.DatasetSnapshot, error) {
	tables, err := baseTables(ctx, transaction)
	if err != nil {
		return generation.DatasetSnapshot{}, err
	}
	if len(tables) == 0 {
		return generation.DatasetSnapshot{}, unsupportedf("the reference database has no base tables")
	}
	if len(tables) > generation.DatasetMaxTables {
		return generation.DatasetSnapshot{}, limitf("%d tables exceed the %d-table limit", len(tables), generation.DatasetMaxTables)
	}
	if err := rejectUnsupportedObjects(ctx, transaction); err != nil {
		return generation.DatasetSnapshot{}, err
	}
	order, err := dependencyOrder(ctx, transaction, tables)
	if err != nil {
		return generation.DatasetSnapshot{}, err
	}

	createStatements := make([]string, 0, len(order))
	for _, table := range order {
		statement, err := createTableStatement(ctx, transaction, table)
		if err != nil {
			return generation.DatasetSnapshot{}, err
		}
		createStatements = append(createStatements, statement)
	}
	snapshot := generation.DatasetSnapshot{SchemaSQL: strings.Join(createStatements, ";\n") + ";"}
	if mode == "SCHEMA_AND_DATA" {
		seed, err := seedScript(ctx, transaction, order)
		if err != nil {
			return generation.DatasetSnapshot{}, err
		}
		snapshot.SeedSQL = seed
	}
	return snapshot, nil
}

func baseTables(ctx context.Context, transaction *sql.Tx) ([]string, error) {
	rows, err := transaction.QueryContext(ctx, `SELECT table_name FROM information_schema.tables
WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE' ORDER BY table_name`)
	if err != nil {
		return nil, unavailable(ctx, err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, unavailable(ctx, err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(ctx, err)
	}
	return names, nil
}

// unsupportedObjectChecks lists the reference-schema features docs/technical/03-generation-and-
// validation.md keeps out of the platform's schema.sql subset. Each query counts occurrences; any
// occurrence fails the snapshot instead of silently rewriting the reference schema.
var unsupportedObjectChecks = []struct {
	description string
	query       string
}{
	{"partitioned tables", `SELECT COUNT(*) FROM information_schema.partitions
WHERE table_schema = DATABASE() AND partition_name IS NOT NULL`},
	{"generated columns", `SELECT COUNT(*) FROM information_schema.columns
WHERE table_schema = DATABASE() AND extra LIKE '%GENERATED%'`},
	{"CHECK constraints", `SELECT COUNT(*) FROM information_schema.check_constraints
WHERE constraint_schema = DATABASE()`},
	{"full-text or spatial indexes", `SELECT COUNT(*) FROM information_schema.statistics
WHERE table_schema = DATABASE() AND index_type IN ('FULLTEXT', 'SPATIAL')`},
	{"spatial columns", `SELECT COUNT(*) FROM information_schema.columns
WHERE table_schema = DATABASE()
  AND LOWER(data_type) IN ('geometry', 'point', 'linestring', 'polygon', 'multipoint', 'multilinestring', 'multipolygon', 'geometrycollection')`},
	{"tables without a primary key", `SELECT COUNT(*) FROM information_schema.tables t
WHERE t.table_schema = DATABASE() AND t.table_type = 'BASE TABLE'
  AND NOT EXISTS (SELECT 1 FROM information_schema.table_constraints c
    WHERE c.table_schema = t.table_schema AND c.table_name = t.table_name AND c.constraint_type = 'PRIMARY KEY')`},
	{"cross-database foreign keys", `SELECT COUNT(*) FROM information_schema.key_column_usage
WHERE table_schema = DATABASE() AND referenced_table_schema IS NOT NULL AND referenced_table_schema <> DATABASE()`},
	{"self-referencing foreign keys", `SELECT COUNT(*) FROM information_schema.key_column_usage
WHERE table_schema = DATABASE() AND referenced_table_schema = DATABASE() AND table_name = referenced_table_name`},
}

func rejectUnsupportedObjects(ctx context.Context, transaction *sql.Tx) error {
	for _, check := range unsupportedObjectChecks {
		var count int
		if err := transaction.QueryRowContext(ctx, check.query).Scan(&count); err != nil {
			return unavailable(ctx, err)
		}
		if count > 0 {
			return unsupportedf("the reference schema has %s", check.description)
		}
	}
	return nil
}

// dependencyOrder returns the tables with every referenced table before the tables that reference
// it, so CREATE TABLE and seed INSERT statements can run in order. Ties keep name order.
func dependencyOrder(ctx context.Context, transaction *sql.Tx, tables []string) ([]string, error) {
	rows, err := transaction.QueryContext(ctx, `SELECT table_name, referenced_table_name FROM information_schema.key_column_usage
WHERE table_schema = DATABASE() AND referenced_table_name IS NOT NULL`)
	if err != nil {
		return nil, unavailable(ctx, err)
	}
	defer rows.Close()
	parents := make(map[string]map[string]bool, len(tables))
	for _, table := range tables {
		parents[table] = map[string]bool{}
	}
	for rows.Next() {
		var child, parent string
		if err := rows.Scan(&child, &parent); err != nil {
			return nil, unavailable(ctx, err)
		}
		if _, ok := parents[child]; ok {
			parents[child][parent] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(ctx, err)
	}

	order := make([]string, 0, len(tables))
	placed := make(map[string]bool, len(tables))
	for len(order) < len(tables) {
		progressed := false
		for _, table := range tables {
			if placed[table] {
				continue
			}
			ready := true
			for parent := range parents[table] {
				if !placed[parent] {
					ready = false
					break
				}
			}
			if ready {
				order = append(order, table)
				placed[table] = true
				progressed = true
			}
		}
		if !progressed {
			return nil, unsupportedf("the reference schema's foreign keys form a cycle")
		}
	}
	return order, nil
}

// createTableStatement renders one table from SHOW CREATE TABLE, dropping the table options
// (engine, auto-increment counter, default character set, comments) that schema.sql does not carry.
func createTableStatement(ctx context.Context, transaction *sql.Tx, table string) (string, error) {
	var name, ddl string
	if err := transaction.QueryRowContext(ctx, "SHOW CREATE TABLE "+quoteIdentifier(table)).Scan(&name, &ddl); err != nil {
		return "", unavailable(ctx, err)
	}
	closing := strings.LastIndex(ddl, "\n)")
	if closing < 0 {
		return "", unsupportedf("table %s has an unrecognized definition", table)
	}
	definition := ddl[:closing+2]
	if strings.Contains(definition, "/*!") {
		return "", unsupportedf("table %s uses versioned MySQL comments", table)
	}
	return definition, nil
}

// seedScript produces INSERT statements for every table's rows in dependency order. Rows are
// counted before any data is read, so an oversized reference is refused without copying it.
func seedScript(ctx context.Context, transaction *sql.Tx, order []string) (string, error) {
	counts := make(map[string]int, len(order))
	total := 0
	for _, table := range order {
		var count int
		if err := transaction.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteIdentifier(table)).Scan(&count); err != nil {
			return "", unavailable(ctx, err)
		}
		counts[table] = count
		total += count
	}
	if total > generation.DatasetMaxTotalRows {
		return "", limitf("%d rows exceed the %d-row limit", total, generation.DatasetMaxTotalRows)
	}

	var statements []string
	for _, table := range order {
		if counts[table] == 0 {
			continue
		}
		tableStatements, err := tableInserts(ctx, transaction, table)
		if err != nil {
			return "", err
		}
		statements = append(statements, tableStatements...)
	}
	if len(statements) == 0 {
		return "", nil
	}
	return strings.Join(statements, ";\n") + ";", nil
}

// tableInserts reads one table ordered by its primary key and renders its rows as literal INSERT
// statements of at most DatasetMaxRowsPerStatement rows each.
func tableInserts(ctx context.Context, transaction *sql.Tx, table string) ([]string, error) {
	columns, dataTypes, err := tableColumns(ctx, transaction, table)
	if err != nil {
		return nil, err
	}
	keys, err := primaryKeyColumns(ctx, transaction, table)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, unsupportedf("table %s has no primary key", table)
	}

	selectList := make([]string, len(columns))
	insertList := make([]string, len(columns))
	binary := make([]bool, len(columns))
	for index, column := range columns {
		insertList[index] = quoteIdentifier(column)
		binary[index] = binaryDataTypes[strings.ToLower(dataTypes[index])]
		if binary[index] {
			selectList[index] = "HEX(" + quoteIdentifier(column) + ")"
		} else {
			selectList[index] = "CAST(" + quoteIdentifier(column) + " AS CHAR)"
		}
	}
	quotedKeys := make([]string, len(keys))
	for index, key := range keys {
		quotedKeys[index] = quoteIdentifier(key)
	}
	query := "SELECT " + strings.Join(selectList, ", ") + " FROM " + quoteIdentifier(table) +
		" ORDER BY " + strings.Join(quotedKeys, ", ")
	rows, err := transaction.QueryContext(ctx, query)
	if err != nil {
		return nil, unavailable(ctx, err)
	}
	defer rows.Close()

	var tuples []string
	for rows.Next() {
		values := make([]sql.NullString, len(columns))
		destinations := make([]any, len(columns))
		for index := range values {
			destinations[index] = &values[index]
		}
		if err := rows.Scan(destinations...); err != nil {
			return nil, unavailable(ctx, err)
		}
		literals := make([]string, len(values))
		for index, value := range values {
			literals[index] = sqlLiteral(value, binary[index])
		}
		tuples = append(tuples, "("+strings.Join(literals, ", ")+")")
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(ctx, err)
	}

	prefix := "INSERT INTO " + quoteIdentifier(table) + " (" + strings.Join(insertList, ", ") + ") VALUES "
	var statements []string
	for start := 0; start < len(tuples); start += generation.DatasetMaxRowsPerStatement {
		end := min(start+generation.DatasetMaxRowsPerStatement, len(tuples))
		statements = append(statements, prefix+strings.Join(tuples[start:end], ", "))
	}
	return statements, nil
}

func tableColumns(ctx context.Context, transaction *sql.Tx, table string) ([]string, []string, error) {
	rows, err := transaction.QueryContext(ctx, `SELECT column_name, data_type FROM information_schema.columns
WHERE table_schema = DATABASE() AND table_name = ? ORDER BY ordinal_position`, table)
	if err != nil {
		return nil, nil, unavailable(ctx, err)
	}
	defer rows.Close()
	var names, types []string
	for rows.Next() {
		var name, dataType string
		if err := rows.Scan(&name, &dataType); err != nil {
			return nil, nil, unavailable(ctx, err)
		}
		names = append(names, name)
		types = append(types, dataType)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, unavailable(ctx, err)
	}
	return names, types, nil
}

func primaryKeyColumns(ctx context.Context, transaction *sql.Tx, table string) ([]string, error) {
	rows, err := transaction.QueryContext(ctx, `SELECT column_name FROM information_schema.key_column_usage
WHERE table_schema = DATABASE() AND table_name = ? AND constraint_name = 'PRIMARY' ORDER BY ordinal_position`, table)
	if err != nil {
		return nil, unavailable(ctx, err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, unavailable(ctx, err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(ctx, err)
	}
	return names, nil
}

// binaryDataTypes are read as hexadecimal so that no byte sequence is reinterpreted as text.
var binaryDataTypes = map[string]bool{
	"binary": true, "varbinary": true, "tinyblob": true, "blob": true,
	"mediumblob": true, "longblob": true, "bit": true,
}

// sqlStringEscaper makes a value safe inside a single-quoted MySQL string literal under the
// default sql_mode, where backslash is an escape character.
var sqlStringEscaper = strings.NewReplacer(
	`\`, `\\`, `'`, `''`, "\x00", `\0`, "\n", `\n`, "\r", `\r`, "\x1a", `\Z`,
)

func sqlLiteral(value sql.NullString, hexadecimal bool) string {
	switch {
	case !value.Valid:
		return "NULL"
	case hexadecimal:
		return "X'" + value.String + "'"
	default:
		return "'" + sqlStringEscaper.Replace(value.String) + "'"
	}
}

func quoteIdentifier(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// unavailable keeps a context error as itself, so the caller can classify a timeout, and reports
// any other driver failure as an unavailable reference schema.
func unavailable(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("%w: %v", generation.ErrDatasetSourceUnavailable, err)
}

func unsupportedf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", generation.ErrDatasetSourceUnsupported, fmt.Sprintf(format, args...))
}

func limitf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", generation.ErrDatasetSourceLimit, fmt.Sprintf(format, args...))
}
