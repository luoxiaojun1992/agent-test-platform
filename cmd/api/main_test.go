package main

import (
	"testing"
	"time"
)

func TestPlatformMySQLConfig(t *testing.T) {
	t.Parallel()

	configuration, err := platformMySQLConfig("platform:secret@tcp(127.0.0.1:3306)/agent_test_platform")
	if err != nil {
		t.Fatal(err)
	}
	if !configuration.ParseTime || configuration.Loc != time.UTC || configuration.DBName != "agent_test_platform" {
		t.Fatalf("configuration = %#v", configuration)
	}
	if configuration.Timeout == 0 || configuration.ReadTimeout == 0 || configuration.WriteTimeout == 0 {
		t.Fatal("database timeouts were not applied")
	}
}

func TestPlatformMySQLConfigRequiresDatabase(t *testing.T) {
	t.Parallel()

	if _, err := platformMySQLConfig("platform:secret@tcp(127.0.0.1:3306)/"); err == nil {
		t.Fatal("DSN without a database was accepted")
	}
}
