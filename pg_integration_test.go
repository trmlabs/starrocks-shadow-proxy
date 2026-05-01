//go:build integration

package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestPgProxyAlloyDBOmniSmoke(t *testing.T) {
	dsn := os.Getenv("PG_PROXY_DSN")
	if dsn == "" {
		t.Fatal("PG_PROXY_DSN is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect through proxy: %v", err)
	}
	defer conn.Close(ctx)

	var one int
	if err := conn.QueryRow(ctx, "select $1::int + 1", 41).Scan(&one); err != nil {
		t.Fatalf("extended query through proxy: %v", err)
	}
	if one != 42 {
		t.Fatalf("extended query result = %d, want 42", one)
	}

	batch := &pgx.Batch{}
	batch.Queue("select $1::text", "alpha")
	batch.Queue("select $1::int * 2", 21)
	results := conn.SendBatch(ctx, batch)
	defer results.Close()

	var textResult string
	if err := results.QueryRow().Scan(&textResult); err != nil {
		t.Fatalf("batch query 1 through proxy: %v", err)
	}
	if textResult != "alpha" {
		t.Fatalf("batch query 1 = %q, want alpha", textResult)
	}

	var intResult int
	if err := results.QueryRow().Scan(&intResult); err != nil {
		t.Fatalf("batch query 2 through proxy: %v", err)
	}
	if intResult != 42 {
		t.Fatalf("batch query 2 = %d, want 42", intResult)
	}
}
