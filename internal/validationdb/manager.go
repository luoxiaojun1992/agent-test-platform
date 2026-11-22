package validationdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-test-platform/internal/sandbox"

	"github.com/go-sql-driver/mysql"
)

const (
	adminDialTimeout  = 5 * time.Second
	adminQueryTimeout = 10 * time.Second
	adminMaxOpenConns = 5
	adminMaxIdleConns = 2
)

// Lookup loads a registered Target by ID. The MySQL store satisfies it.
type Lookup interface {
	FindByID(ctx context.Context, id string) (Target, error)
}

// Credentials resolves the administrator password of a Target together with the time the secret was last
// written. The secrets package satisfies it.
type Credentials interface {
	ResolveVersioned(ctx context.Context, name string) (string, time.Time, error)
}

// Connection is the administrator connection of one registered Target. Callers never close Admin.Admin:
// the Manager owns it.
type Connection struct {
	ID    string
	Name  string
	Admin sandbox.AdminConnection
}

type managedConnection struct {
	version    time.Time
	connection Connection
}

// Manager opens the administrator connection of a registered Target the first time it is needed, and
// reuses it until the secret that holds its password is replaced. Nothing is read from the environment: the
// endpoint, TLS settings, and CA certificate come from the Target record, and the password from the secret.
type Manager struct {
	lookup      Lookup
	credentials Credentials
	platform    Endpoint

	mu      sync.Mutex
	current map[string]managedConnection
	retired []*sql.DB
}

// NewManager returns a Manager. The platform endpoint is compared with every Target before connecting.
func NewManager(lookup Lookup, credentials Credentials, platform Endpoint) (*Manager, error) {
	if lookup == nil || credentials == nil {
		return nil, errors.New("target lookup and credentials are required")
	}
	if platform.Host == "" || platform.Port < 1 {
		return nil, errors.New("platform endpoint is required")
	}
	return &Manager{lookup: lookup, credentials: credentials, platform: platform, current: make(map[string]managedConnection)}, nil
}

// Connection returns the administrator connection of the Target with targetID. A Target that does not
// exist is ErrNotFound; one that points at Platform MySQL is ErrSameAsPlatform, a second check after the
// one made at registration. Errors name the Target and the secret, never the password.
func (manager *Manager) Connection(ctx context.Context, targetID string) (Connection, error) {
	record, err := manager.lookup.FindByID(ctx, targetID)
	if err != nil {
		return Connection{}, err
	}
	if strings.EqualFold(record.Host, manager.platform.Host) && record.Port == manager.platform.Port {
		return Connection{}, ErrSameAsPlatform
	}
	password, version, err := manager.credentials.ResolveVersioned(ctx, record.AdminPasswordSecretName)
	if err != nil {
		return Connection{}, fmt.Errorf("resolve the administrator password of Target %s: %w", record.Name, err)
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if existing, found := manager.current[record.ID]; found && existing.version.Equal(version) {
		return existing.connection, nil
	}
	connection, err := buildConnection(record, password)
	if err != nil {
		return Connection{}, fmt.Errorf("configure the connection of Target %s: %w", record.Name, err)
	}
	if existing, found := manager.current[record.ID]; found {
		// A stage may still be using the replaced pool, so it is closed with the Manager, not now.
		manager.retired = append(manager.retired, existing.connection.Admin.Admin)
	}
	manager.current[record.ID] = managedConnection{version: version, connection: connection}
	return connection, nil
}

// Close closes every pool the Manager opened.
func (manager *Manager) Close() error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	var closeErr error
	for id, managed := range manager.current {
		closeErr = errors.Join(closeErr, managed.connection.Admin.Admin.Close())
		delete(manager.current, id)
	}
	for _, database := range manager.retired {
		closeErr = errors.Join(closeErr, database.Close())
	}
	manager.retired = nil
	return closeErr
}

// EndpointFromDSN returns the TCP endpoint a Platform MySQL DSN addresses. Platform MySQL must be reachable over
// TCP, because a registered Target is refused when it names the same host and port.
func EndpointFromDSN(dataSourceName string) (Endpoint, error) {
	config, err := mysql.ParseDSN(dataSourceName)
	if err != nil {
		return Endpoint{}, fmt.Errorf("parse PLATFORM_MYSQL_DSN: %w", err)
	}
	if config.Net != "tcp" {
		return Endpoint{}, errors.New("PLATFORM_MYSQL_DSN must address Platform MySQL over TCP")
	}
	endpoint, err := EndpointFromAddr(config.Addr)
	if err != nil {
		return Endpoint{}, fmt.Errorf("PLATFORM_MYSQL_DSN must address a host and port: %w", err)
	}
	return endpoint, nil
}

func buildConnection(record Target, password string) (Connection, error) {
	config := mysql.NewConfig()
	config.User = record.AdminUsername
	config.Passwd = password
	config.Net = "tcp"
	config.Addr = net.JoinHostPort(record.Host, strconv.Itoa(record.Port))
	config.Timeout = adminDialTimeout
	config.ReadTimeout = adminQueryTimeout
	config.WriteTimeout = adminQueryTimeout
	if err := sandbox.ApplyTLS(config, record.TLSMode, record.Host, record.CACertificatePEM); err != nil {
		return Connection{}, err
	}
	connector, err := mysql.NewConnector(config)
	if err != nil {
		return Connection{}, err
	}
	database := sql.OpenDB(connector)
	database.SetMaxOpenConns(adminMaxOpenConns)
	database.SetMaxIdleConns(adminMaxIdleConns)
	database.SetConnMaxLifetime(5 * time.Minute)
	return Connection{
		ID: record.ID, Name: record.Name,
		Admin: sandbox.AdminConnection{Admin: database, Base: config},
	}, nil
}
