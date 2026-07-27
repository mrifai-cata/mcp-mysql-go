// Smallest thing that fails if the read-only guard or the row cap breaks.
// Needs a live MySQL + the same env as the server:
//
//	set -a; . ./.env; set +a; DBROWLIMIT=2 go test ./...
package main

import (
	"context"
	"os"
	"testing"
)

func TestReadOnlyQuery(t *testing.T) {
	if os.Getenv("DBNAME") == "" {
		t.Skip("no DBNAME in env — needs a live MySQL")
	}
	os.Setenv("DBROWLIMIT", "2") // pin the cap so three rows must come back capped
	if err := setup(); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	res, err := readOnlyQuery(ctx, "SELECT 1 AS one")
	if err != nil || res.Rows[0]["one"] != int64(1) {
		t.Fatalf("basic SELECT should work: %v %v", res, err)
	}

	big, err := readOnlyQuery(ctx, "SELECT n FROM (SELECT 1 n UNION SELECT 2 UNION SELECT 3) t")
	if err != nil {
		t.Fatal(err)
	}
	if len(big.Rows) != 2 || !big.Truncated {
		t.Fatalf("row cap should truncate to DBROWLIMIT and flag it: %+v", big)
	}

	if _, err := readOnlyQuery(ctx, "CREATE TABLE ponytail_should_not_exist (id INT)"); err == nil {
		t.Fatal("DDL must be rejected by the read-only transaction")
	}
	if _, err := readOnlyQuery(ctx, "SELECT 1; SELECT 2"); err == nil {
		t.Fatal("stacked statements must be rejected")
	}
}
