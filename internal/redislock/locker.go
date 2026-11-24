package redislock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"agent-test-platform/internal/run"
)

// Locker is a Redis-backed per-Agent distributed lock (08 section 5).
type Locker struct {
	client *redis.Client
	ttl    time.Duration
}

// New returns a Locker. A non-positive ttl defaults to 5 minutes.
func New(client *redis.Client, ttl time.Duration) *Locker {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &Locker{client: client, ttl: ttl}
}

// LockAgent acquires a per-Agent lock with SET NX EX. It returns run.ErrAgentBusy when the Agent
// is already locked, and an unlock func on success.
func (l *Locker) LockAgent(ctx context.Context, agentID string) (func(), error) {
	key := "agent-run-lock:" + agentID
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	ok, err := l.client.SetNX(ctx, key, token, l.ttl).Result()
	if err != nil {
		return nil, fmt.Errorf("acquire agent lock: %w", err)
	}
	if !ok {
		return nil, run.ErrAgentBusy
	}
	unlock := func() {
		// Release only if we still own it.
		_ = l.client.Eval(ctx, `if redis.call("get", KEYS[1]) == ARGV[1] then return redis.call("del", KEYS[1]) else return 0 end`, []string{key}, token).Err()
	}
	return unlock, nil
}

func randomToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
