package main

import (
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"agent-test-platform/contracts"
	"agent-test-platform/internal/mocktarget"
)

func main() {
	validator, err := contracts.NewValidator()
	if err != nil {
		log.Fatal(err)
	}
	handler, err := mocktarget.NewServer(mocktarget.Config{
		BearerToken: requiredEnv("MOCK_TARGET_BEARER_TOKEN"),
		ModelID:     envOrDefault("MOCK_TARGET_MODEL", "mock-agent-v1"),
		IndexDelay:  durationOrDefault("MOCK_TARGET_INDEX_DELAY", 2*time.Second),
	}, validator)
	if err != nil {
		log.Fatal(err)
	}
	address := envOrDefault("HTTP_ADDR", ":8082")
	server := &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 5 * time.Minute, IdleTimeout: 60 * time.Second}
	log.Printf("Mock Target listening on %s", address)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func requiredEnv(name string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		log.Fatalf("%s is required", name)
	}
	return value
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func durationOrDefault(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		log.Fatalf("invalid %s: %v", name, err)
	}
	return parsed
}
