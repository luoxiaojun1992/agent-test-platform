package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"
)

const (
	isolationSchema = "CREATE TABLE orders (id VARCHAR(32) PRIMARY KEY, status VARCHAR(32) NOT NULL) ENGINE=InnoDB"
	isolationSeed   = "INSERT INTO orders (id, status) VALUES ('O10001', 'PAID')"
)

// TestRunDatabasesAreIsolatedOnMySQL84 provisions two Runs from the same validated schema and seed on
// MySQL 8.4. A change made in one Run's database is not visible in the other. Both databases are kept
// after provisioning, and provisioning a Run again is refused without changing its data.
func TestRunDatabasesAreIsolatedOnMySQL84(t *testing.T) {
	admin, config := integrationAdmin(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	target := AdminConnection{Admin: admin, Base: config}

	runA, runB := newTestULID(t), newTestULID(t)
	idA, err := ProvisionRun(ctx, target, runA, []string{isolationSchema}, []string{isolationSeed})
	if err != nil {
		t.Fatalf("provision run A: %v", err)
	}
	idB, err := ProvisionRun(ctx, target, runB, []string{isolationSchema}, []string{isolationSeed})
	if err != nil {
		t.Fatalf("provision run B: %v", err)
	}
	if idA.Database == idB.Database {
		t.Fatal("two Runs share one database")
	}

	databaseA, err := target.OpenDatabase(ctx, idA.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer databaseA.Close()
	databaseB, err := target.OpenDatabase(ctx, idB.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer databaseB.Close()

	if _, err := databaseA.ExecContext(ctx, `UPDATE orders SET status = 'CANCELLED' WHERE id = 'O10001'`); err != nil {
		t.Fatalf("change run A: %v", err)
	}
	var statusB string
	if err := databaseB.QueryRowContext(ctx, `SELECT status FROM orders WHERE id = 'O10001'`).Scan(&statusB); err != nil {
		t.Fatalf("read run B: %v", err)
	}
	if statusB != "PAID" {
		t.Fatalf("run B sees run A's change: status = %q", statusB)
	}

	assertDatabaseExists(ctx, t, admin, idA.Database)
	assertDatabaseExists(ctx, t, admin, idB.Database)

	if _, err := ProvisionRun(ctx, target, runA, []string{isolationSchema}, []string{isolationSeed}); !errors.Is(err, ErrDatabaseExists) {
		t.Fatalf("provisioning run A again: error = %v, want ErrDatabaseExists", err)
	}
	var statusA string
	if err := databaseA.QueryRowContext(ctx, `SELECT status FROM orders WHERE id = 'O10001'`).Scan(&statusA); err != nil {
		t.Fatalf("read run A after the refused provisioning: %v", err)
	}
	if statusA != "CANCELLED" {
		t.Fatalf("a refused provisioning changed run A: status = %q", statusA)
	}
}
