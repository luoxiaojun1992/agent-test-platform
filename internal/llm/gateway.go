package llm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

var ErrCircuitOpen = errors.New("model circuit is open")

type Completer interface {
	Complete(context.Context, CompletionRequest) (CompletionResponse, error)
}

type ModelCapabilities struct {
	ToolCalling       bool
	NamedToolChoice   bool
	ParallelToolCalls bool
	JSONSchemaOutput  bool
	Usage             bool
	ContextWindow     int
	MaxOutputTokens   int
	TokenizerID       string
}

type ModelConfig struct {
	Alias        string
	Model        ModelRef
	Capabilities ModelCapabilities
	Adapter      Completer
	Timeout      time.Duration
	MaxRetries   int
	Backoff      []time.Duration
}

type Sleeper func(context.Context, time.Duration) error
type Clock func() time.Time

type Gateway struct {
	models           map[string]ModelConfig
	now              Clock
	sleep            Sleeper
	failureThreshold int
	openDuration     time.Duration
	mu               sync.Mutex
	states           map[string]circuitState
}

type circuitState struct {
	consecutiveFailures int
	openUntil           time.Time
}

func NewGateway(configs []ModelConfig) (*Gateway, error) {
	return newGateway(configs, time.Now, sleepContext)
}

func newGateway(configs []ModelConfig, now Clock, sleep Sleeper) (*Gateway, error) {
	if len(configs) == 0 || now == nil || sleep == nil {
		return nil, fmt.Errorf("model configs, clock, and sleeper are required")
	}
	gateway := &Gateway{
		models: make(map[string]ModelConfig, len(configs)), now: now, sleep: sleep,
		failureThreshold: 5, openDuration: 30 * time.Second, states: make(map[string]circuitState),
	}
	for _, config := range configs {
		if config.Alias == "" || config.Model.Provider == "" || config.Model.Model == "" || config.Adapter == nil {
			return nil, fmt.Errorf("model alias, model ref, and adapter are required")
		}
		if _, exists := gateway.models[config.Alias]; exists {
			return nil, fmt.Errorf("duplicate model alias %q", config.Alias)
		}
		if config.Timeout <= 0 || config.MaxRetries < 0 || config.MaxRetries > 2 {
			return nil, fmt.Errorf("model %s has invalid timeout or retry count", config.Alias)
		}
		if config.Capabilities.ContextWindow < 1 || config.Capabilities.MaxOutputTokens < 1 {
			return nil, fmt.Errorf("model %s has invalid token limits", config.Alias)
		}
		gateway.models[config.Alias] = config
	}
	return gateway, nil
}

// Complete resolves a model alias once and never silently changes Provider or model.
func (gateway *Gateway) Complete(ctx context.Context, alias string, request CompletionRequest) (CompletionResponse, error) {
	config, ok := gateway.models[alias]
	if !ok {
		return CompletionResponse{}, fmt.Errorf("unknown model alias %q", alias)
	}
	if err := requireCapabilities(config.Capabilities, request); err != nil {
		return CompletionResponse{}, fmt.Errorf("model %s: %w", alias, err)
	}
	if gateway.circuitOpen(alias) {
		return CompletionResponse{}, fmt.Errorf("model %s: %w", alias, ErrCircuitOpen)
	}
	request.Model = config.Model
	if request.MaxOutputTokens > config.Capabilities.MaxOutputTokens {
		return CompletionResponse{}, fmt.Errorf("requested output tokens %d exceed model maximum %d", request.MaxOutputTokens, config.Capabilities.MaxOutputTokens)
	}

	var lastErr error
	for attempt := 0; attempt <= config.MaxRetries; attempt++ {
		callContext, cancel := context.WithTimeout(ctx, config.Timeout)
		response, err := config.Adapter.Complete(callContext, request)
		cancel()
		if err == nil {
			gateway.recordSuccess(alias)
			return response, nil
		}
		lastErr = err
		retryable := retryableError(err) || (errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil)
		if !retryable || attempt == config.MaxRetries {
			if retryable {
				gateway.recordTransientFailure(alias)
			} else {
				gateway.recordSuccess(alias)
			}
			break
		}
		gateway.recordTransientFailure(alias)
		if gateway.circuitOpen(alias) {
			break
		}
		backoff := retryBackoff(config.Backoff, attempt)
		if err := gateway.sleep(ctx, backoff); err != nil {
			return CompletionResponse{}, err
		}
	}
	return CompletionResponse{}, fmt.Errorf("model %s completion failed: %w", alias, lastErr)
}

func requireCapabilities(capabilities ModelCapabilities, request CompletionRequest) error {
	if len(request.Tools) > 0 && !capabilities.ToolCalling {
		return fmt.Errorf("tool calling is not supported")
	}
	if request.ToolChoice.Mode == ToolChoiceNamed && !capabilities.NamedToolChoice {
		return fmt.Errorf("named tool choice is not supported")
	}
	if len(request.ResponseSchema) > 0 && !capabilities.JSONSchemaOutput {
		return fmt.Errorf("JSON Schema output is not supported")
	}
	return nil
}

func retryableError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var httpError *HTTPError
	if errors.As(err, &httpError) {
		return httpError.StatusCode == http.StatusRequestTimeout || httpError.StatusCode == http.StatusTooManyRequests || httpError.StatusCode >= 500
	}
	var networkError net.Error
	return errors.As(err, &networkError)
}

func retryBackoff(configured []time.Duration, attempt int) time.Duration {
	if attempt < len(configured) {
		return configured[attempt]
	}
	if attempt == 0 {
		return 250 * time.Millisecond
	}
	return time.Second
}

func (gateway *Gateway) circuitOpen(alias string) bool {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	state := gateway.states[alias]
	if state.openUntil.IsZero() {
		return false
	}
	if !gateway.now().Before(state.openUntil) {
		delete(gateway.states, alias)
		return false
	}
	return true
}

func (gateway *Gateway) recordSuccess(alias string) {
	gateway.mu.Lock()
	delete(gateway.states, alias)
	gateway.mu.Unlock()
}

func (gateway *Gateway) recordTransientFailure(alias string) {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	state := gateway.states[alias]
	state.consecutiveFailures++
	if state.consecutiveFailures >= gateway.failureThreshold {
		state.openUntil = gateway.now().Add(gateway.openDuration)
	}
	gateway.states[alias] = state
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
