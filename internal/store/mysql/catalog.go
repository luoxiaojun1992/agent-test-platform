package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-test-platform/internal/catalog"
)

type kindSQL struct {
	table, revisionTable, parentColumn, columns string
	scan                                        func(row rowScanner) (catalog.Row, error)
}

var catalogKinds = map[catalog.Kind]kindSQL{
	catalog.Scenario: {
		table: "scenario", revisionTable: "scenario_revision", parentColumn: "scenario_id",
		columns: "id, scenario_id, revision_no, quality_level, content_json, content_sha256, lifecycle_status, created_at",
		scan: func(row rowScanner) (catalog.Row, error) {
			var id, parent, quality, digest, status string
			var no uint
			var content []byte
			var created time.Time
			if err := row.Scan(&id, &parent, &no, &quality, &content, &digest, &status, &created); err != nil {
				return catalog.Row{}, err
			}
			return catalog.Row{ID: id, CreatedAt: created, Body: map[string]any{
				"id": id, "scenario_id": parent, "revision_no": no, "quality_level": quality,
				"content": json.RawMessage(content), "content_sha256": digest, "lifecycle_status": status, "created_at": created}}, nil
		},
	},
	catalog.Dataset: {
		table: "dataset", revisionTable: "dataset_revision", parentColumn: "dataset_id",
		columns: "id, dataset_id, scenario_revision_id, revision_no, schema_sql, seed_sql, constraints_json, metadata_json, content_sha256, lifecycle_status, created_at",
		scan: func(row rowScanner) (catalog.Row, error) {
			var id, parent, scenario, schema, seed, digest, status string
			var no uint
			var constraints, metadata []byte
			var created time.Time
			if err := row.Scan(&id, &parent, &scenario, &no, &schema, &seed, &constraints, &metadata, &digest, &status, &created); err != nil {
				return catalog.Row{}, err
			}
			return catalog.Row{ID: id, CreatedAt: created, Body: map[string]any{
				"id": id, "dataset_id": parent, "scenario_revision_id": scenario, "revision_no": no, "schema_sql": schema, "seed_sql": seed,
				"constraints": json.RawMessage(constraints), "metadata": json.RawMessage(metadata),
				"content_sha256": digest, "lifecycle_status": status, "created_at": created}}, nil
		},
	},
	catalog.TestCase: {
		table: "test_case", revisionTable: "test_case_revision", parentColumn: "test_case_id",
		columns: "id, test_case_id, scenario_revision_id, dataset_revision_id, revision_no, content_json, content_sha256, lifecycle_status, created_at",
		scan: func(row rowScanner) (catalog.Row, error) {
			var id, parent, scenario, dataset, digest, status string
			var no uint
			var content []byte
			var created time.Time
			if err := row.Scan(&id, &parent, &scenario, &dataset, &no, &content, &digest, &status, &created); err != nil {
				return catalog.Row{}, err
			}
			return catalog.Row{ID: id, CreatedAt: created, Body: map[string]any{
				"id": id, "test_case_id": parent, "scenario_revision_id": scenario, "dataset_revision_id": dataset, "revision_no": no, "content": json.RawMessage(content),
				"content_sha256": digest, "lifecycle_status": status, "created_at": created}}, nil
		},
	},
}

// CatalogStore reads published Scenarios, Datasets and Test Cases.
type CatalogStore struct{ database *sql.DB }

// NewCatalogStore returns a read-only catalog store over Platform MySQL.
func NewCatalogStore(database *sql.DB) (*CatalogStore, error) {
	if database == nil {
		return nil, errors.New("Platform MySQL database is required")
	}
	return &CatalogStore{database: database}, nil
}

func (store *CatalogStore) List(ctx context.Context, kind catalog.Kind, after *catalog.Cursor, limit int) ([]catalog.Row, error) {
	spec, ok := catalogKinds[kind]
	if !ok {
		return nil, fmt.Errorf("unknown catalog kind %q", kind)
	}
	query := `SELECT id, created_at FROM ` + spec.table
	args := []any{}
	if after != nil {
		query += ` WHERE (created_at < ? OR (created_at = ? AND id < ?))`
		args = append(args, after.CreatedAt, after.CreatedAt, after.ID)
	}
	rows, err := store.database.QueryContext(ctx, query+` ORDER BY created_at DESC, id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", spec.table, err)
	}
	defer rows.Close()
	var items []catalog.Row
	for rows.Next() {
		var id string
		var created time.Time
		if err := rows.Scan(&id, &created); err != nil {
			return nil, fmt.Errorf("scan %s: %w", spec.table, err)
		}
		items = append(items, catalog.Row{ID: id, CreatedAt: created, Body: map[string]any{"id": id, "created_at": created}})
	}
	return items, rows.Err()
}

func (store *CatalogStore) ListRevisions(ctx context.Context, kind catalog.Kind, parentID string, after *catalog.Cursor, limit int) ([]catalog.Row, error) {
	spec, ok := catalogKinds[kind]
	if !ok {
		return nil, fmt.Errorf("unknown catalog kind %q", kind)
	}
	query := `SELECT ` + spec.columns + ` FROM ` + spec.revisionTable + ` WHERE ` + spec.parentColumn + ` = ?`
	args := []any{parentID}
	if after != nil {
		query += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
		args = append(args, after.CreatedAt, after.CreatedAt, after.ID)
	}
	rows, err := store.database.QueryContext(ctx, query+` ORDER BY created_at DESC, id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", spec.revisionTable, err)
	}
	defer rows.Close()
	var items []catalog.Row
	for rows.Next() {
		item, err := spec.scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", spec.revisionTable, err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// SearchRevisions lists the revisions of every asset of a kind, newest first. A query matches the revision id, the logical asset id
// and the JSON content (Scenario name and domain, Test Case prompt, Dataset schema).
func (store *CatalogStore) SearchRevisions(ctx context.Context, kind catalog.Kind, query string, after *catalog.Cursor, limit int) ([]catalog.Row, error) {
	spec, ok := catalogKinds[kind]
	if !ok {
		return nil, fmt.Errorf("unknown catalog kind %q", kind)
	}
	text := "COALESCE(CAST(content_json AS CHAR), '')"
	if kind == catalog.Dataset {
		text = "CONCAT(COALESCE(CAST(metadata_json AS CHAR), ''), schema_sql)"
	}
	var conditions []string
	var args []any
	if query != "" {
		like := "%" + escapeLike(query) + "%"
		conditions = append(conditions, "(id LIKE ? OR "+spec.parentColumn+" LIKE ? OR "+text+" LIKE ?)")
		args = append(args, like, like, like)
	}
	if after != nil {
		conditions = append(conditions, "(created_at < ? OR (created_at = ? AND id < ?))")
		args = append(args, after.CreatedAt, after.CreatedAt, after.ID)
	}
	statement := `SELECT ` + spec.columns + ` FROM ` + spec.revisionTable
	if len(conditions) > 0 {
		statement += ` WHERE ` + strings.Join(conditions, " AND ")
	}
	rows, err := store.database.QueryContext(ctx, statement+` ORDER BY created_at DESC, id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("search %s: %w", spec.revisionTable, err)
	}
	defer rows.Close()
	var items []catalog.Row
	for rows.Next() {
		item, err := spec.scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", spec.revisionTable, err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func escapeLike(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
}

func (store *CatalogStore) GetRevision(ctx context.Context, kind catalog.Kind, id string) (catalog.Row, error) {
	spec, ok := catalogKinds[kind]
	if !ok {
		return catalog.Row{}, fmt.Errorf("unknown catalog kind %q", kind)
	}
	row, err := spec.scan(store.database.QueryRowContext(ctx, `SELECT `+spec.columns+` FROM `+spec.revisionTable+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return catalog.Row{}, catalog.ErrNotFound
	}
	if err != nil {
		return catalog.Row{}, fmt.Errorf("load %s: %w", spec.revisionTable, err)
	}
	return row, nil
}

func (store *CatalogStore) Exists(ctx context.Context, kind catalog.Kind, id string) (bool, error) {
	spec, ok := catalogKinds[kind]
	if !ok {
		return false, fmt.Errorf("unknown catalog kind %q", kind)
	}
	var found int
	err := store.database.QueryRowContext(ctx, `SELECT 1 FROM `+spec.table+` WHERE id = ?`, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
