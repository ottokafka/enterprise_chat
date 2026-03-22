package main

import (
	"database/sql"
	"log"
	"os"

	clickhouse "github.com/ClickHouse/clickhouse-go/v2"
)

var (
	ClickhouseDB   *sql.DB
	ClickhouseConn clickhouse.Conn
)

func getClickhouseClient() clickhouse.Conn {
	return ClickhouseConn
}

func InitClickhouse() {
	dsn := os.Getenv("CLICKHOUSE_URL")
	if dsn == "" {
		dsn = "clickhouse://admin:admin@10.90.24.27:9000"
		log.Println("CLICKHOUSE_URL environment variable is not set, using fallback: " + dsn)
	}

	options, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		log.Fatal("Failed to parse Clickhouse DSN: ", err)
	}

	// For specialized ClickHouse features (like batching in document_ingestion.go)
	ClickhouseConn, err = clickhouse.Open(options)
	if err != nil {
		log.Printf("Failed to connect to Clickhouse (native): %v", err)
	}

	// For standard sql.DB interface
	ClickhouseDB = clickhouse.OpenDB(options)

	if err := ClickhouseDB.Ping(); err != nil {
		log.Printf("Failed to ping Clickhouse: %v", err)
	} else {
		log.Println("Successfully connected to Clickhouse")
	}
}

// ClickhouseQuery executes a query that returns rows, matching the JS helper signature.
func ClickhouseQuery(query string, args ...any) (*sql.Rows, error) {
	rows, err := ClickhouseDB.Query(query, args...)
	if err != nil {
		clickhouseErrorHandler(err)
		return nil, err
	}
	return rows, nil
}

// ClickhouseExec executes a query without returning rows (like INSERT/UPDATE/DELETE).
func ClickhouseExec(query string, args ...any) (sql.Result, error) {
	res, err := ClickhouseDB.Exec(query, args...)
	if err != nil {
		clickhouseErrorHandler(err)
		return nil, err
	}
	return res, nil
}

// clickhouseErrorHandler logs DB errors similar to the JS handle error function.
func clickhouseErrorHandler(err error) {
	log.Printf("DB error -> %v\n", err)
	if err.Error() == "dial tcp: i/o timeout" || err.Error() == "connection timed out" {
		log.Println("Most likely Your not connected to the internet or your VPN is not on. Worst case is the database is down.")
	}
}
