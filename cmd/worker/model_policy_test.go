package main

import (
	"testing"
	"time"

	"agent-test-platform/internal/llm"
)

func TestModelCallPolicyFollowsDesignPerProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		provider llm.Provider
		timeout  time.Duration
		retries  int
	}{
		{llm.ProviderOpenAI, 60 * time.Second, 2},
		{llm.ProviderOllama, 180 * time.Second, 1},
	}
	for _, test := range tests {
		timeout, retries, err := modelCallPolicy(test.provider)
		if err != nil || timeout != test.timeout || retries != test.retries {
			t.Fatalf("%s: timeout=%s retries=%d error=%v", test.provider, timeout, retries, err)
		}
	}
	if _, _, err := modelCallPolicy("unknown"); err == nil {
		t.Fatal("unknown provider was accepted")
	}
}
