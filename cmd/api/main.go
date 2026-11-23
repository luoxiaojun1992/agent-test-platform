package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/agent"
	"agent-test-platform/internal/agentregistry"
	"agent-test-platform/internal/analytics"
	"agent-test-platform/internal/catalog"
	"agent-test-platform/internal/config"
	"agent-test-platform/internal/generation"
	"agent-test-platform/internal/httpapi"
	"agent-test-platform/internal/modelcfg"
	"agent-test-platform/internal/redislock"
	runs "agent-test-platform/internal/run"
	"agent-test-platform/internal/runexec"
	"agent-test-platform/internal/secrets"
	mysqlstore "agent-test-platform/internal/store/mysql"
	validationdb "agent-test-platform/internal/validationdb"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
)

const (
	startupTimeout  = 10 * time.Second
	shutdownTimeout = 15 * time.Second
)

func main() {
	if err := run(); err != nil {
		slog.Error("API stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	configuration, err := config.LoadAPI()
	if err != nil {
		return fmt.Errorf("load API configuration: %w", err)
	}
	database, err := openPlatformDatabase(configuration.PlatformMySQLDSN)
	if err != nil {
		return err
	}
	defer database.Close()

	startupContext, cancelStartup := context.WithTimeout(context.Background(), startupTimeout)
	defer cancelStartup()
	if err := database.PingContext(startupContext); err != nil {
		return fmt.Errorf("ping Platform MySQL: %w", err)
	}

	validator, err := contracts.NewValidator()
	if err != nil {
		return fmt.Errorf("compile contracts: %w", err)
	}
	store, err := mysqlstore.NewGenerationStore(database)
	if err != nil {
		return fmt.Errorf("create generation store: %w", err)
	}
	service, err := generation.NewService(store, configuration.GeneratorConfigSHA256)
	if err != nil {
		return fmt.Errorf("create generation service: %w", err)
	}
	secretStore, err := mysqlstore.NewSecretStore(database)
	if err != nil {
		return fmt.Errorf("create secret store: %w", err)
	}
	secretService, err := secrets.NewService(secretStore, configuration.SecretKeyring)
	if err != nil {
		return fmt.Errorf("create secret service: %w", err)
	}
	endpoint, err := validationdb.EndpointFromDSN(configuration.PlatformMySQLDSN)
	if err != nil {
		return err
	}
	targetStore, err := mysqlstore.NewTargetStore(database)
	if err != nil {
		return fmt.Errorf("create Target store: %w", err)
	}
	targetService, err := validationdb.NewService(targetStore, secretService, endpoint)
	if err != nil {
		return fmt.Errorf("create Target service: %w", err)
	}
	agentStore, err := mysqlstore.NewAgentStore(database)
	if err != nil {
		return fmt.Errorf("create Agent store: %w", err)
	}
	agentService, err := agentregistry.NewService(agentStore, secretService)
	if err != nil {
		return fmt.Errorf("create Agent service: %w", err)
	}
	runDatabaseStore, err := mysqlstore.NewRunStore(database)
	if err != nil {
		return fmt.Errorf("create run store: %w", err)
	}
	databaseChecker, err := runexec.NewDatabaseChecker(runDatabaseStore, secretService)
	if err != nil {
		return fmt.Errorf("create Agent database checker: %w", err)
	}
	agentService.SetDatabaseChecker(databaseChecker)
	catalogStore, err := mysqlstore.NewCatalogStore(database)
	if err != nil {
		return fmt.Errorf("create catalog store: %w", err)
	}
	catalogService, err := catalog.NewService(catalogStore)
	if err != nil {
		return fmt.Errorf("create catalog service: %w", err)
	}
	runStore, err := mysqlstore.NewRunStore(database)
	if err != nil {
		return fmt.Errorf("create run store: %w", err)
	}
	runService, err := runs.NewService(runStore)
	if err != nil {
		return fmt.Errorf("create run service: %w", err)
	}
	if configuration.RedisAddr != "" {
		redisClient := redis.NewClient(&redis.Options{Addr: configuration.RedisAddr})
		defer func() { _ = redisClient.Close() }()
		runService.SetAgentLocker(redislock.New(redisClient, 0))
	}
	handler, err := httpapi.NewServer(service, secretService, targetService, agentService, catalogService, runService, validator, configuration.BearerToken)
	if err != nil {
		return fmt.Errorf("create HTTP API: %w", err)
	}

	modelStore, err := mysqlstore.NewModelStore(database)
	if err != nil {
		return fmt.Errorf("create model store: %w", err)
	}
	modelService, err := modelcfg.NewService(modelStore)
	if err != nil {
		return fmt.Errorf("create model service: %w", err)
	}
	handler.SetModelService(modelService)
	analyticsStore, err := mysqlstore.NewAnalyticsStore(database)
	if err != nil {
		return fmt.Errorf("create analytics store: %w", err)
	}
	analyticsService, err := analytics.NewService(analyticsStore)
	if err != nil {
		return fmt.Errorf("create analytics service: %w", err)
	}
	handler.SetAnalyticsService(analyticsService)
	discovery, err := agentregistry.NewDiscovery(secretService, toolFetcher{validator: validator})
	if err != nil {
		return fmt.Errorf("create tool discovery: %w", err)
	}
	handler.SetToolDiscovery(discovery)

	server := &http.Server{
		Addr:              configuration.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 * 1024,
	}
	return serve(server)
}

func openPlatformDatabase(dataSourceName string) (*sql.DB, error) {
	driverConfig, err := platformMySQLConfig(dataSourceName)
	if err != nil {
		return nil, err
	}
	connector, err := mysqldriver.NewConnector(driverConfig)
	if err != nil {
		return nil, fmt.Errorf("create Platform MySQL connector: %w", err)
	}
	database := sql.OpenDB(connector)
	database.SetMaxOpenConns(20)
	database.SetMaxIdleConns(10)
	database.SetConnMaxLifetime(5 * time.Minute)
	database.SetConnMaxIdleTime(time.Minute)
	return database, nil
}

func platformMySQLConfig(dataSourceName string) (*mysqldriver.Config, error) {
	driverConfig, err := mysqldriver.ParseDSN(dataSourceName)
	if err != nil {
		return nil, fmt.Errorf("parse PLATFORM_MYSQL_DSN: %w", err)
	}
	if driverConfig.DBName == "" {
		return nil, fmt.Errorf("PLATFORM_MYSQL_DSN must select a database")
	}
	driverConfig.ParseTime = true
	driverConfig.Loc = time.UTC
	if driverConfig.Timeout == 0 {
		driverConfig.Timeout = 5 * time.Second
	}
	if driverConfig.ReadTimeout == 0 {
		driverConfig.ReadTimeout = 10 * time.Second
	}
	if driverConfig.WriteTimeout == 0 {
		driverConfig.WriteTimeout = 10 * time.Second
	}
	return driverConfig, nil
}

func serve(server *http.Server) error {
	processContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- server.ListenAndServe()
	}()

	slog.Info("API listening", "address", server.Addr)
	select {
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP API: %w", err)
	case <-processContext.Done():
		shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancelShutdown()
		if err := server.Shutdown(shutdownContext); err != nil {
			return fmt.Errorf("shut down HTTP API: %w", err)
		}
		return nil
	}
}

// toolFetcher reads a Target Agent's paginated GET /v1/tools list.
type toolFetcher struct{ validator *contracts.Validator }

func (fetcher toolFetcher) FetchTools(ctx context.Context, endpoint, token string) ([]agentregistry.ToolDefinition, error) {
	client, err := agent.NewSSEClient(endpoint, token, &http.Client{Timeout: 30 * time.Second}, fetcher.validator)
	if err != nil {
		return nil, err
	}
	tools, err := client.ListAllTools(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]agentregistry.ToolDefinition, 0, len(tools))
	for _, tool := range tools {
		out = append(out, agentregistry.ToolDefinition{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema})
	}
	return out, nil
}
