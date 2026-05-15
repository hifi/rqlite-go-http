# rqlite-go-http
[![Circle CI](https://circleci.com/gh/rqlite/rqlite-go-http/tree/master.svg?style=svg)](https://circleci.com/gh/rqlite/rqlite-go-http/tree/master)
[![Go Reference](https://pkg.go.dev/badge/github.com/rqlite/rqlite-go-http.svg)](https://pkg.go.dev/github.com/rqlite/rqlite-go-http)

_rqlite-go-http_ is a Go client for rqlite that interacts directly with the rqlite [HTTP API](https://rqlite.io/docs/api/), providing minimal abstraction. Robust and easy-to-use, it can be used on its own or as a foundation for higher-level libraries. The client is safe for concurrent use by multiple goroutines.

This library offers support for:

- Executing SQL statements (`INSERT`, `UPDATE`, `DELETE`)
- Running queries (`SELECT`)
- Handling both read and write statements in a single request via the _Unified Endpoint_.
- Backing up and restoring data to your rqlite system
- Booting a rqlite node from a SQLite database file
- Checking node status, diagnostic info, cluster membership, and readiness
- Ability to customize HTTP communications for control over TLS, mutual TLS, timeouts, etc.

Check out the [documentation](https://pkg.go.dev/github.com/rqlite/rqlite-go-http) for more details.

## Installation

```bash
go get github.com/rqlite/rqlite-go-http
```

### Versioning
This library is under active development and is subject to breaking changes. Be sure to use `go mod` to pin any import of this package to a specific commit.

## Example use

```Go
package main

import (
	"context"
	"fmt"
    "log"

	rqlitehttp "github.com/rqlite/rqlite-go-http"
)

func main() {
	// Create a client pointing to a rqlite node
	client, err := rqlitehttp.NewClient("http://localhost:4001", nil)
	if err != nil {
		log.Fatal(err)
	}

	// Optionally set Basic Auth
	client.SetBasicAuth("user", "password")

	// Create a table.
	resp, err := client.ExecuteSingle(context.Background(), "CREATE TABLE foo (id INTEGER PRIMARY KEY, name TEXT)")
	if err != nil {
		log.Fatal(err)
	}

	// Insert a record.
	resp, err = client.Execute(
		context.Background(),
		rqlitehttp.SQLStatements{
			{
				SQL:              "INSERT INTO foo(name) VALUES(?)",
				PositionalParams: []any{"fiona"},
			},
		},
	)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("ExecuteResponse: %+v\n", resp)

	// Query the newly created table
	qResp, err := client.QuerySingle(context.Background(), "SELECT * FROM foo")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("QueryResponse: %+v\n", qResp)
}
```

## database/sql driver

The `stdlib/rqlite` package registers rqlite as a [`database/sql`](https://pkg.go.dev/database/sql) driver, allowing you to use the standard Go database interface.

This driver supports buffered write-only transactions.
Running a read query where rows are expected to be returned during a transaction will immediately fail with an error.
Buffering means all queries will be executed on `Commit()` in a single batch and all errors will be returned by it as a single error value.
Rolling back transactions is effectively no-op as nothing will be executed until `Commit()` is called.

Said limitation can be relaxed by enabling `AllowQueryInTxn` driver flag but these will be executed out-of-band outside the transaction not reflecting any state that would be modified by it.

For extended configuration, import `stdlib` directly and register your own `stdlib.Driver`.
If a load balancer is supplied via `Driver.LoadBalancer`, the driver shares it across all connections and never closes it; it is the caller's responsibility to close it when it is no longer needed. The DSN passed to `sql.Open` is then ignored entirely, including any credentials in it; use `Driver.BasicAuthUser`/`Driver.BasicAuthPass` or embed credentials in the balancer's URLs instead.

```Go
package main

import (
	"database/sql"
	"fmt"
	"log"

	_ "github.com/rqlite/rqlite-go-http/stdlib/rqlite"
)

func main() {
	db, err := sql.Open("rqlite", "http://localhost:4001")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec("CREATE TABLE foo (id INTEGER PRIMARY KEY, name TEXT)"); err != nil {
		log.Fatal(err)
	}

	if _, err := db.Exec("INSERT INTO foo(name) VALUES(?)", "fiona"); err != nil {
		log.Fatal(err)
	}

	var name string
	if err := db.QueryRow("SELECT name FROM foo WHERE id = 1").Scan(&name); err != nil {
		log.Fatal(err)
	}
	fmt.Println(name)

	rows, err := db.Query("SELECT id, name FROM foo")
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int
		var n string
		if err := rows.Scan(&id, &n); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%d: %s\n", id, n)
	}
	if err := rows.Err(); err != nil {
		log.Fatal(err)
	}
}
```

### Errors

Statement errors surface as [`rqlitehttp.StatementError`](#handling-errors) values wrapped by the driver, so `errors.As` recovers the structured error, including the SQLite result codes, from a `database/sql` error:

```go
var stmtErr *rqlitehttp.StatementError
if errors.As(err, &stmtErr) && stmtErr.SQLite != nil {
    fmt.Println(stmtErr.SQLite.Code, stmtErr.SQLite.ExtendedCode)
}
```

Buffered statements are sent to rqlite as one atomic batch on `Commit()`, and rqlite stops executing at the first failing statement, rolling back the whole batch. The returned error therefore carries that first statement's [`rqlitehttp.StatementError`](#handling-errors), wrapped so `errors.As` still recovers it (statement errors are joined via [`errors.Join`](https://pkg.go.dev/errors#Join), so this stays correct if rqlite ever reports several).

### Numbers

The driver passes numbers in query results to `Scan` as concrete Go types — `int64` for integer values, `float64` for fractional ones — rather than the [`json.Number`](https://pkg.go.dev/encoding/json#Number) values returned by the HTTP client (see [Handling numbers](#handling-numbers)). `json.Number` can still be used as a query argument, but scanning into `*json.Number` is not supported.

`[]byte` arguments are encoded as SQLite `x'…'` blob literals, so they always bind as BLOB data even into TEXT columns; pass a `string` if you want the hex digits stored as text.

## Handling numbers
When handling a JSON response from rqlite which a number, this library stores it as a [`json.Number`](https://pkg.go.dev/encoding/json#Number). This avoids precision loss. You can then convert it to the type your schema expects. For example, if you expect an `int64`:

```go
i, err := row[0].(json.Number).Int64()
if err != nil {
    panic("number is not an int64")
}
```

The JSON specification limits the size of numbers, and the Go standard library JSON encoder represents any number larger than the limit as a string. When working with a JSON response containing such values-as-strings, use the [`math/big`](https://pkg.go.dev/math/big) package:
```go
n := &big.Int{}
v, ok := n.SetString(row[0].(json.Number).String(), 10)
if !ok {
    panic("failed to parse big int")
}
// n now holds the parsed big integer.
```

## Handling errors
A request can fail at two levels. Failures at the HTTP level, such as a node being unreachable or a `5xx` response, are always returned as a Go error. Failures at the database level, such as a constraint violation, are reported inside an HTTP `200` response. Call `Err()` on the response to check for them:

```go
resp, err := client.ExecuteSingle(ctx, "INSERT INTO foo(id) VALUES(1)")
if err != nil {
    log.Fatal(err) // HTTP-level failure.
}
if err := resp.Err(); err != nil {
    log.Fatal(err) // Database-level failure, e.g. "statement 0: UNIQUE constraint failed: foo.id"
}
```

Alternatively, call `client.PromoteErrors(true)` and the client will return the result of `Err()` alongside the response, so a single check covers both levels:

```go
client.PromoteErrors(true)
resp, err := client.ExecuteSingle(ctx, "INSERT INTO foo(id) VALUES(1)")
if err != nil { // Will be true for both HTTP-level errors and database-level failures.
    log.Fatal(err) 
}
```

A failed statement is reported as a `StatementError`, wrapped with the index of the statement that failed. Use `errors.As` to retrieve it. If the failure originated in SQLite, it also carries SQLite's [result codes](https://www.sqlite.org/rescode.html):

```go
var stmtErr *rqlitehttp.StatementError
if errors.As(err, &stmtErr) && stmtErr.SQLite != nil {
    fmt.Println(stmtErr.SQLite.Code, stmtErr.SQLite.ExtendedCode)
}
```

The `Error` field on each result is also set to the `StatementError` for that statement, which is useful when a request contains many statements. The client decodes both the legacy `error` string and the structured `error_v2` object introduced in rqlite v10.6.0, so this works against any version of rqlite.
