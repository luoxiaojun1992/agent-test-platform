package snapshot

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	"agent-test-platform/internal/generation"
	"agent-test-platform/internal/validation"

	"github.com/go-sql-driver/mysql"
)

type failingResolver struct{}

func (failingResolver) Resolve(context.Context, string) (string, error) {
	return "", errors.New("no secret")
}

type staticResolver string

func (resolver staticResolver) Resolve(context.Context, string) (string, error) {
	return string(resolver), nil
}

// scriptLimits are the limits the Generation Pipeline validates Dataset SQL against.
var scriptLimits = validation.ScriptLimits{
	MaxStatementBytes: 4096, MaxScriptBytes: 262144,
	MaxTables: 20, MaxRowsPerStatement: 1000, MaxTotalRows: 10000,
}

func validProfile() generation.DatabaseProfileConnection {
	return generation.DatabaseProfileConnection{
		ID: "01K4Z6Y7X8W9V0T1S2R3Q4P5NC", Host: "127.0.0.1", Port: 3306, Database: "reference",
		Username: "atp_reader", PasswordSecretRef: "sales-reader-password", TLSMode: "DISABLED_LOCAL_ONLY",
		SnapshotMode: "SCHEMA_AND_DATA", SnapshotTimeoutSeconds: 60,
	}
}

func TestNewMySQLSourceRequiresResolver(t *testing.T) {
	t.Parallel()

	if _, err := NewMySQLSource(nil); err == nil {
		t.Fatal("nil resolver was accepted")
	}
}

func TestSnapshotRejectsInvalidProfilesBeforeConnecting(t *testing.T) {
	t.Parallel()

	source, err := NewMySQLSource(staticResolver("unused"))
	if err != nil {
		t.Fatal(err)
	}
	timeout := validProfile()
	timeout.SnapshotTimeoutSeconds = 0
	badMode := validProfile()
	badMode.SnapshotMode = "EVERYTHING"
	badTLS := validProfile()
	badTLS.TLSMode = "PLAIN"
	for name, profile := range map[string]generation.DatabaseProfileConnection{
		"timeout out of range": timeout, "unknown snapshot mode": badMode, "unknown tls_mode": badTLS,
	} {
		if _, err := source.Snapshot(context.Background(), profile); !errors.Is(err, generation.ErrDatasetSourceConfig) {
			t.Errorf("%s error = %v, want ErrDatasetSourceConfig", name, err)
		}
	}

	failing, err := NewMySQLSource(failingResolver{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failing.Snapshot(context.Background(), validProfile()); !errors.Is(err, generation.ErrDatasetSourceConfig) {
		t.Fatalf("unresolvable secret error = %v, want ErrDatasetSourceConfig", err)
	}
}

func TestSQLLiteralsEscapeEveryDelimiter(t *testing.T) {
	t.Parallel()

	got := sqlLiteral(sql.NullString{String: "it's a \\ path\nwith\r\x00and\x1a", Valid: true}, false)
	if want := `'it''s a \\ path\nwith\r\0and\Z'`; got != want {
		t.Fatalf("literal = %s, want %s", got, want)
	}
	if sqlLiteral(sql.NullString{}, false) != "NULL" {
		t.Fatal("NULL value was not rendered as NULL")
	}
	if sqlLiteral(sql.NullString{String: "00FF", Valid: true}, true) != "X'00FF'" {
		t.Fatal("binary value was not rendered as a hexadecimal literal")
	}
	if quoteIdentifier("odd`name") != "`odd``name`" {
		t.Fatalf("identifier = %s", quoteIdentifier("odd`name"))
	}
}

// testDSN returns the MySQL 8.4 administrative DSN for integration tests. The reference schemas
// are created on that instance and dropped afterwards.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("ATP_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set ATP_TEST_MYSQL_DSN to run the MySQL 8.4 snapshot integration tests")
	}
	return dsn
}

func openDatabase(t *testing.T, database string) (*sql.DB, *mysql.Config) {
	t.Helper()
	config, err := mysql.ParseDSN(testDSN(t))
	if err != nil {
		t.Fatalf("parse ATP_TEST_MYSQL_DSN: %v", err)
	}
	config.DBName = database
	config.ParseTime = true
	connector, err := mysql.NewConnector(config)
	if err != nil {
		t.Fatal(err)
	}
	opened := sql.OpenDB(connector)
	opened.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = opened.Close() })
	return opened, config
}

func randomName(t *testing.T, prefix string) string {
	t.Helper()
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		t.Fatal(err)
	}
	return prefix + hex.EncodeToString(buffer)
}

// createReferenceDatabase creates a uniquely named database, runs the setup statements in it, and keeps
// it for traceability: tests never drop validation MySQL objects. It returns the database name and the
// connection configuration.
func createReferenceDatabase(t *testing.T, prefix string, setup ...string) (string, *mysql.Config) {
	t.Helper()
	admin, config := openDatabase(t, "")
	name := randomName(t, prefix)
	if _, err := admin.ExecContext(context.Background(), "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	scoped, _ := openDatabase(t, name)
	for _, statement := range setup {
		if _, err := scoped.ExecContext(context.Background(), statement); err != nil {
			t.Fatalf("setup %q: %v", statement, err)
		}
	}
	return name, config
}

func profileFor(t *testing.T, config *mysql.Config, database, mode string) generation.DatabaseProfileConnection {
	t.Helper()
	host, portText, err := net.SplitHostPort(config.Addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	profile := validProfile()
	profile.Host, profile.Port, profile.Database, profile.Username = host, port, database, config.User
	profile.SnapshotMode = mode
	return profile
}

// fingerprint hashes a table's rows in primary key order, using the same representation the
// snapshot writes, so two databases with equal fingerprints hold identical rows.
func fingerprint(t *testing.T, database, table, keys string, columns []string, binary []bool) string {
	t.Helper()
	expressions := make([]string, len(columns))
	for index, column := range columns {
		if binary[index] {
			expressions[index] = "COALESCE(HEX(" + quoteIdentifier(column) + "), '\\\\N')"
		} else {
			expressions[index] = "COALESCE(CAST(" + quoteIdentifier(column) + " AS CHAR), '\\\\N')"
		}
	}
	scoped, _ := openDatabase(t, database)
	if _, err := scoped.ExecContext(context.Background(), "SET SESSION group_concat_max_len = 1048576"); err != nil {
		t.Fatal(err)
	}
	query := fmt.Sprintf("SELECT COUNT(*), SHA2(GROUP_CONCAT(CONCAT_WS(0x1f, %s) ORDER BY %s SEPARATOR 0x0a), 256) FROM %s",
		strings.Join(expressions, ", "), keys, quoteIdentifier(table))
	var count int
	var digest sql.NullString
	if err := scoped.QueryRowContext(context.Background(), query).Scan(&count, &digest); err != nil {
		t.Fatalf("fingerprint %s.%s: %v", database, table, err)
	}
	return fmt.Sprintf("%d:%s", count, digest.String)
}

// replayScript executes a validated script statement by statement, as the Dataset loader does.
func replayScript(t *testing.T, database, script string, context_ validation.SQLContext) {
	t.Helper()
	info, err := validation.ValidateSQLScript(script, context_, scriptLimits)
	if err != nil {
		t.Fatalf("snapshot %s SQL failed platform validation: %v", context_, err)
	}
	scoped, _ := openDatabase(t, database)
	for _, statement := range info.Statements {
		if _, err := scoped.ExecContext(context.Background(), statement); err != nil {
			t.Fatalf("replay %q: %v", statement, err)
		}
	}
}

func TestSnapshotRoundTripsReferenceSchemaAndRows(t *testing.T) {
	reference, config := createReferenceDatabase(t, "atp_ref_",
		`CREATE TABLE customers (id VARCHAR(32) NOT NULL PRIMARY KEY, name VARCHAR(128) NOT NULL, note TEXT NULL) ENGINE=InnoDB AUTO_INCREMENT=50 DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE orders (id VARCHAR(32) NOT NULL PRIMARY KEY, customer_id VARCHAR(32) NOT NULL, status VARCHAR(32) NOT NULL,
			amount DECIMAL(10,2) NOT NULL, placed_at DATETIME(6) NOT NULL, payload JSON NULL, KEY ix_status (status),
			CONSTRAINT fk_orders_customer FOREIGN KEY (customer_id) REFERENCES customers (id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE order_lines (order_id VARCHAR(32) NOT NULL, line_no INT NOT NULL, sku VARCHAR(64) NOT NULL, raw BLOB NULL,
			PRIMARY KEY (order_id, line_no), CONSTRAINT fk_lines_order FOREIGN KEY (order_id) REFERENCES orders (id)) ENGINE=InnoDB`,
		`INSERT INTO customers (id, name, note) VALUES ('C1', 'Ada "quoted" O''Brien', 'line1\nline2 \\ back\\slash'), ('C2', 'Bob', NULL)`,
		`INSERT INTO orders (id, customer_id, status, amount, placed_at, payload) VALUES
			('O1', 'C1', 'PAID', 12.50, '2026-09-11 09:00:00.123456', '{"a": 1, "note": "it''s"}'),
			('O2', 'C2', 'SHIPPED', 0.00, '2026-09-12 10:00:00.000000', NULL)`,
		`INSERT INTO order_lines (order_id, line_no, sku, raw) VALUES ('O1', 1, 'SKU-A', X'00FF10'), ('O1', 2, 'SKU-B', NULL)`,
	)
	source, err := NewMySQLSource(staticResolver(config.Passwd))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := source.Snapshot(context.Background(), profileFor(t, config, reference, "SCHEMA_AND_DATA"))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	customers := strings.Index(snapshot.SchemaSQL, "CREATE TABLE `customers`")
	orders := strings.Index(snapshot.SchemaSQL, "CREATE TABLE `orders`")
	lines := strings.Index(snapshot.SchemaSQL, "CREATE TABLE `order_lines`")
	if customers < 0 || !(customers < orders && orders < lines) {
		t.Fatalf("tables are not in dependency order:\n%s", snapshot.SchemaSQL)
	}
	for _, forbidden := range []string{"ENGINE=", "AUTO_INCREMENT=", "DEFAULT CHARSET="} {
		if strings.Contains(snapshot.SchemaSQL, forbidden) {
			t.Fatalf("schema keeps table option %q:\n%s", forbidden, snapshot.SchemaSQL)
		}
	}

	copyDatabase, _ := createReferenceDatabase(t, "atp_copy_")
	replayScript(t, copyDatabase, snapshot.SchemaSQL, validation.SQLSchema)
	replayScript(t, copyDatabase, snapshot.SeedSQL, validation.SQLSeed)
	for _, table := range []struct {
		name, keys string
		columns    []string
		binary     []bool
	}{
		{"customers", "`id`", []string{"id", "name", "note"}, []bool{false, false, false}},
		{"orders", "`id`", []string{"id", "customer_id", "status", "amount", "placed_at", "payload"}, []bool{false, false, false, false, false, false}},
		{"order_lines", "`order_id`, `line_no`", []string{"order_id", "line_no", "sku", "raw"}, []bool{false, false, false, true}},
	} {
		want := fingerprint(t, reference, table.name, table.keys, table.columns, table.binary)
		got := fingerprint(t, copyDatabase, table.name, table.keys, table.columns, table.binary)
		if want != got {
			t.Fatalf("table %s fingerprint = %s, want %s", table.name, got, want)
		}
	}
}

func TestSnapshotSchemaOnlyCarriesNoRows(t *testing.T) {
	reference, config := createReferenceDatabase(t, "atp_schema_",
		`CREATE TABLE items (id INT NOT NULL PRIMARY KEY, label VARCHAR(32) NOT NULL)`,
		`INSERT INTO items (id, label) VALUES (1, 'secret-row-value')`,
	)
	source, err := NewMySQLSource(staticResolver(config.Passwd))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := source.Snapshot(context.Background(), profileFor(t, config, reference, "SCHEMA_ONLY"))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot.SeedSQL != "" || strings.Contains(snapshot.SchemaSQL, "secret-row-value") {
		t.Fatalf("SCHEMA_ONLY snapshot carries rows: seed=%q", snapshot.SeedSQL)
	}
}

func TestSnapshotRefusesUnsupportedReferenceSchemas(t *testing.T) {
	tests := map[string][]string{
		"generated column":          {`CREATE TABLE t (id INT NOT NULL PRIMARY KEY, doubled INT AS (id * 2) VIRTUAL)`},
		"table without primary key": {`CREATE TABLE t (a INT NOT NULL)`},
	}
	for name, setup := range tests {
		reference, config := createReferenceDatabase(t, "atp_unsupported_", setup...)
		source, err := NewMySQLSource(staticResolver(config.Passwd))
		if err != nil {
			t.Fatal(err)
		}
		_, err = source.Snapshot(context.Background(), profileFor(t, config, reference, "SCHEMA_AND_DATA"))
		if !errors.Is(err, generation.ErrDatasetSourceUnsupported) {
			t.Errorf("%s: error = %v, want ErrDatasetSourceUnsupported", name, err)
		}
	}
}

func TestSnapshotRefusesOversizedReferenceData(t *testing.T) {
	reference, config := createReferenceDatabase(t, "atp_bulk_",
		`CREATE TABLE bulk (id INT NOT NULL PRIMARY KEY)`,
	)
	scoped, _ := openDatabase(t, reference)
	if _, err := scoped.ExecContext(context.Background(), "SET SESSION cte_max_recursion_depth = 20000"); err != nil {
		t.Fatal(err)
	}
	if _, err := scoped.ExecContext(context.Background(),
		"INSERT INTO bulk (id) WITH RECURSIVE seq (n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < 10001) SELECT n FROM seq"); err != nil {
		t.Fatalf("seed bulk rows: %v", err)
	}
	source, err := NewMySQLSource(staticResolver(config.Passwd))
	if err != nil {
		t.Fatal(err)
	}
	_, err = source.Snapshot(context.Background(), profileFor(t, config, reference, "SCHEMA_AND_DATA"))
	if !errors.Is(err, generation.ErrDatasetSourceLimit) {
		t.Fatalf("error = %v, want ErrDatasetSourceLimit", err)
	}
}
