package runexec

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"agent-test-platform/internal/run"
	"agent-test-platform/internal/sandbox"
)

// SweepStore reads what the sweeper needs from Platform MySQL.
type SweepStore interface {
	// AgentDatabases returns every distinct database server configured on an Agent.
	AgentDatabases(ctx context.Context) ([]run.AgentDatabase, error)
	// RunStatuses returns the status of each known Run ID; unknown IDs are absent.
	RunStatuses(ctx context.Context, runIDs []string) (map[string]string, error)
}

// Sweeper drops Run databases that a crashed Worker left on Agent database servers. A database is dropped only when the
// platform knows its Run and that Run is no longer being prepared or executed, so databases of other tools and of Runs in
// flight are never touched.
type Sweeper struct {
	store   SweepStore
	secrets Secrets
	now     func() time.Time
}

// NewSweeper returns a Sweeper.
func NewSweeper(store SweepStore, secrets Secrets) (*Sweeper, error) {
	if store == nil || secrets == nil {
		return nil, errors.New("sweeper store and secrets are required")
	}
	return &Sweeper{store: store, secrets: secrets, now: time.Now}, nil
}

var activeStatuses = map[string]bool{"QUEUED": true, run.StatusPreparing: true, run.StatusRunning: true}

// SweepOnce visits every Agent database server once and returns how many databases it dropped. A server that cannot be reached
// is skipped and reported in the error, so one broken Agent does not stop the others.
func (sweeper *Sweeper) SweepOnce(ctx context.Context) (int, error) {
	servers, err := sweeper.store.AgentDatabases(ctx)
	if err != nil {
		return 0, err
	}
	dropped := 0
	var failures []error
	for _, server := range servers {
		count, err := sweeper.sweepServer(ctx, server)
		dropped += count
		if err != nil {
			failures = append(failures, err)
		}
	}
	return dropped, errors.Join(failures...)
}

func (sweeper *Sweeper) sweepServer(ctx context.Context, server run.AgentDatabase) (int, error) {
	password, err := sweeper.secrets.Resolve(ctx, server.PasswordSecretRef)
	if err != nil {
		return 0, err
	}
	connection, err := sandbox.OpenServer(sandbox.ServerEndpoint{Host: server.Host, Port: server.Port, Username: server.Username,
		Password: password, TLSMode: server.TLSMode, CACertificatePEM: server.CACertificatePEM})
	if err != nil {
		return 0, err
	}
	defer connection.Admin.Close()
	names, err := sandbox.ListRunDatabases(ctx, connection)
	if err != nil || len(names) == 0 {
		return 0, err
	}
	ids := make([]string, len(names))
	for index, name := range names {
		ids[index] = strings.ToUpper(strings.TrimPrefix(name, "run_"))
	}
	statuses, err := sweeper.store.RunStatuses(ctx, ids)
	if err != nil {
		return 0, err
	}
	dropped := 0
	for index, name := range names {
		status, known := statuses[ids[index]]
		if !known || activeStatuses[status] {
			continue
		}
		if err := sandbox.DropDatabase(ctx, connection, name); err != nil {
			slog.Warn("Could not drop a leftover Run database", "database", name, "error", err)
			continue
		}
		dropped++
	}
	return dropped, nil
}

// Run sweeps every interval until ctx ends.
func (sweeper *Sweeper) Run(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			dropped, err := sweeper.SweepOnce(ctx)
			if err != nil {
				slog.Warn("Run database sweep finished with errors", "dropped", dropped, "error", err)
			} else if dropped > 0 {
				slog.Info("Dropped leftover Run databases", "dropped", dropped)
			}
		}
	}
}
