package sandbox

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// ServerEndpoint is a MySQL server reached with one account. The password is resolved by the caller from a secret.
type ServerEndpoint struct {
	Host             string
	Port             int
	Username         string
	Password         string
	TLSMode          string
	CACertificatePEM string
}

// OpenServer opens a bounded pool to the server without selecting a database.
func OpenServer(endpoint ServerEndpoint) (AdminConnection, error) {
	config := mysql.NewConfig()
	config.User = endpoint.Username
	config.Passwd = endpoint.Password
	config.Net = "tcp"
	config.Addr = net.JoinHostPort(endpoint.Host, strconv.Itoa(endpoint.Port))
	config.Timeout = 10 * time.Second
	config.ReadTimeout = 30 * time.Second
	config.WriteTimeout = 30 * time.Second
	if err := ApplyTLS(config, endpoint.TLSMode, endpoint.Host, endpoint.CACertificatePEM); err != nil {
		return AdminConnection{}, fmt.Errorf("database TLS: %w", err)
	}
	connector, err := mysql.NewConnector(config)
	if err != nil {
		return AdminConnection{}, fmt.Errorf("configure database: %w", err)
	}
	database := sql.OpenDB(connector)
	database.SetMaxOpenConns(4)
	database.SetConnMaxLifetime(5 * time.Minute)
	return AdminConnection{Admin: database, Base: config}, nil
}

// CheckRunDatabasePrivileges proves the account can do what a Run needs: create a database, create a table and write a row in it,
// and drop the database again. It uses a throwaway database that no Run can collide with, and always tries to drop it.
func CheckRunDatabasePrivileges(ctx context.Context, connection AdminConnection, probeID string) error {
	name := "atp_probe_" + strings.ToLower(probeID)
	if !probeName.MatchString(name) {
		return fmt.Errorf("unsafe probe database name %q", name)
	}
	quoted := "`" + name + "`"
	if err := connection.Admin.PingContext(ctx); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	if _, err := connection.ExecContext(ctx, "CREATE DATABASE "+quoted); err != nil {
		return fmt.Errorf("the account cannot create databases: %w", err)
	}
	defer func() {
		dropCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = connection.ExecContext(dropCtx, "DROP DATABASE IF EXISTS "+quoted)
	}()
	database, err := connection.OpenDatabase(ctx, name)
	if err != nil {
		return fmt.Errorf("open probe database: %w", err)
	}
	defer database.Close()
	for _, statement := range []string{"CREATE TABLE probe (id INT PRIMARY KEY)", "INSERT INTO probe (id) VALUES (1)"} {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("the account cannot write test data: %w", err)
		}
	}
	return nil
}

var probeName = regexp.MustCompile(`^atp_probe_[a-z0-9]{1,40}$`)

// ListRunDatabases returns the names of databases that look like Run databases (run_<26 chars>) on the server.
func ListRunDatabases(ctx context.Context, connection AdminConnection) ([]string, error) {
	rows, err := connection.Admin.QueryContext(ctx, "SELECT schema_name FROM information_schema.schemata WHERE schema_name LIKE 'run\\_%'")
	if err != nil {
		return nil, fmt.Errorf("list run databases: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if runDatabaseName.MatchString(name) {
			names = append(names, name)
		}
	}
	return names, rows.Err()
}

var runDatabaseName = regexp.MustCompile(`^run_[0-9a-z]{26}$`)

// DropDatabase drops one database by an already validated Run database name.
func DropDatabase(ctx context.Context, connection AdminConnection, name string) error {
	if !runDatabaseName.MatchString(name) {
		return fmt.Errorf("refusing to drop %q: not a Run database name", name)
	}
	_, err := connection.ExecContext(ctx, "DROP DATABASE IF EXISTS `"+name+"`")
	return err
}
