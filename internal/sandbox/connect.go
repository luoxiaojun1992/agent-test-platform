package sandbox

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// TLS modes shared by validation MySQL registrations and Database Profiles.
const (
	// TLSDisabledLocalOnly sends no TLS and is limited to isolated local networks (docs/technical/06 section 4).
	TLSDisabledLocalOnly = "DISABLED_LOCAL_ONLY"
	// TLSVerifyIdentity verifies the server certificate and host name.
	TLSVerifyIdentity = "VERIFY_IDENTITY"
)

// ErrTLSConfig reports an unsupported TLS mode or a CA certificate that holds no certificates.
var ErrTLSConfig = errors.New("TLS settings are invalid")

// ApplyTLS sets the transport security of a MySQL client connection. VERIFY_IDENTITY verifies the server
// against caPEM when it is given and against the system roots otherwise. caPEM is public material that the
// platform keeps in Platform MySQL; it is never a credential.
func ApplyTLS(config *mysql.Config, mode, host, caPEM string) error {
	switch mode {
	case TLSVerifyIdentity:
		tlsConfig := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
		if strings.TrimSpace(caPEM) != "" {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM([]byte(caPEM)) {
				return fmt.Errorf("%w: the CA certificate holds no certificates", ErrTLSConfig)
			}
			tlsConfig.RootCAs = pool
		}
		config.TLS = tlsConfig
	case TLSDisabledLocalOnly:
		config.TLSConfig = "false"
	default:
		return fmt.Errorf("%w: unsupported tls_mode %q", ErrTLSConfig, mode)
	}
	return nil
}

// AdminConnection adapts the administrator account of one registered validation MySQL (architecture 16.5)
// into the shape the pipeline needs: ExecContext to create databases, and OpenDatabase to load and read
// them. The platform uses no other validation MySQL account and creates none.
type AdminConnection struct {
	Admin *sql.DB
	Base  *mysql.Config
}

func (connection AdminConnection) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return connection.Admin.ExecContext(ctx, query, args...)
}

// OpenDatabase opens a connection to one database as the registered administrator. The caller closes
// the returned handle.
func (connection AdminConnection) OpenDatabase(_ context.Context, databaseName string) (*sql.DB, error) {
	config := connection.Base.Clone()
	config.DBName = databaseName
	config.ParseTime = false
	connector, err := mysql.NewConnector(config)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(5 * time.Minute)
	return db, nil
}
