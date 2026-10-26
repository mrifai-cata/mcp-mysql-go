// Read-only MySQL MCP server — Go port of ../mcp-mysql-nodejs/server.js.
// Same three tools, same env vars, same guards.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var (
	db         *sql.DB
	dbName     string
	rowLimit   int
	timeoutMS  int
	allowWrite bool
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// setup reads env and opens the pool. Separate from main so the test can call it.
func setup() error {
	user := os.Getenv("DBUSER")
	dbName = os.Getenv("DBNAME")
	if user == "" || dbName == "" {
		return fmt.Errorf("missing env: DBUSER and DBNAME are required")
	}

	var err error
	if rowLimit, err = strconv.Atoi(env("DBROWLIMIT", "200")); err != nil || rowLimit < 1 {
		return fmt.Errorf("DBROWLIMIT must be a positive integer, got %q", os.Getenv("DBROWLIMIT"))
	}
	if timeoutMS, err = strconv.Atoi(env("DBTIMEOUTMS", "5000")); err != nil || timeoutMS < 1 {
		return fmt.Errorf("DBTIMEOUTMS must be a positive integer, got %q", os.Getenv("DBTIMEOUTMS"))
	}

	allowWrite = os.Getenv("DBALLOWWRITE") == "true"

	cfg := mysql.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(env("DBHOST", "127.0.0.1"), env("DBPORT", "3306"))
	cfg.User = user
	cfg.Passwd = os.Getenv("DBPSWD")
	cfg.DBName = dbName
	// MultiStatements stays false (driver default): one statement per call,
	// so `; DROP ...` cannot ride along on a SELECT.

	if db, err = sql.Open("mysql", cfg.FormatDSN()); err != nil {
		return err
	}
	db.SetMaxOpenConns(2)
	return nil
}

type result struct {
	Rows      []map[string]any `json:"rows"`
	Truncated bool             `json:"truncated"`
}

// Read-only is enforced in three places on purpose: DB grants (see README),
// a READ ONLY transaction, and a statement timeout. The grants are the real
// defense — the rest just fails faster and with a clearer message.
func readOnlyQuery(ctx context.Context, query string, args ...any) (*result, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMS)*time.Millisecond)
	defer cancel()

	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // nothing to roll back; the conn goes home either way

	// Server-side twin of the ctx deadline, so MySQL kills the statement itself.
	// timeoutMS is an int parsed in setup(), so this Sprintf can't inject.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET SESSION MAX_EXECUTION_TIME=%d", timeoutMS)); err != nil {
		return nil, err
	}

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	out := &result{Rows: []map[string]any{}}
	for rows.Next() {
		if len(out.Rows) == rowLimit {
			out.Truncated = true
			break
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			// ponytail: []byte covers every text/blob/date column the driver
			// hands back untyped. Add per-type conversion when a caller cares.
			if b, ok := vals[i].([]byte); ok {
				row[c] = string(b)
			} else {
				row[c] = vals[i]
			}
		}
		out.Rows = append(out.Rows, row)
	}
	return out, rows.Err()
}

func asText(v any) *mcp.CallToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}
}

func listTables(ctx context.Context, _ *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
	res, err := readOnlyQuery(ctx,
		`SELECT table_name, table_rows, table_comment
		   FROM information_schema.tables
		  WHERE table_schema = ?
		  ORDER BY table_name`, dbName)
	if err != nil {
		return nil, nil, err
	}
	return asText(res.Rows), nil, nil
}

type describeArgs struct {
	Table string `json:"table" jsonschema:"table name"`
}

// information_schema instead of SHOW FULL COLUMNS FROM <table>: the table name
// becomes a ? placeholder, so there is no identifier to escape by hand.
func describeTable(ctx context.Context, _ *mcp.CallToolRequest, in describeArgs) (*mcp.CallToolResult, any, error) {
	columns, err := readOnlyQuery(ctx,
		`SELECT column_name, column_type, is_nullable, column_key, column_default, extra, column_comment
		   FROM information_schema.columns
		  WHERE table_schema = ? AND table_name = ?
		  ORDER BY ordinal_position`, dbName, in.Table)
	if err != nil {
		return nil, nil, err
	}
	indexes, err := readOnlyQuery(ctx,
		`SELECT index_name, seq_in_index, column_name, non_unique, index_type
		   FROM information_schema.statistics
		  WHERE table_schema = ? AND table_name = ?
		  ORDER BY index_name, seq_in_index`, dbName, in.Table)
	if err != nil {
		return nil, nil, err
	}
	return asText(map[string]any{"columns": columns.Rows, "indexes": indexes.Rows}), nil, nil
}

type queryArgs struct {
	SQL    string `json:"sql" jsonschema:"a single SELECT/SHOW/EXPLAIN statement, ? for placeholders"`
	Params []any  `json:"params,omitempty" jsonschema:"values for the ? placeholders"`
}

func runQuery(ctx context.Context, _ *mcp.CallToolRequest, in queryArgs) (*mcp.CallToolResult, any, error) {
	res, err := readOnlyQuery(ctx, in.SQL, in.Params...)
	if err != nil {
		return nil, nil, err
	}
	return asText(map[string]any{
		"rowCount":  len(res.Rows),
		"truncated": res.Truncated,
		"rows":      res.Rows,
	}), nil, nil
}

type execResult struct {
	AffectedRows int64 `json:"affectedRows"`
	InsertID     int64 `json:"insertId"`
}

// Write path, only reachable through the `execute` tool, which only exists when
// DBALLOWWRITE=true. Still one statement per call (MultiStatements false).
// The DB user's grants decide what actually goes through.
func writeQuery(ctx context.Context, query string, args ...any) (*execResult, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMS)*time.Millisecond)
	defer cancel()

	res, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	out := &execResult{}
	out.AffectedRows, _ = res.RowsAffected() // mysql driver never errors on these
	out.InsertID, _ = res.LastInsertId()
	return out, nil
}

func runExecute(ctx context.Context, _ *mcp.CallToolRequest, in queryArgs) (*mcp.CallToolResult, any, error) {
	res, err := writeQuery(ctx, in.SQL, in.Params...)
	if err != nil {
		return nil, nil, err
	}
	return asText(res), nil, nil
}

func main() {
	log.SetFlags(0) // stderr only: anything on stdout that isn't JSON-RPC corrupts the stream
	if err := setup(); err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	s := mcp.NewServer(&mcp.Implementation{Name: "mcp-mysql-go", Version: "0.1.0"}, nil)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_tables",
		Title:       "List tables",
		Description: fmt.Sprintf("List tables in the %s database with approximate row counts.", dbName),
	}, listTables)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "describe_table",
		Title:       "Describe table",
		Description: "Show columns, types, keys and indexes for one table.",
	}, describeTable)

	mcp.AddTool(s, &mcp.Tool{
		Name:  "query",
		Title: "Run a read-only SQL query",
		Description: fmt.Sprintf(
			"Run one read-only SQL statement against %s. Writes and DDL are rejected. "+
				"Results are capped at %d rows and %dms — add LIMIT and WHERE yourself.",
			dbName, rowLimit, timeoutMS),
	}, runQuery)

	if allowWrite {
		mcp.AddTool(s, &mcp.Tool{
			Name:  "execute",
			Title: "Run a write SQL statement",
			Description: fmt.Sprintf(
				"Run one INSERT/UPDATE/DELETE/DDL statement against %s. Commits immediately, no undo. "+
					"Use `query` for reads. Timeout %dms. sql takes ? placeholders filled from params.",
				dbName, timeoutMS),
		}, runExecute)
	}

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
