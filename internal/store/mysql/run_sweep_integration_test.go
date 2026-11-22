package mysqlstore_test

import (
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-test-platform/internal/runexec"
	"agent-test-platform/internal/sandbox"
	mysqlstore "agent-test-platform/internal/store/mysql"
	"agent-test-platform/migrations"

	"github.com/go-sql-driver/mysql"
)

type sweepSecrets struct{ password string }

func (s sweepSecrets) Resolve(context.Context, string) (string, error) { return s.password, nil }

// TestAgentDatabaseCheckAndSweepOnMySQL84 uses a real server as the Agent's database: the privilege probe must pass for an
// administrator and fail for a read-only account, and the sweeper must drop only leftover databases of finished Runs.
func TestAgentDatabaseCheckAndSweepOnMySQL84(t *testing.T) {
	dsn := os.Getenv("ATP_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set ATP_TEST_MYSQL_DSN to run the Agent database integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	adminConfig, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	adminConfig.DBName, adminConfig.ParseTime, adminConfig.Loc = "", true, time.UTC
	admin := open(t, adminConfig)
	defer admin.Close()
	host, portText, _ := net.SplitHostPort(adminConfig.Addr)
	port, _ := strconv.Atoi(portText)

	platformName := strings.NewReplacer(".", "_").Replace("atp_sweep_" + time.Now().Format("150405.000000"))
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+platformName+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatal(err)
	}
	platformConfig := *adminConfig
	platformConfig.DBName = platformName
	platformConfig.Params = map[string]string{"foreign_key_checks": "0"}
	platform := open(t, &platformConfig)
	defer platform.Close()
	if err := migrations.Apply(ctx, platform); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.ExecContext(ctx, `INSERT INTO database_profile (id, name, engine, host, port, database_name, username, password_secret_ref, tls_mode, snapshot_mode, config_json, config_sha256, idempotency_key)
		VALUES ('01HZZZZZZZZZZZZZZZZZZZZZP1', 'agent-db', 'MYSQL', ?, ?, 'agent_db', ?, 'pw', 'DISABLED_LOCAL_ONLY', 'SCHEMA_ONLY', JSON_OBJECT(), REPEAT('f', 64), 'k1')`, host, port, adminConfig.User); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.ExecContext(ctx, `INSERT INTO agent (id, name, database_profile_id, config_sha256, idempotency_key) VALUES ('AG', 'a', '01HZZZZZZZZZZZZZZZZZZZZZP1', REPEAT('e', 64), 'ak')`); err != nil {
		t.Fatal(err)
	}
	store, err := mysqlstore.NewRunStore(platform)
	if err != nil {
		t.Fatal(err)
	}
	secrets := sweepSecrets{password: adminConfig.Passwd}

	checker, err := runexec.NewDatabaseChecker(store, secrets)
	if err != nil {
		t.Fatal(err)
	}
	if err := checker.CheckDatabase(ctx, "01HZZZZZZZZZZZZZZZZZZZZZP1"); err != nil {
		t.Fatalf("an administrator must pass the check: %v", err)
	}
	var leftover int
	_ = admin.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name LIKE 'atp\\_probe\\_%'").Scan(&leftover)
	if leftover != 0 {
		t.Fatalf("the probe database must be dropped, found %d", leftover)
	}

	readOnly := "atp_ro_" + strconv.FormatInt(time.Now().UnixNano()%1_000_000_000, 10)
	for _, statement := range []string{"CREATE USER '" + readOnly + "'@'%' IDENTIFIED BY 'ro-pass'", "GRANT SELECT ON *.* TO '" + readOnly + "'@'%'"} {
		if _, err := admin.ExecContext(ctx, statement); err != nil {
			t.Skipf("cannot create a limited account on this server: %v", err)
		}
	}
	defer admin.ExecContext(context.Background(), "DROP USER '"+readOnly+"'@'%'")
	limited, err := sandbox.OpenServer(sandbox.ServerEndpoint{Host: host, Port: port, Username: readOnly, Password: "ro-pass", TLSMode: "DISABLED_LOCAL_ONLY"})
	if err != nil {
		t.Fatal(err)
	}
	defer limited.Admin.Close()
	if err := sandbox.CheckRunDatabasePrivileges(ctx, limited, "01HZZZZZZZZZZZZZZZZZZZZZP9"); err == nil || !strings.Contains(err.Error(), "cannot create databases") {
		t.Fatalf("a read-only account must be rejected with a clear reason: %v", err)
	}

	finished, active, unknown := "01HZZZZZZZZZZZZZZZZZZZZZR1", "01HZZZZZZZZZZZZZZZZZZZZZR2", "01HZZZZZZZZZZZZZZZZZZZZZR3"
	for _, row := range [][2]string{{finished, "COMPLETED"}, {active, "RUNNING"}} {
		if _, err := platform.ExecContext(ctx, `INSERT INTO test_run (id, idempotency_key, test_case_revision_id, dataset_revision_id, agent_version_id, status, input_text) VALUES (?, ?, 'T', 'D', 'V', ?, 'x')`, row[0], "key-"+row[0], row[1]); err != nil {
			t.Fatal(err)
		}
	}
	names := map[string]string{finished: "run_" + strings.ToLower(finished), active: "run_" + strings.ToLower(active), unknown: "run_" + strings.ToLower(unknown)}
	for _, name := range names {
		if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+name+"`"); err != nil {
			t.Fatal(err)
		}
		defer admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS `"+name+"`")
	}
	sweeper, err := runexec.NewSweeper(store, secrets)
	if err != nil {
		t.Fatal(err)
	}
	dropped, err := sweeper.SweepOnce(ctx)
	if err != nil || dropped != 1 {
		t.Fatalf("dropped=%d err=%v, want exactly the finished Run's database", dropped, err)
	}
	exists := func(name string) bool {
		var n int
		_ = admin.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name = ?", name).Scan(&n)
		return n == 1
	}
	if exists(names[finished]) || !exists(names[active]) || !exists(names[unknown]) {
		t.Fatalf("finished=%v active=%v unknown=%v: only the finished Run's database may be dropped", exists(names[finished]), exists(names[active]), exists(names[unknown]))
	}
}
