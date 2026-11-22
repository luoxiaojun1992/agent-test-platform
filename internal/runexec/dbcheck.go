package runexec

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agent-test-platform/internal/run"
	"agent-test-platform/internal/sandbox"

	"github.com/oklog/ulid/v2"
)

// ProfileLoader reads the connection settings of an Agent database profile.
type ProfileLoader interface {
	LoadAgentDatabase(ctx context.Context, profileID string) (run.AgentDatabase, error)
}

// DatabaseChecker verifies an Agent database by running the same operations a Run performs, in a throwaway database.
type DatabaseChecker struct {
	profiles ProfileLoader
	secrets  Secrets
}

// NewDatabaseChecker returns a DatabaseChecker.
func NewDatabaseChecker(profiles ProfileLoader, secrets Secrets) (*DatabaseChecker, error) {
	if profiles == nil || secrets == nil {
		return nil, errors.New("database checker needs a profile loader and secrets")
	}
	return &DatabaseChecker{profiles: profiles, secrets: secrets}, nil
}

// CheckDatabase connects with the stored credentials, creates and writes a probe database, and drops it.
func (checker *DatabaseChecker) CheckDatabase(ctx context.Context, profileID string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	profile, err := checker.profiles.LoadAgentDatabase(ctx, profileID)
	if err != nil {
		return err
	}
	password, err := checker.secrets.Resolve(ctx, profile.PasswordSecretRef)
	if err != nil {
		return fmt.Errorf("resolve the database password: %w", err)
	}
	connection, err := sandbox.OpenServer(sandbox.ServerEndpoint{Host: profile.Host, Port: profile.Port, Username: profile.Username,
		Password: password, TLSMode: profile.TLSMode, CACertificatePEM: profile.CACertificatePEM})
	if err != nil {
		return err
	}
	defer connection.Admin.Close()
	return sandbox.CheckRunDatabasePrivileges(ctx, connection, ulid.Make().String())
}
