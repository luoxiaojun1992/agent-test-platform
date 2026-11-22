package validation

import (
	"reflect"
	"testing"
)

func defaultScriptLimits() ScriptLimits {
	return ScriptLimits{MaxStatementBytes: 4096, MaxScriptBytes: 65536, MaxTables: 20, MaxRowsPerStatement: 1000, MaxTotalRows: 10000}
}

func TestValidateSQLScriptSchema(t *testing.T) {
	t.Parallel()

	script := `
CREATE TABLE customers (
	id VARCHAR(32) PRIMARY KEY,
	name VARCHAR(128) NOT NULL
);
CREATE TABLE orders (
	id VARCHAR(32) PRIMARY KEY,
	customer_id VARCHAR(32) NOT NULL,
	status VARCHAR(32) NOT NULL,
	updated_at DATETIME(6) NULL,
	CONSTRAINT fk_orders_customer FOREIGN KEY (customer_id) REFERENCES customers (id)
);
CREATE INDEX idx_orders_status ON orders (status);
`
	info, err := ValidateSQLScript(script, SQLSchema, defaultScriptLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Tables) != 2 {
		t.Fatalf("tables = %#v", info.Tables)
	}
	orders := info.Tables[1]
	if orders.Name != "orders" || !reflect.DeepEqual(orders.PrimaryKey, []string{"id"}) {
		t.Fatalf("orders = %#v", orders)
	}
	if len(orders.ForeignKeys) != 1 || orders.ForeignKeys[0].ReferencedTable != "customers" ||
		!reflect.DeepEqual(orders.ForeignKeys[0].Columns, []string{"customer_id"}) {
		t.Fatalf("foreign keys = %#v", orders.ForeignKeys)
	}
	for _, column := range orders.Columns {
		switch column.Name {
		case "id":
			if column.Nullable {
				t.Fatalf("primary key column reported nullable: %#v", column)
			}
		case "updated_at":
			if !column.Nullable {
				t.Fatalf("explicitly nullable column reported not-null: %#v", column)
			}
		case "status":
			if column.Nullable {
				t.Fatalf("NOT NULL column reported nullable: %#v", column)
			}
		}
	}
}

func TestValidateSQLScriptSeed(t *testing.T) {
	t.Parallel()

	script := `
INSERT INTO customers (id, name) VALUES ('C1', 'Ada');
INSERT INTO orders (id, customer_id, status) VALUES ('O1', 'C1', 'PAID'), ('O2', 'C1', 'SHIPPED');
`
	info, err := ValidateSQLScript(script, SQLSeed, defaultScriptLimits())
	if err != nil {
		t.Fatal(err)
	}
	if info.TotalRows != 3 || info.RowsByTable["customers"] != 1 || info.RowsByTable["orders"] != 2 {
		t.Fatalf("info = %#v", info)
	}
	empty, err := ValidateSQLScript("  \n", SQLSeed, defaultScriptLimits())
	if err != nil || empty.TotalRows != 0 {
		t.Fatalf("empty seed info = %#v error = %v, want an accepted script without rows", empty, err)
	}
	if _, err := ValidateSQLScript("  \n", SQLSchema, defaultScriptLimits()); err == nil {
		t.Fatal("empty schema script was accepted")
	}
}

func TestValidateSQLScriptRejectsSchemaViolations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		script string
	}{
		{"no primary key", "CREATE TABLE items (id INT)"},
		{"temporary table", "CREATE TEMPORARY TABLE items (id INT PRIMARY KEY)"},
		{"cross database", "CREATE TABLE platform.items (id INT PRIMARY KEY)"},
		{"trigger", "CREATE TRIGGER trg BEFORE INSERT ON items FOR EACH ROW SET NEW.id = 1"},
		{"view", "CREATE VIEW v AS SELECT 1"},
		{"alter add column", "ALTER TABLE items ADD COLUMN extra INT"},
		{"drop table", "DROP TABLE items"},
		{"duplicate table", "CREATE TABLE items (id INT PRIMARY KEY); CREATE TABLE items (id INT PRIMARY KEY)"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ValidateSQLScript(test.script, SQLSchema, defaultScriptLimits()); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestValidateSQLScriptRejectsSeedViolations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		script string
	}{
		{"insert select", "INSERT INTO items (id) SELECT id FROM other"},
		{"no columns", "INSERT INTO items VALUES (1)"},
		{"non literal value", "INSERT INTO items (id) VALUES (RAND())"},
		{"replace", "REPLACE INTO items (id) VALUES (1)"},
		{"on duplicate", "INSERT INTO items (id) VALUES (1) ON DUPLICATE KEY UPDATE id = 2"},
		{"update statement", "UPDATE items SET id = 2"},
		{"cross database", "INSERT INTO platform.items (id) VALUES (1)"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ValidateSQLScript(test.script, SQLSeed, defaultScriptLimits()); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestValidateSQLScriptEnforcesLimits(t *testing.T) {
	t.Parallel()

	limits := defaultScriptLimits()
	limits.MaxTables = 1
	script := "CREATE TABLE a (id INT PRIMARY KEY); CREATE TABLE b (id INT PRIMARY KEY)"
	if _, err := ValidateSQLScript(script, SQLSchema, limits); err == nil {
		t.Fatal("expected a table-count limit error")
	}

	rowLimits := defaultScriptLimits()
	rowLimits.MaxRowsPerStatement = 1
	if _, err := ValidateSQLScript("INSERT INTO items (id) VALUES (1), (2)", SQLSeed, rowLimits); err == nil {
		t.Fatal("expected a per-statement row limit error")
	}

	totalLimits := defaultScriptLimits()
	totalLimits.MaxTotalRows = 1
	if _, err := ValidateSQLScript("INSERT INTO items (id) VALUES (1); INSERT INTO items (id) VALUES (2)", SQLSeed, totalLimits); err == nil {
		t.Fatal("expected a total row limit error")
	}
}
