package validation

import (
	"fmt"
	"strings"

	"vitess.io/vitess/go/vt/sqlparser"
)

type SQLContext string

const (
	SQLAssertion SQLContext = "ASSERTION"
	SQLQuery     SQLContext = "QUERY"
	SQLExecute   SQLContext = "EXECUTE"
)

type SQLInfo struct {
	Operation           string
	Tables              []string
	SelectedColumns     []string
	Predicates          []Predicate
	Assignments         []Assignment
	SimpleMatchComplete bool
	Canonical           string
	Ordered             bool
	Statement           sqlparser.Statement
}

type Predicate struct {
	Column   string
	Operator string
	Value    any
}

type Assignment struct {
	Column string
	Value  any
}

// ValidateSQL parses one statement and enforces the MVP SQL allowlist.
func ValidateSQL(query string, context SQLContext) (SQLInfo, error) {
	if strings.TrimSpace(query) == "" {
		return SQLInfo{}, fmt.Errorf("SQL is empty")
	}
	if len(query) > 4096 {
		return SQLInfo{}, fmt.Errorf("SQL exceeds 4096 characters")
	}
	if containsSQLComment(query) {
		return SQLInfo{}, fmt.Errorf("SQL comments are not allowed")
	}

	parser := sqlparser.NewTestParser()
	pieces, err := parser.SplitStatementToPieces(query)
	if err != nil {
		return SQLInfo{}, fmt.Errorf("split SQL: %w", err)
	}
	if len(pieces) != 1 || strings.TrimSpace(pieces[0]) == "" {
		return SQLInfo{}, fmt.Errorf("exactly one SQL statement is required")
	}
	statement, err := parser.Parse(pieces[0])
	if err != nil {
		return SQLInfo{}, fmt.Errorf("parse SQL: %w", err)
	}

	operation := sqlparser.ASTToStatementType(statement).String()
	if insert, ok := statement.(*sqlparser.Insert); ok && insert.Action == sqlparser.ReplaceAct {
		return SQLInfo{}, fmt.Errorf("REPLACE is not allowed")
	}
	if err := allowStatement(operation, context); err != nil {
		return SQLInfo{}, err
	}
	analysis, err := inspectAST(statement)
	if err != nil {
		return SQLInfo{}, err
	}
	return SQLInfo{
		Operation: operation, Tables: analysis.tables,
		SelectedColumns: analysis.selectedColumns, Predicates: analysis.predicates,
		Assignments: analysis.assignments, SimpleMatchComplete: analysis.simpleMatchComplete,
		Canonical: sqlparser.String(statement), Ordered: statementOrdered(statement), Statement: statement,
	}, nil
}

func statementOrdered(statement sqlparser.Statement) bool {
	tableStatement, ok := statement.(sqlparser.TableStatement)
	return ok && len(tableStatement.GetOrderBy()) > 0
}

func allowStatement(operation string, context SQLContext) error {
	switch context {
	case SQLAssertion, SQLQuery:
		if operation != "SELECT" {
			return fmt.Errorf("%s context only allows SELECT, got %s", context, operation)
		}
	case SQLExecute:
		if operation != "INSERT" && operation != "UPDATE" && operation != "DELETE" {
			return fmt.Errorf("EXECUTE context only allows INSERT, UPDATE, or DELETE, got %s", operation)
		}
	default:
		return fmt.Errorf("unknown SQL context %q", context)
	}
	return nil
}

type astAnalysis struct {
	tables              []string
	selectedColumns     []string
	predicates          []Predicate
	assignments         []Assignment
	simpleMatchComplete bool
}

func inspectAST(statement sqlparser.Statement) (astAnalysis, error) {
	tableSet := make(map[string]struct{})
	var inspectionErr error
	err := sqlparser.Walk(func(node sqlparser.SQLNode) (bool, error) {
		if inspectionErr != nil {
			return false, inspectionErr
		}
		switch typed := node.(type) {
		case sqlparser.TableName:
			if !typed.Qualifier.IsEmpty() {
				inspectionErr = fmt.Errorf("cross-database table reference %s is not allowed", sqlparser.String(typed))
				return false, inspectionErr
			}
		case *sqlparser.AliasedTableExpr:
			table, ok := typed.Expr.(sqlparser.TableName)
			if ok && !table.Name.IsEmpty() && !strings.EqualFold(table.Name.String(), "dual") {
				tableSet[strings.ToLower(table.Name.String())] = struct{}{}
			}
		case *sqlparser.Variable:
			inspectionErr = fmt.Errorf("SQL variables are not allowed")
			return false, inspectionErr
		case *sqlparser.AssignmentExpr:
			inspectionErr = fmt.Errorf("SQL variable assignment is not allowed")
			return false, inspectionErr
		case *sqlparser.CurTimeFuncExpr:
			inspectionErr = fmt.Errorf("nondeterministic time function %s is not allowed", typed.Name.String())
			return false, inspectionErr
		case *sqlparser.FuncExpr:
			if !typed.Qualifier.IsEmpty() {
				inspectionErr = fmt.Errorf("qualified function %s is not allowed", sqlparser.String(typed))
				return false, inspectionErr
			}
			if nondeterministicFunction(typed.Name.String()) {
				inspectionErr = fmt.Errorf("nondeterministic function %s is not allowed", typed.Name.String())
				return false, inspectionErr
			}
		case *sqlparser.Select:
			if typed.Lock != 0 {
				inspectionErr = fmt.Errorf("SELECT locking clauses are not allowed")
				return false, inspectionErr
			}
			if typed.Into != nil {
				inspectionErr = fmt.Errorf("SELECT INTO is not allowed")
				return false, inspectionErr
			}
		case *sqlparser.Union:
			if typed.Lock != 0 {
				inspectionErr = fmt.Errorf("UNION locking clauses are not allowed")
				return false, inspectionErr
			}
			if typed.Into != nil {
				inspectionErr = fmt.Errorf("UNION INTO is not allowed")
				return false, inspectionErr
			}
		}
		return true, nil
	}, statement)
	if err != nil {
		return astAnalysis{}, fmt.Errorf("inspect SQL AST: %w", err)
	}
	if inspectionErr != nil {
		return astAnalysis{}, inspectionErr
	}
	tables := make([]string, 0, len(tableSet))
	for table := range tableSet {
		tables = append(tables, table)
	}
	slicesSort(tables)
	analysis := astAnalysis{tables: tables, simpleMatchComplete: true}
	switch typed := statement.(type) {
	case *sqlparser.Select:
		analysis.selectedColumns, analysis.simpleMatchComplete = selectedColumns(typed)
		analysis.predicates, analysis.simpleMatchComplete = mergePredicates(analysis.simpleMatchComplete, typed.Where)
	case *sqlparser.Update:
		analysis.assignments, analysis.simpleMatchComplete = assignments(typed.Exprs)
		analysis.predicates, analysis.simpleMatchComplete = mergePredicates(analysis.simpleMatchComplete, typed.Where)
	case *sqlparser.Delete:
		analysis.predicates, analysis.simpleMatchComplete = mergePredicates(analysis.simpleMatchComplete, typed.Where)
	case *sqlparser.Insert:
		analysis.assignments, analysis.simpleMatchComplete = insertAssignments(typed)
	case *sqlparser.Union:
		analysis.simpleMatchComplete = false
	}
	return analysis, nil
}

func selectedColumns(statement *sqlparser.Select) ([]string, bool) {
	if statement.SelectExprs == nil {
		return nil, false
	}
	columns := make([]string, 0, len(statement.SelectExprs.Exprs))
	for _, expression := range statement.SelectExprs.Exprs {
		aliased, ok := expression.(*sqlparser.AliasedExpr)
		if !ok {
			return nil, false
		}
		column, ok := aliased.Expr.(*sqlparser.ColName)
		if !ok {
			return nil, false
		}
		columns = append(columns, strings.ToLower(column.Name.String()))
	}
	return columns, true
}

func mergePredicates(complete bool, where *sqlparser.Where) ([]Predicate, bool) {
	if where == nil {
		return nil, complete
	}
	predicates, ok := flattenPredicates(where.Expr)
	return predicates, complete && ok
}

func flattenPredicates(expression sqlparser.Expr) ([]Predicate, bool) {
	switch typed := expression.(type) {
	case *sqlparser.AndExpr:
		left, leftOK := flattenPredicates(typed.Left)
		right, rightOK := flattenPredicates(typed.Right)
		return append(left, right...), leftOK && rightOK
	case *sqlparser.ComparisonExpr:
		column, ok := typed.Left.(*sqlparser.ColName)
		if !ok {
			return nil, false
		}
		value, ok := literalValue(typed.Right)
		if !ok {
			return nil, false
		}
		return []Predicate{{Column: strings.ToLower(column.Name.String()), Operator: strings.ToUpper(typed.Operator.ToString()), Value: value}}, true
	case *sqlparser.IsExpr:
		column, ok := typed.Left.(*sqlparser.ColName)
		if !ok {
			return nil, false
		}
		operator := strings.ToUpper(strings.ReplaceAll(typed.Right.ToString(), " ", "_"))
		if operator != "IS_NULL" && operator != "IS_NOT_NULL" {
			return nil, false
		}
		return []Predicate{{Column: strings.ToLower(column.Name.String()), Operator: operator}}, true
	default:
		return nil, false
	}
}

func assignments(expressions sqlparser.UpdateExprs) ([]Assignment, bool) {
	result := make([]Assignment, 0, len(expressions))
	for _, expression := range expressions {
		value, ok := literalValue(expression.Expr)
		if !ok {
			return nil, false
		}
		result = append(result, Assignment{Column: strings.ToLower(expression.Name.Name.String()), Value: value})
	}
	return result, true
}

func insertAssignments(statement *sqlparser.Insert) ([]Assignment, bool) {
	values, ok := statement.Rows.(sqlparser.Values)
	if !ok || len(values) != 1 || len(statement.Columns) != len(values[0]) {
		return nil, false
	}
	result := make([]Assignment, 0, len(statement.Columns))
	for index, column := range statement.Columns {
		value, ok := literalValue(values[0][index])
		if !ok {
			return nil, false
		}
		result = append(result, Assignment{Column: strings.ToLower(column.String()), Value: value})
	}
	return result, true
}

func literalValue(expression sqlparser.Expr) (any, bool) {
	switch typed := expression.(type) {
	case *sqlparser.Literal:
		return typed.Val, true
	case *sqlparser.NullVal:
		return nil, true
	case sqlparser.BoolVal:
		return bool(typed), true
	default:
		return nil, false
	}
}

func nondeterministicFunction(name string) bool {
	switch strings.ToLower(name) {
	case "rand", "random_bytes", "uuid", "uuid_short", "sysdate", "sleep", "benchmark",
		"connection_id", "current_user", "session_user", "system_user", "user":
		return true
	default:
		return false
	}
}

func containsSQLComment(query string) bool {
	var quote byte
	for index := 0; index < len(query); index++ {
		char := query[index]
		next := byte(0)
		if index+1 < len(query) {
			next = query[index+1]
		}
		if quote != 0 {
			if char == '\\' && quote != '`' && next != 0 {
				index++
				continue
			}
			if char == quote {
				if next == quote {
					index++
					continue
				}
				quote = 0
			}
			continue
		}
		switch {
		case char == '\'' || char == '"' || char == '`':
			quote = char
		case char == '#':
			return true
		case char == '/' && next == '*':
			return true
		case char == '-' && next == '-' && (index+2 == len(query) || isSQLSpace(query[index+2])):
			return true
		}
	}
	return false
}

func isSQLSpace(char byte) bool {
	return char == ' ' || char == '\t' || char == '\n' || char == '\r' || char == '\f'
}

func slicesSort(values []string) {
	for index := 1; index < len(values); index++ {
		for current := index; current > 0 && values[current] < values[current-1]; current-- {
			values[current], values[current-1] = values[current-1], values[current]
		}
	}
}
