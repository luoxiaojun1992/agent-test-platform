package validation

import (
	"fmt"
	"strings"

	"vitess.io/vitess/go/vt/sqlparser"
)

const (
	SQLSchema SQLContext = "SCHEMA"
	SQLSeed   SQLContext = "SEED"
)

// ColumnInfo is one column extracted from a schema.sql CREATE TABLE statement.
type ColumnInfo struct {
	Name     string
	Type     string
	Nullable bool
}

// ForeignKeyInfo is one foreign key extracted from a schema.sql CREATE TABLE statement.
type ForeignKeyInfo struct {
	Columns           []string
	ReferencedTable   string
	ReferencedColumns []string
}

// TableInfo is one table extracted from a schema.sql CREATE TABLE statement.
type TableInfo struct {
	Name        string
	Columns     []ColumnInfo
	PrimaryKey  []string
	ForeignKeys []ForeignKeyInfo
}

// ScriptLimits bounds a schema.sql or seed.sql script during ValidateSQLScript.
type ScriptLimits struct {
	MaxStatementBytes   int
	MaxScriptBytes      int
	MaxTables           int // enforced for SQLSchema
	MaxRowsPerStatement int // enforced for SQLSeed
	MaxTotalRows        int // enforced for SQLSeed
}

// ScriptInfo is what ValidateSQLScript extracts from a schema.sql or seed.sql script.
type ScriptInfo struct {
	Tables      []TableInfo    // populated for SQLSchema
	RowsByTable map[string]int // populated for SQLSeed
	TotalRows   int            // populated for SQLSeed
	Statements  []string       // every individual statement, in file order, for later execution
}

// ValidateSQLScript parses every statement in a multi-statement script and enforces the
// schema.sql or seed.sql allowlist. Parser gaps fail closed: any statement type outside the
// documented allowlist is rejected.
func ValidateSQLScript(script string, context SQLContext, limits ScriptLimits) (ScriptInfo, error) {
	if context != SQLSchema && context != SQLSeed {
		return ScriptInfo{}, fmt.Errorf("script validation only supports SCHEMA or SEED context, got %s", context)
	}
	if strings.TrimSpace(script) == "" {
		if context == SQLSeed {
			// A reference schema without rows has an empty seed.sql. It holds no statements, so it
			// satisfies the INSERT-only seed allowlist trivially.
			return ScriptInfo{RowsByTable: map[string]int{}}, nil
		}
		return ScriptInfo{}, fmt.Errorf("SQL script is empty")
	}
	if len(script) > limits.MaxScriptBytes {
		return ScriptInfo{}, fmt.Errorf("SQL script exceeds %d bytes", limits.MaxScriptBytes)
	}
	if containsSQLComment(script) {
		return ScriptInfo{}, fmt.Errorf("SQL comments are not allowed")
	}

	parser := sqlparser.NewTestParser()
	pieces, err := parser.SplitStatementToPieces(script)
	if err != nil {
		return ScriptInfo{}, fmt.Errorf("split SQL script: %w", err)
	}

	info := ScriptInfo{RowsByTable: map[string]int{}}
	seenTables := map[string]struct{}{}
	statementCount := 0
	for _, piece := range pieces {
		piece = strings.TrimSpace(piece)
		if piece == "" {
			continue
		}
		statementCount++
		if len(piece) > limits.MaxStatementBytes {
			return ScriptInfo{}, fmt.Errorf("SQL statement exceeds %d bytes", limits.MaxStatementBytes)
		}
		statement, err := parser.Parse(piece)
		if err != nil {
			return ScriptInfo{}, fmt.Errorf("parse SQL statement: %w", err)
		}
		info.Statements = append(info.Statements, piece)
		switch context {
		case SQLSchema:
			table, err := inspectSchemaStatement(statement)
			if err != nil {
				return ScriptInfo{}, err
			}
			if table == nil {
				continue
			}
			if _, exists := seenTables[table.Name]; exists {
				return ScriptInfo{}, fmt.Errorf("table %q is defined more than once", table.Name)
			}
			seenTables[table.Name] = struct{}{}
			info.Tables = append(info.Tables, *table)
			if len(info.Tables) > limits.MaxTables {
				return ScriptInfo{}, fmt.Errorf("schema exceeds %d tables", limits.MaxTables)
			}
		case SQLSeed:
			table, rows, err := inspectSeedStatement(statement, limits.MaxRowsPerStatement)
			if err != nil {
				return ScriptInfo{}, err
			}
			info.RowsByTable[table] += rows
			info.TotalRows += rows
			if info.TotalRows > limits.MaxTotalRows {
				return ScriptInfo{}, fmt.Errorf("seed data exceeds %d total rows", limits.MaxTotalRows)
			}
		}
	}
	if statementCount == 0 {
		return ScriptInfo{}, fmt.Errorf("SQL script has no statements")
	}
	return info, nil
}

func inspectSchemaStatement(statement sqlparser.Statement) (*TableInfo, error) {
	switch typed := statement.(type) {
	case *sqlparser.CreateTable:
		return inspectCreateTable(typed)
	case *sqlparser.AlterTable:
		return nil, inspectCreateIndex(typed)
	default:
		return nil, fmt.Errorf("schema.sql only allows CREATE TABLE and CREATE INDEX, got %T", statement)
	}
}

func inspectCreateTable(node *sqlparser.CreateTable) (*TableInfo, error) {
	if !node.Table.Qualifier.IsEmpty() {
		return nil, fmt.Errorf("cross-database table %s is not allowed", sqlparser.String(node.Table))
	}
	if node.Temp {
		return nil, fmt.Errorf("CREATE TEMPORARY TABLE is not allowed")
	}
	if node.OptLike != nil || node.Select != nil {
		return nil, fmt.Errorf("CREATE TABLE LIKE/AS SELECT is not allowed")
	}
	spec := node.TableSpec
	if spec == nil {
		return nil, fmt.Errorf("CREATE TABLE requires an explicit column list")
	}
	if spec.PartitionOption != nil {
		return nil, fmt.Errorf("table partitioning is not allowed")
	}

	table := TableInfo{Name: strings.ToLower(node.Table.Name.String())}
	for _, column := range spec.Columns {
		name := strings.ToLower(column.Name.String())
		nullable := true
		var options *sqlparser.ColumnTypeOptions
		if column.Type != nil {
			options = column.Type.Options
		}
		if options != nil && options.Null != nil {
			nullable = *options.Null
		}
		typeName := ""
		if column.Type != nil {
			typeName = strings.ToUpper(column.Type.Type)
		}
		table.Columns = append(table.Columns, ColumnInfo{Name: name, Type: typeName, Nullable: nullable})
		if options != nil && options.KeyOpt == sqlparser.ColKeyPrimary {
			table.PrimaryKey = appendUnique(table.PrimaryKey, name)
		}
		if options != nil && options.Reference != nil {
			foreignKey, err := buildForeignKey([]string{name}, options.Reference)
			if err != nil {
				return nil, err
			}
			table.ForeignKeys = append(table.ForeignKeys, foreignKey)
		}
	}
	for _, index := range spec.Indexes {
		if index.Info == nil || index.Info.Type != sqlparser.IndexTypePrimary {
			continue
		}
		for _, column := range index.Columns {
			table.PrimaryKey = appendUnique(table.PrimaryKey, strings.ToLower(column.Column.String()))
		}
	}
	for _, constraint := range spec.Constraints {
		foreignKeyDefinition, ok := constraint.Details.(*sqlparser.ForeignKeyDefinition)
		if !ok {
			return nil, fmt.Errorf("table %q has an unsupported table constraint", table.Name)
		}
		columns := make([]string, 0, len(foreignKeyDefinition.Source))
		for _, column := range foreignKeyDefinition.Source {
			columns = append(columns, strings.ToLower(column.String()))
		}
		foreignKey, err := buildForeignKey(columns, foreignKeyDefinition.ReferenceDefinition)
		if err != nil {
			return nil, err
		}
		table.ForeignKeys = append(table.ForeignKeys, foreignKey)
	}
	if len(table.PrimaryKey) == 0 {
		return nil, fmt.Errorf("table %q has no primary key", table.Name)
	}
	markPrimaryKeyNotNull(&table)
	return &table, nil
}

func markPrimaryKeyNotNull(table *TableInfo) {
	primary := make(map[string]struct{}, len(table.PrimaryKey))
	for _, name := range table.PrimaryKey {
		primary[name] = struct{}{}
	}
	for index := range table.Columns {
		if _, ok := primary[table.Columns[index].Name]; ok {
			table.Columns[index].Nullable = false
		}
	}
}

func buildForeignKey(columns []string, reference *sqlparser.ReferenceDefinition) (ForeignKeyInfo, error) {
	if reference == nil {
		return ForeignKeyInfo{}, fmt.Errorf("foreign key requires a reference definition")
	}
	if !reference.ReferencedTable.Qualifier.IsEmpty() {
		return ForeignKeyInfo{}, fmt.Errorf("cross-database foreign key reference is not allowed")
	}
	referencedColumns := make([]string, 0, len(reference.ReferencedColumns))
	for _, column := range reference.ReferencedColumns {
		referencedColumns = append(referencedColumns, strings.ToLower(column.String()))
	}
	return ForeignKeyInfo{
		Columns: columns, ReferencedTable: strings.ToLower(reference.ReferencedTable.Name.String()),
		ReferencedColumns: referencedColumns,
	}, nil
}

func inspectCreateIndex(node *sqlparser.AlterTable) error {
	if !node.Table.Qualifier.IsEmpty() {
		return fmt.Errorf("cross-database table %s is not allowed", sqlparser.String(node.Table))
	}
	if len(node.AlterOptions) != 1 {
		return fmt.Errorf("schema.sql only allows CREATE INDEX, not a compound ALTER TABLE")
	}
	if _, ok := node.AlterOptions[0].(*sqlparser.AddIndexDefinition); !ok {
		return fmt.Errorf("schema.sql only allows CREATE INDEX")
	}
	return nil
}

func inspectSeedStatement(statement sqlparser.Statement, maxRowsPerStatement int) (string, int, error) {
	insert, ok := statement.(*sqlparser.Insert)
	if !ok {
		return "", 0, fmt.Errorf("seed.sql only allows INSERT, got %T", statement)
	}
	if insert.Action == sqlparser.ReplaceAct {
		return "", 0, fmt.Errorf("REPLACE is not allowed in seed.sql")
	}
	tableName, err := insert.Table.TableName()
	if err != nil {
		return "", 0, fmt.Errorf("resolve INSERT table name: %w", err)
	}
	if !tableName.Qualifier.IsEmpty() {
		return "", 0, fmt.Errorf("cross-database table %s is not allowed", sqlparser.String(tableName))
	}
	if len(insert.Columns) == 0 {
		return "", 0, fmt.Errorf("seed.sql INSERT requires an explicit column list")
	}
	if insert.OnDup != nil {
		return "", 0, fmt.Errorf("ON DUPLICATE KEY UPDATE is not allowed in seed.sql")
	}
	values, ok := insert.Rows.(sqlparser.Values)
	if !ok {
		return "", 0, fmt.Errorf("seed.sql only allows literal VALUES, not INSERT ... SELECT")
	}
	if len(values) == 0 {
		return "", 0, fmt.Errorf("seed.sql INSERT has no rows")
	}
	if len(values) > maxRowsPerStatement {
		return "", 0, fmt.Errorf("INSERT statement exceeds %d rows", maxRowsPerStatement)
	}
	for _, row := range values {
		if len(row) != len(insert.Columns) {
			return "", 0, fmt.Errorf("INSERT row has %d values, want %d", len(row), len(insert.Columns))
		}
		for _, value := range row {
			if _, ok := literalValue(value); !ok {
				return "", 0, fmt.Errorf("seed.sql values must be literals")
			}
		}
	}
	return strings.ToLower(tableName.Name.String()), len(values), nil
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
