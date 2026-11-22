package main

import (
	"encoding/base64"
	"strings"
	"testing"

	"agent-test-platform/internal/llm"
)

// secretKeyVariable holds the key with id k1 in worker configuration tests.
const secretKeyVariable = "ATP_ENCRYPTION_KEY_K1"

// withSecretKey adds the keyring variables every worker configuration requires.
func withSecretKey(values map[string]string) map[string]string {
	values["SECRET_ENCRYPTION_KEY_ID"] = "k1"
	values["SECRET_ENCRYPTION_KEY_IDS"] = "k1"
	values[secretKeyVariable] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	return values
}

func TestLoadConfigSeparatesProviderToken(t *testing.T) {
	t.Parallel()

	values := withSecretKey(map[string]string{
		"PLATFORM_MYSQL_DSN":         "platform:secret@tcp(mysql:3306)/platform",
		"GENERATION_MODEL_BASE_URL":  "http://mock-llm:8081/v1",
		"GENERATION_MODEL_NAME":      "mock-model",
		"GENERATION_MODEL_API_TOKEN": "llm-token",
	})
	configuration, err := loadConfig(func(name string) (string, bool) { value, found := values[name]; return value, found })
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Provider != llm.ProviderOpenAI || configuration.ModelAPIToken != "llm-token" {
		t.Fatalf("configuration = %#v", configuration)
	}
}

func TestLoadConfigAllowsLocalOllamaWithoutToken(t *testing.T) {
	t.Parallel()

	values := withSecretKey(map[string]string{
		"PLATFORM_MYSQL_DSN":        "platform:secret@tcp(mysql:3306)/platform",
		"GENERATION_MODEL_BASE_URL": "http://ollama:11434/v1",
		"GENERATION_MODEL_NAME":     "qwen",
		"GENERATION_MODEL_PROVIDER": "ollama",
	})
	configuration, err := loadConfig(func(name string) (string, bool) { value, found := values[name]; return value, found })
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Provider != llm.ProviderOllama || configuration.ModelAPIToken != "" {
		t.Fatalf("configuration = %#v", configuration)
	}
}

// validation MySQL instances are registered through the API and read from Platform MySQL, so the worker needs no
// Target setting in its environment.
func TestLoadConfigNeedsNoTargetEnvironment(t *testing.T) {
	t.Parallel()

	values := withSecretKey(map[string]string{
		"PLATFORM_MYSQL_DSN":         "platform:secret@tcp(mysql:3306)/platform",
		"GENERATION_MODEL_BASE_URL":  "http://mock-llm:8081/v1",
		"GENERATION_MODEL_NAME":      "mock-model",
		"GENERATION_MODEL_API_TOKEN": "llm-token",
	})
	if _, err := loadConfig(func(name string) (string, bool) { value, found := values[name]; return value, found }); err != nil {
		t.Fatalf("worker configuration requires a Target setting: %v", err)
	}
}
