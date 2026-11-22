package main

import (
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"agent-test-platform/internal/mockllm"
)

func main() {
	address := envOrDefault("HTTP_ADDR", ":8081")
	token := requiredEnv("MOCK_LLM_BEARER_TOKEN")
	handler, err := mockllm.NewServer(token)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	log.Printf("Mock LLM listening on %s", address)
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
