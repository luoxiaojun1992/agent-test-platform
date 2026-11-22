// Command purge-drafts deletes expired Generation staging drafts of finished Jobs. It runs once, when
// an operator starts it, prints how many rows it deleted, and exits. Nothing schedules it
// (docs/technical/01-storage-and-workflows.md, section 2).
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"agent-test-platform/internal/generation"
	"agent-test-platform/internal/jobs/stagingpurge"
	mysqlstore "agent-test-platform/internal/store/mysql"

	mysqldriver "github.com/go-sql-driver/mysql"
)

func main() {
	os.Exit(run())
}

func run() int {
	dsn, err := platformDSN(os.LookupEnv)
	if err != nil {
		slog.Error("Staging draft purge not started", "error", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result, err := purgeDrafts(ctx, dsn)
	slog.Info("Expired staging drafts purged", "artifact_drafts", result.ArtifactDrafts, "test_case_drafts", result.TestCaseDrafts)
	if err != nil {
		slog.Error("Staging draft purge failed", "error", err)
		return 1
	}
	return 0
}

// platformDSN returns PLATFORM_MYSQL_DSN, the only setting the purge needs.
func platformDSN(lookup func(string) (string, bool)) (string, error) {
	value, found := lookup("PLATFORM_MYSQL_DSN")
	if !found || strings.TrimSpace(value) == "" {
		return "", errors.New("PLATFORM_MYSQL_DSN is required")
	}
	return strings.TrimSpace(value), nil
}

// purgeDrafts deletes the expired drafts reachable through dsn and reports what it deleted.
func purgeDrafts(ctx context.Context, dsn string) (generation.DraftPurge, error) {
	database, err := openPlatformDatabase(dsn)
	if err != nil {
		return generation.DraftPurge{}, err
	}
	defer database.Close()
	store, err := mysqlstore.NewGenerationStore(database)
	if err != nil {
		return generation.DraftPurge{}, fmt.Errorf("create generation store: %w", err)
	}
	purger, err := stagingpurge.New(store, stagingpurge.DefaultBatchSize)
	if err != nil {
		return generation.DraftPurge{}, fmt.Errorf("create staging purge: %w", err)
	}
	result, err := purger.PurgeOnce(ctx)
	if err != nil {
		return result, fmt.Errorf("purge expired drafts: %w", err)
	}
	return result, nil
}

// openPlatformDatabase connects with the same driver settings as the worker: UTC times and bounded
// timeouts.
func openPlatformDatabase(dataSourceName string) (*sql.DB, error) {
	driverConfig, err := mysqldriver.ParseDSN(dataSourceName)
	if err != nil {
		return nil, fmt.Errorf("parse PLATFORM_MYSQL_DSN: %w", err)
	}
	if driverConfig.DBName == "" {
		return nil, errors.New("PLATFORM_MYSQL_DSN must name the platform database")
	}
	driverConfig.ParseTime = true
	driverConfig.Loc = time.UTC
	driverConfig.Timeout = 5 * time.Second
	driverConfig.ReadTimeout = 10 * time.Second
	driverConfig.WriteTimeout = 10 * time.Second
	connector, err := mysqldriver.NewConnector(driverConfig)
	if err != nil {
		return nil, fmt.Errorf("create MySQL connector: %w", err)
	}
	database := sql.OpenDB(connector)
	database.SetMaxOpenConns(2)
	database.SetMaxIdleConns(1)
	database.SetConnMaxLifetime(5 * time.Minute)
	return database, nil
}
