package sandbox

import (
	"database/sql"
	"encoding/base64"
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestBuildSnapshotQuery(t *testing.T) {
	t.Parallel()

	query, err := BuildSnapshotQuery(TableSpec{
		Name:       "Orders",
		Columns:    []ColumnSpec{{Name: "ID", Type: CellString}, {Name: "status", Type: CellString}},
		PrimaryKey: []string{"ID"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "SELECT `id`, `status` FROM `orders` ORDER BY `id`"
	if query != want {
		t.Fatalf("query = %q, want %q", query, want)
	}
}

func TestBuildSnapshotQueryRejectsUnsafeCatalog(t *testing.T) {
	t.Parallel()

	tests := []TableSpec{
		{Name: "orders; DROP TABLE agent", Columns: []ColumnSpec{{Name: "id", Type: CellString}}, PrimaryKey: []string{"id"}},
		{Name: "orders", Columns: []ColumnSpec{{Name: "id", Type: CellString}}, PrimaryKey: []string{"missing"}},
		{Name: "orders", Columns: []ColumnSpec{{Name: "ID", Type: CellString}, {Name: "id", Type: CellString}}, PrimaryKey: []string{"id"}},
	}
	for _, table := range tests {
		if _, err := BuildSnapshotQuery(table); err == nil {
			t.Errorf("unsafe table passed: %#v", table)
		}
	}
}

func TestCellFromRaw(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		cellType CellType
		raw      sql.RawBytes
		want     Cell
	}{
		{name: "null", cellType: CellString, raw: nil, want: Cell{Type: CellNull}},
		{name: "integer", cellType: CellInteger, raw: []byte("42"), want: Cell{Type: CellInteger, Value: "42"}},
		{name: "large integer", cellType: CellInteger, raw: []byte("18446744073709551615"), want: Cell{Type: CellInteger, Value: "18446744073709551615"}},
		{name: "boolean", cellType: CellBoolean, raw: []byte("1"), want: Cell{Type: CellBoolean, Value: "true"}},
		{name: "datetime", cellType: CellDateTime, raw: []byte("2026-09-11 10:00:00.123456"), want: Cell{Type: CellDateTime, Value: "2026-09-11T10:00:00.123456Z"}},
		{name: "binary", cellType: CellBinary, raw: []byte{0x00, 0xff}, want: Cell{Type: CellBinary, Value: base64.RawStdEncoding.EncodeToString([]byte{0x00, 0xff})}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := cellFromRaw(test.cellType, test.raw)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("cell = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestNormalizeCreateTableIgnoresAutoIncrementCounter(t *testing.T) {
	t.Parallel()

	first, err := normalizeCreateTable("CREATE TABLE `orders` (`id` bigint NOT NULL AUTO_INCREMENT, PRIMARY KEY (`id`)) ENGINE=InnoDB AUTO_INCREMENT=2 DEFAULT CHARSET=utf8mb4")
	if err != nil {
		t.Fatal(err)
	}
	second, err := normalizeCreateTable("CREATE TABLE `orders` (`id` bigint NOT NULL AUTO_INCREMENT, PRIMARY KEY (`id`)) ENGINE=InnoDB AUTO_INCREMENT=99 DEFAULT CHARSET=utf8mb4")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("runtime AUTO_INCREMENT changed schema\nfirst: %s\nsecond: %s", first, second)
	}
}

func TestNormalizeCreateTableRejectsPartialDDL(t *testing.T) {
	t.Parallel()

	if _, err := normalizeCreateTable("CREATE TABLE orders disregard invalid tail"); err == nil {
		t.Fatal("partial DDL was accepted")
	}
}

func TestParseMySQLDateTime(t *testing.T) {
	t.Parallel()

	parsed, err := parseMySQLDateTime("2026-09-11 10:00:00")
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Equal(time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("parsed = %s", parsed)
	}
	if _, err := parseMySQLDateTime("not-a-time"); err == nil {
		t.Fatal("invalid datetime was accepted")
	}
}

func TestComputeSchemaDigest(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	listQuery := regexp.QuoteMeta("SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE' ORDER BY table_name")
	mock.ExpectQuery(listQuery).WillReturnRows(sqlmock.NewRows([]string{"table_name"}).AddRow("customer").AddRow("orders"))
	mock.ExpectQuery(regexp.QuoteMeta("SHOW CREATE TABLE `customer`")).WillReturnRows(
		sqlmock.NewRows([]string{"Table", "Create Table"}).AddRow("customer", "CREATE TABLE `customer` (`id` varchar(32) NOT NULL, PRIMARY KEY (`id`)) ENGINE=InnoDB"),
	)
	mock.ExpectQuery(regexp.QuoteMeta("SHOW CREATE TABLE `orders`")).WillReturnRows(
		sqlmock.NewRows([]string{"Table", "Create Table"}).AddRow("orders", "CREATE TABLE `orders` (`id` varchar(32) NOT NULL, PRIMARY KEY (`id`)) ENGINE=InnoDB"),
	)
	digest, err := ComputeSchemaDigest(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	if len(digest) != 64 {
		t.Fatalf("digest = %q", digest)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestComputeSchemaDigestRejectsEmptyDatabase(t *testing.T) {
	t.Parallel()

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	listQuery := regexp.QuoteMeta("SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE' ORDER BY table_name")
	mock.ExpectQuery(listQuery).WillReturnRows(sqlmock.NewRows([]string{"table_name"}))
	if _, err := ComputeSchemaDigest(t.Context(), database); err == nil {
		t.Fatal("empty database produced a schema digest")
	}
}
