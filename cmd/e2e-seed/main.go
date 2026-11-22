// Command e2e-seed prepares a throwaway Platform MySQL database for the browser end-to-end tests.
//
//	e2e-seed init  creates an empty database and applies migrations. Every asset is generated through the UI.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"

	"agent-test-platform/migrations"

	"github.com/go-sql-driver/mysql"
)

func main() {
	if len(os.Args) != 2 || os.Args[1] != "init" {
		log.Fatal("usage: e2e-seed init")
	}
	dsn := os.Getenv("E2E_PLATFORM_DSN")
	name := os.Getenv("E2E_PLATFORM_DB")
	if dsn == "" || name == "" {
		log.Fatal("E2E_PLATFORM_DSN (without database) and E2E_PLATFORM_DB are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		log.Fatal(err)
	}
	config.ParseTime, config.Loc = true, time.UTC
	{
		config.DBName = ""
		admin := open(config)
		defer admin.Close()
		must(admin.ExecContext(ctx, "DROP DATABASE IF EXISTS `"+name+"`"))
		must(admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"))
		config.DBName = name
		database := open(config)
		defer database.Close()
		if err := migrations.Apply(ctx, database); err != nil {
			log.Fatal(err)
		}
		fmt.Println("created empty platform database", name)
		return
	}
}

func open(config *mysql.Config) *sql.DB {
	connector, err := mysql.NewConnector(config)
	if err != nil {
		log.Fatal(err)
	}
	return sql.OpenDB(connector)
}

func must(result sql.Result, err error) sql.Result {
	if err != nil {
		log.Fatal(err)
	}
	return result
}
