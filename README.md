# mcp-mysql-go

Read-only MySQL MCP server, Go port of [`../mcp-mysql-nodejs`](../mcp-mysql-nodejs).
Same tools (`list_tables`, `describe_table`, `query`, opt-in `execute`), same env vars, same
guards. Why it is shaped this way:
[ADR 001](../mcp-mysql-nodejs/docs/adr-001-read-only-mysql-mcp.md).

## Setup

```bash
go build -o mcp-mysql-go .
cp .env.example .env   # fill it in

# check: needs a live DB, prints "ok"
set -a; . ./.env; set +a; go test ./...
```

`go test` skips itself when `DBNAME` is unset.

### Create the read-only DB user first

The transaction guard in `main.go` is a convenience. Grants are the actual
security boundary — do this before pointing Claude at anything you care about:

```sql
CREATE USER 'mcp_readonly'@'%' IDENTIFIED BY 'a-strong-password';
GRANT SELECT, SHOW VIEW ON mydb.* TO 'mcp_readonly'@'%';
FLUSH PRIVILEGES;
```

Point this at local, dev, or a replica. Not production.

## Claude Code / Desktop config

No `--env-file` equivalent here — the MCP client passes the env instead:

```json
{
  "mcpServers": {
    "mysql-go": {
      "command": "/abs/path/mcp-mysql-go/mcp-mysql-go",
      "env": {
        "DBHOST": "127.0.0.1",
        "DBPORT": "3306",
        "DBUSER": "mcp_readonly",
        "DBPSWD": "a-strong-password",
        "DBNAME": "mydb"
      }
    }
  }
}
```

## Tools

| Tool | Input | Returns |
|------|-------|---------|
| `list_tables` | — | table name, approx row count, comment, for every table in `DBNAME` |
| `describe_table` | `table` | columns (type, nullability, key, default, extra, comment) + indexes |
| `query` | `sql`, optional `params[]` | `{ rowCount, truncated, rows }` |

Use `?` placeholders rather than string-concatenated values:

```
sql:    SELECT id, email FROM users WHERE created_at > ? LIMIT 20
params: ["2026-01-01"]
```

## Write mode

Writes are off by default. If you set `DBALLOWWRITE=true`, the server
registers a fourth tool, `execute`. The `query` tool stays read-only.

| Tool | Input | Returns |
|------|-------|---------|
| `execute` | `sql`, optional `params[]` | `{ affectedRows, insertId }` |

Each `execute` call runs one statement and commits at once. There is no undo.
The DB user grants set the real limit, so grant only what you need:

```sql
GRANT SELECT, INSERT, UPDATE ON mydb.* TO 'mcp_rw'@'%';
```

Query results reach the model as text, so a hostile row can steer the next
`execute` call. Use write mode only on local or dev databases.

## How it works

```
Claude  ──stdio JSON-RPC──▶  main.go
                              │  mcp.AddTool handler
                              ▼
                          readOnlyQuery(ctx, sql, args...)
                              │
                              ├─ ctx deadline = DBTIMEOUTMS
                              ├─ db.BeginTx(ReadOnly: true)     (pool max 2)
                              ├─ SET SESSION MAX_EXECUTION_TIME=DBTIMEOUTMS
                              ├─ tx.QueryContext(sql, args...)  (one statement)
                              ├─ stop scanning at DBROWLIMIT rows
                              └─ tx.Rollback()                  (always, deferred)
                              │
                              ▼
                    json.MarshalIndent → TextContent → Claude
```

Differences from the Node version, all of them forced by the language:

- **`database/sql` + `go-sql-driver/mysql`** instead of `mysql2/promise`.
  `sql.Open` is lazy, same as the mysql2 pool: a dead DB doesn't stop the server
  from registering tools.
- **`BeginTx(&sql.TxOptions{ReadOnly: true})`** is what emits
  `START TRANSACTION READ ONLY`, so the guard is one struct field instead of a
  raw statement.
- **Two timeouts, not one.** A `context` deadline (client side, cancels the
  wait) plus `MAX_EXECUTION_TIME` (server side, MySQL kills the statement).
- **`describe_table` reads `information_schema`** instead of `SHOW FULL COLUMNS
  FROM ??`. Go's driver has no `??` identifier escaping, and querying
  `information_schema.columns` / `.statistics` makes the table name an ordinary
  `?` placeholder — nothing to escape by hand.
- **Row cap stops the scan** at `DBROWLIMIT` rather than slicing after the fact,
  so a huge result set never lands in memory. `truncated` is set the same way.
- **Untyped columns come back as `[]byte`** and are converted to `string`, so
  dates and text land as JSON strings. Numbers stay numbers.
- **Input schemas are inferred from Go structs** (`jsonschema:"..."` tags) rather
  than declared with zod.
- **Errors are returned as `error`** — the SDK's typed handler packs them into
  the result with `isError: true` by itself.
- **No dotenv.** MCP clients set env per server (see config above); the test
  sources `.env` from the shell.

Everything else is unchanged: three tools, one `readOnlyQuery` funnel, stdio
transport, the same opt-in `execute` tool, and the same three read-only guards (driver
`MultiStatements: false`, the `READ ONLY` transaction, and the DB grants — the
grants being the real boundary).

## Known limits

Same as the Node version: one database per process, blunt row/time caps,
`information_schema` row counts are estimates on InnoDB, and query results reach
the model as text — a row containing "ignore previous instructions…" is prompt
injection with a direct line to the next tool call, which is the reason
the write path is off by default.
