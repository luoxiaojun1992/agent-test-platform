// Command secrets-rewrap re-seals every stored credential under the current encryption key
// (docs/technical/06-sandbox-and-security.md section 8.1). An operator runs it once per key rotation,
// after the API and worker have restarted with the new keyring. It logs how many credentials remain under
// each key id and exits with status 1 when a credential cannot be opened. Credential values are never logged.
package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"agent-test-platform/internal/secrets"
	mysqlstore "agent-test-platform/internal/store/mysql"

	mysqldriver "github.com/go-sql-driver/mysql"
)

const rewrapBatchSize = 100

func main() {
	if err := run(); err != nil {
		slog.Error("secrets rewrap failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := os.Getenv("PLATFORM_MYSQL_DSN")
	if dsn == "" {
		return errors.New("PLATFORM_MYSQL_DSN is required")
	}
	keyring, err := secrets.LoadKeyring(os.LookupEnv)
	if err != nil {
		return err
	}
	database, err := openPlatformDatabase(dsn)
	if err != nil {
		return err
	}
	defer database.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := mysqlstore.NewSecretStore(database)
	if err != nil {
		return err
	}
	service, err := secrets.NewService(store, keyring)
	if err != nil {
		return err
	}
	report, err := service.Rewrap(ctx, rewrapBatchSize)
	if err != nil {
		return err
	}
	slog.Info("credentials rewrapped", "rewrapped", report.Rewrapped, "current_key_id", keyring.CurrentID())
	keyIDs := make([]string, 0, len(report.RemainingByKey))
	for keyID := range report.RemainingByKey {
		keyIDs = append(keyIDs, keyID)
	}
	sort.Strings(keyIDs)
	for _, keyID := range keyIDs {
		slog.Info("credentials remaining under key", "key_id", keyID, "count", report.RemainingByKey[keyID])
	}
	return nil
}

func openPlatformDatabase(dataSourceName string) (*sql.DB, error) {
	driverConfig, err := mysqldriver.ParseDSN(dataSourceName)
	if err != nil {
		return nil, errors.New("parse PLATFORM_MYSQL_DSN: invalid DSN")
	}
	if driverConfig.DBName == "" {
		return nil, errors.New("PLATFORM_MYSQL_DSN must name the platform database")
	}
	driverConfig.ParseTime = true
	driverConfig.Loc = time.UTC
	driverConfig.Timeout = 5 * time.Second
	driverConfig.ReadTimeout = 30 * time.Second
	driverConfig.WriteTimeout = 30 * time.Second
	connector, err := mysqldriver.NewConnector(driverConfig)
	if err != nil {
		return nil, errors.New("create Platform MySQL connector")
	}
	database := sql.OpenDB(connector)
	database.SetMaxOpenConns(2)
	return database, nil
}
