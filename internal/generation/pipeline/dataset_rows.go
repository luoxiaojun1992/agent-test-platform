package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	datasetReadToolName    = "read_dataset_rows"
	datasetReadSchema      = "schemas/read-dataset-rows.schema.json"
	datasetReadDefaultRows = 50
	datasetReadMaxRows     = 200
	datasetReadPageBytes   = 4096
	datasetCoverageRanges  = 16
)

type datasetColumn struct {
	name     string
	dataType string
}

type datasetTable struct {
	name       string
	columns    []datasetColumn
	primaryKey []string
	rowCount   int
}

// datasetRows reads pages of one Dataset's rows from its execution schema. The catalog comes from
// information_schema. A table named by the model is only matched against that catalog, so model
// text reaches SQL only as a catalog name and as bound LIMIT and OFFSET values.
type datasetRows struct {
	database *sql.DB
	tables   []datasetTable
	byName   map[string]int
	coverage datasetCoverage
}

// datasetPage is the read_dataset_rows result. next_offset is null on the last page.
type datasetPage struct {
	Status       string      `json:"status"`
	Table        string      `json:"table"`
	Columns      []string    `json:"columns"`
	Offset       int         `json:"offset"`
	ReturnedRows int         `json:"returned_rows"`
	TotalRows    int         `json:"total_rows"`
	NextOffset   *int        `json:"next_offset"`
	Rows         [][]*string `json:"rows"`
}

// datasetReadSummary is the bounded semantic summary of one page, kept when the page is compacted.
type datasetReadSummary struct {
	Table        string `json:"table"`
	Offset       int    `json:"offset"`
	ReturnedRows int    `json:"returned_rows"`
	TotalRows    int    `json:"total_rows"`
}

// datasetTableSummary is one catalog entry in the Critic's user message.
type datasetTableSummary struct {
	Name       string   `json:"name"`
	RowCount   int      `json:"row_count"`
	PrimaryKey []string `json:"primary_key"`
}

// datasetCoverage is the platform state ledger of a review: the row ranges read per table. It is
// built from the typed summaries of the reads, so compaction of the Tool results cannot lose it.
type datasetCoverage struct {
	Tables map[string]tableCoverage `json:"tables"`
}

type tableCoverage struct {
	TotalRows int      `json:"total_rows"`
	Ranges    [][2]int `json:"ranges"`
}

// loadDatasetRows reads the table catalog, primary keys, and row counts of the execution schema.
// Counts are read once, so every page of a review reports the same totals.
func loadDatasetRows(ctx context.Context, database *sql.DB) (*datasetRows, error) {
	reader := &datasetRows{
		database: database, byName: map[string]int{},
		coverage: datasetCoverage{Tables: map[string]tableCoverage{}},
	}
	err := queryAll(ctx, database, `SELECT TABLE_NAME FROM information_schema.TABLES
WHERE TABLE_SCHEMA = DATABASE() AND TABLE_TYPE = 'BASE TABLE' ORDER BY TABLE_NAME`, 1, func(values []string) error {
		reader.byName[values[0]] = len(reader.tables)
		reader.tables = append(reader.tables, datasetTable{name: values[0]})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list Dataset tables: %w", err)
	}
	err = queryAll(ctx, database, `SELECT TABLE_NAME, COLUMN_NAME, DATA_TYPE FROM information_schema.COLUMNS
WHERE TABLE_SCHEMA = DATABASE() ORDER BY TABLE_NAME, ORDINAL_POSITION`, 3, func(values []string) error {
		index, ok := reader.byName[values[0]]
		if !ok {
			return fmt.Errorf("column of unknown table %s", values[0])
		}
		reader.tables[index].columns = append(reader.tables[index].columns, datasetColumn{name: values[1], dataType: values[2]})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list Dataset columns: %w", err)
	}
	err = queryAll(ctx, database, `SELECT TABLE_NAME, COLUMN_NAME FROM information_schema.KEY_COLUMN_USAGE
WHERE TABLE_SCHEMA = DATABASE() AND CONSTRAINT_NAME = 'PRIMARY' ORDER BY TABLE_NAME, ORDINAL_POSITION`, 2, func(values []string) error {
		index, ok := reader.byName[values[0]]
		if !ok {
			return fmt.Errorf("primary key of unknown table %s", values[0])
		}
		reader.tables[index].primaryKey = append(reader.tables[index].primaryKey, values[1])
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list Dataset primary keys: %w", err)
	}
	for index := range reader.tables {
		var count int
		if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteDatasetIdentifier(reader.tables[index].name)).Scan(&count); err != nil {
			return nil, fmt.Errorf("count Dataset rows: %w", err)
		}
		reader.tables[index].rowCount = count
	}
	return reader, nil
}

// queryAll runs one catalog query and passes each row's text values to visit. It closes the result
// before returning, so the next query can use the same handle.
func queryAll(ctx context.Context, database *sql.DB, query string, width int, visit func(values []string) error) error {
	rows, err := database.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		values := make([]sql.NullString, width)
		pointers := make([]any, width)
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			return err
		}
		texts := make([]string, width)
		for index, value := range values {
			texts[index] = value.String
		}
		if err := visit(texts); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (reader *datasetRows) lookup(name string) (datasetTable, bool) {
	index, ok := reader.byName[name]
	if !ok {
		return datasetTable{}, false
	}
	return reader.tables[index], true
}

func (reader *datasetRows) describe() []datasetTableSummary {
	summaries := make([]datasetTableSummary, 0, len(reader.tables))
	for _, table := range reader.tables {
		summaries = append(summaries, datasetTableSummary{Name: table.name, RowCount: table.rowCount, PrimaryKey: table.primaryKey})
	}
	return summaries
}

// page reads one page in primary-key order, or in every column order when the table has no
// primary key. The page stops before a row that would exceed datasetReadPageBytes, unless the page
// is still empty, so every page makes progress and next_offset always points at the next row.
func (reader *datasetRows) page(ctx context.Context, table datasetTable, offset, limit int) (datasetPage, error) {
	selected := make([]string, len(table.columns))
	names := make([]string, len(table.columns))
	for index, column := range table.columns {
		names[index] = column.name
		quoted := quoteDatasetIdentifier(column.name)
		if isBinaryDataType(column.dataType) {
			selected[index] = "HEX(" + quoted + ")"
		} else {
			selected[index] = "CAST(" + quoted + " AS CHAR)"
		}
	}
	order := table.primaryKey
	if len(order) == 0 {
		order = names
	}
	quotedOrder := make([]string, len(order))
	for index, column := range order {
		quotedOrder[index] = quoteDatasetIdentifier(column)
	}
	query := "SELECT " + strings.Join(selected, ", ") + " FROM " + quoteDatasetIdentifier(table.name) +
		" ORDER BY " + strings.Join(quotedOrder, ", ") + " LIMIT ? OFFSET ?"
	rows, err := reader.database.QueryContext(ctx, query, limit, offset)
	if err != nil {
		return datasetPage{}, fmt.Errorf("read Dataset rows: %w", err)
	}
	defer rows.Close()

	page := datasetPage{
		Status: "OK", Table: table.name, Columns: names, Offset: offset, TotalRows: table.rowCount, Rows: [][]*string{},
	}
	pageBytes := 0
	for rows.Next() {
		values := make([]sql.NullString, len(table.columns))
		pointers := make([]any, len(values))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			return datasetPage{}, fmt.Errorf("scan Dataset row: %w", err)
		}
		row := make([]*string, len(values))
		for index, value := range values {
			if value.Valid {
				text := value.String
				row[index] = &text
			}
		}
		encoded, err := json.Marshal(row)
		if err != nil {
			return datasetPage{}, fmt.Errorf("encode Dataset row: %w", err)
		}
		if len(page.Rows) > 0 && pageBytes+len(encoded) > datasetReadPageBytes {
			break
		}
		pageBytes += len(encoded)
		page.Rows = append(page.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return datasetPage{}, fmt.Errorf("read Dataset rows: %w", err)
	}
	page.ReturnedRows = len(page.Rows)
	if next := offset + page.ReturnedRows; page.ReturnedRows > 0 && next < table.rowCount {
		page.NextOffset = &next
	}
	return page, nil
}

// add records rows read from offset, with the table's total. Ranges are half-open and merged.
func (coverage *datasetCoverage) add(table string, offset, rows, total int) error {
	entry := coverage.Tables[table]
	entry.TotalRows = total
	if rows > 0 {
		entry.Ranges = mergeRanges(entry.Ranges, [2]int{offset, offset + rows})
	}
	if len(entry.Ranges) > datasetCoverageRanges {
		return fmt.Errorf("coverage of table %s has more than %d ranges", table, datasetCoverageRanges)
	}
	coverage.Tables[table] = entry
	return nil
}

func mergeRanges(ranges [][2]int, next [2]int) [][2]int {
	all := append(append([][2]int(nil), ranges...), next)
	sort.Slice(all, func(i, j int) bool { return all[i][0] < all[j][0] })
	merged := make([][2]int, 0, len(all))
	for _, current := range all {
		if last := len(merged) - 1; last >= 0 && current[0] <= merged[last][1] {
			if current[1] > merged[last][1] {
				merged[last][1] = current[1]
			}
			continue
		}
		merged = append(merged, current)
	}
	return merged
}

// foldDatasetRead is the state fold of the Dataset Critic. It adds the rows that each compacted
// read_dataset_rows result delivered to the coverage ledger, using the summary the executor
// recorded for that result.
func foldDatasetRead(previous, record json.RawMessage) (json.RawMessage, error) {
	coverage := datasetCoverage{Tables: map[string]tableCoverage{}}
	if len(previous) > 0 {
		if err := json.Unmarshal(previous, &coverage); err != nil {
			return nil, fmt.Errorf("decode coverage ledger: %w", err)
		}
		if coverage.Tables == nil {
			coverage.Tables = map[string]tableCoverage{}
		}
	}
	var group struct {
		ToolCalls []struct {
			CallID   string `json:"call_id"`
			ToolName string `json:"tool_name"`
		} `json:"tool_calls"`
		ToolResults []struct {
			CallID  string          `json:"call_id"`
			Status  string          `json:"status"`
			Summary json.RawMessage `json:"summary"`
		} `json:"tool_results"`
	}
	if err := json.Unmarshal(record, &group); err != nil {
		return nil, fmt.Errorf("decode compacted Tool group: %w", err)
	}
	names := make(map[string]string, len(group.ToolCalls))
	for _, call := range group.ToolCalls {
		names[call.CallID] = call.ToolName
	}
	for _, result := range group.ToolResults {
		if names[result.CallID] != datasetReadToolName || result.Status != "SUCCESS" {
			continue
		}
		var summary datasetReadSummary
		if err := json.Unmarshal(result.Summary, &summary); err != nil {
			return nil, fmt.Errorf("decode read summary: %w", err)
		}
		if err := coverage.add(summary.Table, summary.Offset, summary.ReturnedRows, summary.TotalRows); err != nil {
			return nil, err
		}
	}
	encoded, err := json.Marshal(coverage)
	if err != nil {
		return nil, fmt.Errorf("encode coverage ledger: %w", err)
	}
	return encoded, nil
}

// quoteDatasetIdentifier quotes a catalog name. Catalog names come from information_schema, so only
// backticks need escaping.
func quoteDatasetIdentifier(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

func isBinaryDataType(dataType string) bool {
	switch strings.ToLower(dataType) {
	case "binary", "varbinary", "tinyblob", "blob", "mediumblob", "longblob", "bit",
		"geometry", "point", "linestring", "polygon", "multipoint", "multilinestring", "multipolygon", "geometrycollection":
		return true
	}
	return false
}
