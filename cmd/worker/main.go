package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/evaluation"
	"agent-test-platform/internal/generation/pipeline"
	"agent-test-platform/internal/jobs/evalworker"
	"agent-test-platform/internal/jobs/generationworker"
	"agent-test-platform/internal/jobs/mysqlqueue"
	"agent-test-platform/internal/jobs/runworker"
	"agent-test-platform/internal/llm"
	"agent-test-platform/internal/modelcfg"
	"agent-test-platform/internal/runexec"
	"agent-test-platform/internal/secrets"
	"agent-test-platform/internal/snapshot"
	mysqlstore "agent-test-platform/internal/store/mysql"
	validationdb "agent-test-platform/internal/validationdb"

	mysqldriver "github.com/go-sql-driver/mysql"
)

type config struct {
	PlatformMySQLDSN string
	Owner            string
	Provider         llm.Provider
	ModelBaseURL     string
	ModelAPIToken    string
	ModelName        string
	ModelAlias       string
	ContextWindow    int
	MaxOutputTokens  int
	SecretKeyring    *secrets.Keyring
	ExtraJudge       *config
	ExtraGeneration  *config
}

func main() {
	if err := run(); err != nil {
		slog.Error("Worker stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	configuration, err := loadConfig(os.LookupEnv)
	if err != nil {
		return err
	}
	database, err := openDatabase(configuration.PlatformMySQLDSN)
	if err != nil {
		return err
	}
	defer database.Close()
	startupContext, cancelStartup := context.WithTimeout(context.Background(), 10*time.Second)
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
		return err
	}
	queue, err := mysqlqueue.New(database)
	if err != nil {
		return err
	}
	adapter, err := modelAdapter(configuration, &http.Client{Transport: http.DefaultTransport})
	if err != nil {
		return err
	}
	callTimeout, callRetries, err := modelCallPolicy(configuration.Provider)
	if err != nil {
		return err
	}
	modelConfigs := []llm.ModelConfig{{
		Alias: configuration.ModelAlias,
		Model: llm.ModelRef{Provider: configuration.Provider, Model: configuration.ModelName},
		Capabilities: llm.ModelCapabilities{
			ToolCalling: true, NamedToolChoice: true, JSONSchemaOutput: true, Usage: true,
			ContextWindow: configuration.ContextWindow, MaxOutputTokens: configuration.MaxOutputTokens,
			TokenizerID: "conservative-v1",
		},
		Adapter: adapter, Timeout: callTimeout, MaxRetries: callRetries,
	}}
	if extra := configuration.ExtraJudge; extra != nil {
		extraAdapter, err := modelAdapter(*extra, &http.Client{Transport: http.DefaultTransport})
		if err != nil {
			return err
		}
		extraTimeout, extraRetries, err := modelCallPolicy(extra.Provider)
		if err != nil {
			return err
		}
		modelConfigs = append(modelConfigs, llm.ModelConfig{
			Alias: extra.ModelAlias,
			Model: llm.ModelRef{Provider: extra.Provider, Model: extra.ModelName},
			Capabilities: llm.ModelCapabilities{
				ToolCalling: true, NamedToolChoice: true, JSONSchemaOutput: true, Usage: true,
				ContextWindow: extra.ContextWindow, MaxOutputTokens: extra.MaxOutputTokens, TokenizerID: "conservative-v1",
			},
			Adapter: extraAdapter, Timeout: extraTimeout, MaxRetries: extraRetries,
		})
	}
	if extra := configuration.ExtraGeneration; extra != nil {
		extraAdapter, err := modelAdapter(*extra, &http.Client{Transport: http.DefaultTransport})
		if err != nil {
			return err
		}
		extraTimeout, extraRetries, err := modelCallPolicy(extra.Provider)
		if err != nil {
			return err
		}
		modelConfigs = append(modelConfigs, llm.ModelConfig{
			Alias: extra.ModelAlias,
			Model: llm.ModelRef{Provider: extra.Provider, Model: extra.ModelName},
			Capabilities: llm.ModelCapabilities{
				ToolCalling: true, NamedToolChoice: true, JSONSchemaOutput: true, Usage: true,
				ContextWindow: extra.ContextWindow, MaxOutputTokens: extra.MaxOutputTokens, TokenizerID: "conservative-v1",
			},
			Adapter: extraAdapter, Timeout: extraTimeout, MaxRetries: extraRetries,
		})
	}
	gateway, err := llm.NewGateway(modelConfigs)
	if err != nil {
		return fmt.Errorf("create Model Gateway: %w", err)
	}
	scenarioOutputTokens := min(configuration.MaxOutputTokens, 4096)
	datasetOutputTokens := min(configuration.MaxOutputTokens, 4096)
	testCaseOutputTokens := min(configuration.MaxOutputTokens, 4096)
	compactionPolicy := func(reservedOutputTokens int) llm.CompactionPolicy {
		return llm.CompactionPolicy{
			ContextWindow: configuration.ContextWindow, ReservedOutputTokens: reservedOutputTokens,
			SafetyMarginTokens: 1024, KeepRecentGroups: 1, MaxSummaryBytes: 1024,
			MaxCompactions: 4, MaxRetainedRecords: 4, MaxRollupBytes: 4_096, Conservative: true,
		}
	}
	secretStore, err := mysqlstore.NewSecretStore(database)
	if err != nil {
		return err
	}
	secretService, err := secrets.NewService(secretStore, configuration.SecretKeyring)
	if err != nil {
		return fmt.Errorf("create credential service: %w", err)
	}
	snapshotSource, err := snapshot.NewMySQLSource(secretService)
	if err != nil {
		return fmt.Errorf("configure validation MySQL snapshot source: %w", err)
	}
	targetStore, err := mysqlstore.NewTargetStore(database)
	if err != nil {
		return err
	}
	targetEndpoint, err := validationdb.EndpointFromDSN(configuration.PlatformMySQLDSN)
	if err != nil {
		return err
	}
	targetManager, err := validationdb.NewManager(targetStore, secretService, targetEndpoint)
	if err != nil {
		return fmt.Errorf("create validation MySQL manager: %w", err)
	}
	defer targetManager.Close()
	modelStore, err := mysqlstore.NewModelStore(database)
	if err != nil {
		return err
	}
	generationModels := []config{configuration}
	if configuration.ExtraGeneration != nil {
		generationModels = append(generationModels, *configuration.ExtraGeneration)
	}
	var generationAliases []string
	for _, model := range generationModels {
		if err := modelStore.Register(startupContext, modelcfg.RoleGeneration, model.ModelAlias, string(model.Provider), model.ModelName, model.ModelBaseURL); err != nil {
			return fmt.Errorf("register Generation model: %w", err)
		}
		generationAliases = append(generationAliases, model.ModelAlias)
	}
	for _, model := range append([]config{configuration}, extraJudges(configuration)...) {
		if err := modelStore.Register(startupContext, modelcfg.RoleJudge, model.ModelAlias, string(model.Provider), model.ModelName, model.ModelBaseURL); err != nil {
			return fmt.Errorf("register Judge model: %w", err)
		}
	}
	// Generation calls follow the model selected through PUT /api/v1/models/generation/active.
	generationGateway := newActiveModelGateway(gateway, modelStore, modelcfg.RoleGeneration, configuration.ModelAlias, generationAliases)
	processor, err := pipeline.New(store, validator,
		pipeline.WithScenarioGeneration(generationGateway, pipeline.ScenarioConfig{
			ModelAlias: configuration.ModelAlias, MaxOutputTokens: scenarioOutputTokens,
			CompactionPolicy: compactionPolicy(scenarioOutputTokens),
		}),
		pipeline.WithDatasetGeneration(generationGateway, pipeline.DatasetConfig{
			ModelAlias: configuration.ModelAlias, MaxOutputTokens: datasetOutputTokens,
			CompactionPolicy: compactionPolicy(datasetOutputTokens),
		}),
		pipeline.WithValidationDB(targetResolver{manager: targetManager}),
		pipeline.WithDatasetSnapshotSource(snapshotSource),
		pipeline.WithTestCaseGeneration(generationGateway, pipeline.TestCaseConfig{
			ModelAlias: configuration.ModelAlias, MaxOutputTokens: testCaseOutputTokens,
			CompactionPolicy: compactionPolicy(testCaseOutputTokens),
		}),
		// Reuses the Generation model/gateway under an independent Prompt Role, the minimum bar
		// docs/architecture.md 9.1 requires; point CRITIC_MODEL_ALIAS at a second registered
		// Model Gateway entry later to use a distinct model instead.
		pipeline.WithCriticReview(generationGateway, pipeline.CriticConfig{
			ModelAlias: configuration.ModelAlias, MaxOutputTokens: min(configuration.MaxOutputTokens, 2048),
			CompactionPolicy: compactionPolicy(min(configuration.MaxOutputTokens, 2048)),
		}),
	)
	if err != nil {
		return fmt.Errorf("create Generation pipeline: %w", err)
	}
	worker, err := generationworker.New(queue, store, processor, generationworker.Config{
		Owner: configuration.Owner, LeaseDuration: 60 * time.Second,
		HeartbeatInterval: 20 * time.Second, PollInterval: time.Second,
		StageTimeouts: generationworker.DefaultStageTimeouts(),
	})
	if err != nil {
		return fmt.Errorf("create Generation Worker: %w", err)
	}

	runStore, err := mysqlstore.NewRunStore(database)
	if err != nil {
		return err
	}
	toolCallStore, err := mysqlstore.NewToolCallStore(database)
	if err != nil {
		return err
	}
	executor, err := runexec.New(runexec.Deps{
		Secrets: secretService, ToolCalls: toolCallStore, Cancel: runStore,
		HTTP: &http.Client{}, Validator: validator,
	})
	if err != nil {
		return fmt.Errorf("create Run executor: %w", err)
	}
	runWorker, err := runworker.New(queue, runStore, executor, runworker.Config{
		Owner: configuration.Owner, LeaseDuration: 60 * time.Second,
		HeartbeatInterval: 20 * time.Second, PollInterval: time.Second, StageTimeout: 10 * time.Minute,
	})
	if err != nil {
		return fmt.Errorf("create Run Worker: %w", err)
	}
	sweeper, err := runexec.NewSweeper(runStore, secretService)
	if err != nil {
		return fmt.Errorf("create Run database sweeper: %w", err)
	}
	judges := map[string]evalworker.Evaluator{}
	for _, model := range append([]config{configuration}, extraJudges(configuration)...) {
		if err := modelStore.Register(startupContext, modelcfg.RoleJudge, model.ModelAlias, string(model.Provider), model.ModelName, model.ModelBaseURL); err != nil {
			return fmt.Errorf("register Judge model: %w", err)
		}
	}
	for _, model := range append([]config{configuration}, extraJudges(configuration)...) {
		tokens := min(model.MaxOutputTokens, 2048)
		judge, err := evaluation.NewJudge(gateway, validator, model.ModelAlias, tokens)
		if err != nil {
			return fmt.Errorf("create Judge %s: %w", model.ModelAlias, err)
		}
		evaluator, err := evalworker.NewJudgeEvaluator(judge, evalworker.JudgeModel{Alias: model.ModelAlias, Provider: string(model.Provider),
			Model: model.ModelName, BaseURL: model.ModelBaseURL, MaxTokens: tokens})
		if err != nil {
			return fmt.Errorf("create Judge evaluator %s: %w", model.ModelAlias, err)
		}
		judges[model.ModelAlias] = evaluator
	}
	judgeEvaluator, err := evalworker.NewRouter(configuration.ModelAlias, judges)
	if err != nil {
		return err
	}
	evaluationWorker, err := evalworker.New(queue, runStore, judgeEvaluator, evalworker.Config{
		Owner: configuration.Owner, LeaseDuration: 60 * time.Second,
		HeartbeatInterval: 20 * time.Second, PollInterval: time.Second, StageTimeout: 5 * time.Minute,
	})
	if err != nil {
		return fmt.Errorf("create Evaluation Worker: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("Workers started", "owner", configuration.Owner, "provider", configuration.Provider, "model", configuration.ModelName)
	errs := make(chan error, 4)
	var group sync.WaitGroup
	for _, loop := range []func(context.Context) error{worker.Run, runWorker.Run, evaluationWorker.Run, func(ctx context.Context) error { return sweeper.Run(ctx, 10*time.Minute) }} {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := loop(ctx); err != nil {
				errs <- err
				stop()
			}
		}()
	}
	group.Wait()
	close(errs)
	return <-errs
}

func extraJudges(configuration config) []config {
	if configuration.ExtraJudge == nil {
		return nil
	}
	return []config{*configuration.ExtraJudge}
}

func loadConfig(lookup func(string) (string, bool)) (config, error) {
	required := func(name string) (string, error) {
		value, found := lookup(name)
		if !found || strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("%s is required", name)
		}
		return strings.TrimSpace(value), nil
	}
	dsn, err := required("PLATFORM_MYSQL_DSN")
	if err != nil {
		return config{}, err
	}
	keyring, err := secrets.LoadKeyring(lookup)
	if err != nil {
		return config{}, err
	}
	baseURL, err := required("GENERATION_MODEL_BASE_URL")
	if err != nil {
		return config{}, err
	}
	model, err := required("GENERATION_MODEL_NAME")
	if err != nil {
		return config{}, err
	}
	providerValue := env(lookup, "GENERATION_MODEL_PROVIDER", "openai")
	provider := llm.Provider(providerValue)
	if provider != llm.ProviderOpenAI && provider != llm.ProviderOllama {
		return config{}, fmt.Errorf("GENERATION_MODEL_PROVIDER must be openai or ollama")
	}
	token := env(lookup, "GENERATION_MODEL_API_TOKEN", "")
	if provider == llm.ProviderOpenAI && token == "" {
		return config{}, fmt.Errorf("GENERATION_MODEL_API_TOKEN is required for OpenAI")
	}
	contextWindow, err := envInt(lookup, "GENERATION_CONTEXT_WINDOW", 32768)
	if err != nil || contextWindow < 4096 {
		return config{}, fmt.Errorf("GENERATION_CONTEXT_WINDOW must be an integer >= 4096")
	}
	maxOutputTokens, err := envInt(lookup, "GENERATION_MAX_OUTPUT_TOKENS", 4096)
	if err != nil || maxOutputTokens < 256 || maxOutputTokens >= contextWindow {
		return config{}, fmt.Errorf("GENERATION_MAX_OUTPUT_TOKENS must be in [256, context window)")
	}
	var extraGeneration *config
	if name := env(lookup, "GENERATION_ALT_MODEL_NAME", ""); name != "" {
		altProvider := llm.Provider(env(lookup, "GENERATION_ALT_MODEL_PROVIDER", string(provider)))
		if altProvider != llm.ProviderOpenAI && altProvider != llm.ProviderOllama {
			return config{}, fmt.Errorf("GENERATION_ALT_MODEL_PROVIDER must be openai or ollama")
		}
		altAlias := env(lookup, "GENERATION_ALT_MODEL_ALIAS", "generation-"+name)
		if altAlias == env(lookup, "GENERATION_MODEL_ALIAS", "generation-primary") {
			return config{}, fmt.Errorf("GENERATION_ALT_MODEL_ALIAS must differ from GENERATION_MODEL_ALIAS")
		}
		extraGeneration = &config{Provider: altProvider, ModelBaseURL: env(lookup, "GENERATION_ALT_MODEL_BASE_URL", baseURL),
			ModelAPIToken: env(lookup, "GENERATION_ALT_MODEL_API_TOKEN", token), ModelName: name, ModelAlias: altAlias,
			ContextWindow: contextWindow, MaxOutputTokens: maxOutputTokens}
	}
	var extra *config
	if name := env(lookup, "JUDGE_MODEL_NAME", ""); name != "" {
		extraProvider := llm.Provider(env(lookup, "JUDGE_MODEL_PROVIDER", string(provider)))
		if extraProvider != llm.ProviderOpenAI && extraProvider != llm.ProviderOllama {
			return config{}, fmt.Errorf("JUDGE_MODEL_PROVIDER must be openai or ollama")
		}
		extraAlias := env(lookup, "JUDGE_MODEL_ALIAS", "judge-"+name)
		if extraAlias == env(lookup, "GENERATION_MODEL_ALIAS", "generation-primary") {
			return config{}, fmt.Errorf("JUDGE_MODEL_ALIAS must differ from GENERATION_MODEL_ALIAS")
		}
		extra = &config{Provider: extraProvider, ModelBaseURL: env(lookup, "JUDGE_MODEL_BASE_URL", baseURL),
			ModelAPIToken: env(lookup, "JUDGE_MODEL_API_TOKEN", token), ModelName: name, ModelAlias: extraAlias,
			ContextWindow: contextWindow, MaxOutputTokens: maxOutputTokens}
	}
	return config{
		ExtraJudge:       extra,
		ExtraGeneration:  extraGeneration,
		PlatformMySQLDSN: dsn,
		Owner:            env(lookup, "WORKER_OWNER", hostname()),
		Provider:         provider, ModelBaseURL: baseURL, ModelAPIToken: token,
		ModelName: model, ModelAlias: env(lookup, "GENERATION_MODEL_ALIAS", "generation-primary"),
		ContextWindow: contextWindow, MaxOutputTokens: maxOutputTokens,
		SecretKeyring: keyring,
	}, nil
}

// targetResolver connects the pipeline to a registered validation MySQL through the Target manager.
type targetResolver struct {
	manager *validationdb.Manager
}

func (resolver targetResolver) ValidationDB(ctx context.Context, targetID string) (pipeline.ValidationDB, error) {
	connection, err := resolver.manager.Connection(ctx, targetID)
	if err != nil {
		return nil, err
	}
	return connection.Admin, nil
}

func modelAdapter(configuration config, client llm.HTTPDoer) (llm.Completer, error) {
	switch configuration.Provider {
	case llm.ProviderOpenAI:
		return llm.NewOpenAIAdapter(configuration.ModelBaseURL, configuration.ModelAPIToken, client)
	case llm.ProviderOllama:
		if configuration.ModelAPIToken != "" {
			return llm.NewOllamaAdapterWithToken(configuration.ModelBaseURL, configuration.ModelAPIToken, client)
		}
		return llm.NewOllamaAdapter(configuration.ModelBaseURL, client)
	default:
		return nil, fmt.Errorf("unsupported Generation Provider %q", configuration.Provider)
	}
}

// modelCallPolicy returns the per-call timeout and transient retry count of a Generation provider
// (docs/technical/02-llm-gateway.md, section 9).
func modelCallPolicy(provider llm.Provider) (time.Duration, int, error) {
	switch provider {
	case llm.ProviderOpenAI:
		return 60 * time.Second, 2, nil
	case llm.ProviderOllama:
		return 180 * time.Second, 1, nil
	default:
		return 0, 0, fmt.Errorf("unsupported Generation Provider %q", provider)
	}
}

func openDatabase(dataSourceName string) (*sql.DB, error) {
	driverConfig, err := mysqldriver.ParseDSN(dataSourceName)
	if err != nil || driverConfig.DBName == "" {
		return nil, fmt.Errorf("parse PLATFORM_MYSQL_DSN: %w", err)
	}
	driverConfig.ParseTime = true
	driverConfig.Loc = time.UTC
	driverConfig.Timeout = 5 * time.Second
	driverConfig.ReadTimeout = 10 * time.Second
	driverConfig.WriteTimeout = 10 * time.Second
	connector, err := mysqldriver.NewConnector(driverConfig)
	if err != nil {
		return nil, err
	}
	database := sql.OpenDB(connector)
	database.SetMaxOpenConns(10)
	database.SetMaxIdleConns(5)
	database.SetConnMaxLifetime(5 * time.Minute)
	return database, nil
}

func env(lookup func(string) (string, bool), name, fallback string) string {
	if value, found := lookup(name); found && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
}

func envInt(lookup func(string) (string, bool), name string, fallback int) (int, error) {
	value := env(lookup, name, "")
	if value == "" {
		return fallback, nil
	}
	return strconv.Atoi(value)
}

func hostname() string {
	value, err := os.Hostname()
	if err != nil || strings.TrimSpace(value) == "" {
		return "generation-worker"
	}
	return value
}
