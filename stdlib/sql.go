package stdlib

import (
	"context"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"

	rqlitehttp "github.com/rqlite/rqlite-go-http"
)

var _ driver.Driver = (*Driver)(nil)
var _ driver.Conn = (*Conn)(nil)
var _ driver.ConnBeginTx = (*Conn)(nil)
var _ driver.StmtExecContext = (*Stmt)(nil)
var _ driver.StmtQueryContext = (*Stmt)(nil)
var _ driver.NamedValueChecker = (*Stmt)(nil)

var ErrRequest = errors.New("rqlite request failed")
var ErrQuery = errors.New("rqlite query failed")

type Driver struct {
	HTTPClient   *http.Client
	LoadBalancer rqlitehttp.LoadBalancer

	// Basic authentication can also be passed in the DSN: http://foo:bar@host:port
	BasicAuthUser string
	BasicAuthPass string

	// QueryOptions holds default options for every /db/query request.
	// BlobAsArray is always forced to true.
	QueryOptions rqlitehttp.QueryOptions

	// ExecuteOptions holds default options for every /db/execute request.
	// Transaction is always forced to true for transaction commits.
	ExecuteOptions rqlitehttp.ExecuteOptions

	// AllowQueryInTxn permits Query/QueryContext calls during a transaction.
	// When true the query executes directly against the database outside the
	// buffered transaction. When false (default) any Query inside a transaction
	// returns ErrQuery immediately.
	AllowQueryInTxn bool
}

func (d *Driver) Open(name string) (_ driver.Conn, err error) {
	var client *rqlitehttp.Client
	if d.LoadBalancer == nil {
		if client, err = rqlitehttp.NewClient(name, d.HTTPClient); err != nil {
			return nil, err
		}
	} else {
		if client, err = rqlitehttp.NewClientWithLoadBalancer(d.LoadBalancer, d.HTTPClient); err != nil {
			return nil, err
		}
	}

	if d.BasicAuthUser != "" || d.BasicAuthPass != "" {
		client.SetBasicAuth(d.BasicAuthUser, d.BasicAuthPass)
	}

	return &Conn{
		Client:          client,
		allowQueryInTxn: d.AllowQueryInTxn,
		queryOptions:    d.QueryOptions,
		execOptions:     d.ExecuteOptions,
	}, nil
}

type Conn struct {
	*rqlitehttp.Client

	allowQueryInTxn bool
	queryOptions    rqlitehttp.QueryOptions
	execOptions     rqlitehttp.ExecuteOptions
	inTransaction   bool
	txBuffer        []*rqlitehttp.SQLStatement
}

func (c *Conn) Prepare(query string) (driver.Stmt, error) {
	return &Stmt{Stmt: query, Conn: c}, nil
}

func (c *Conn) Close() error {
	return nil
}

func (c *Conn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *Conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.inTransaction = true
	c.txBuffer = nil
	return &Tx{conn: c, ctx: ctx}, nil
}

type Tx struct {
	conn *Conn
	ctx  context.Context
}

func (tx *Tx) Commit() error {
	defer func() {
		tx.conn.inTransaction = false
		tx.conn.txBuffer = nil
	}()

	if len(tx.conn.txBuffer) == 0 {
		return nil
	}

	opts := tx.conn.execOptions
	opts.Transaction = true
	er, err := tx.conn.Execute(tx.ctx, rqlitehttp.SQLStatements(tx.conn.txBuffer), &opts)
	if err != nil {
		return err
	} else if er.Error != "" {
		return fmt.Errorf("%w: %s", ErrRequest, er.Error)
	}

	var errs []error
	for _, result := range er.Results {
		if result.Error != nil {
			errs = append(errs, result.Error)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", ErrQuery, errors.Join(errs...))
	}
	return nil
}

func (tx *Tx) Rollback() error {
	tx.conn.inTransaction = false
	tx.conn.txBuffer = nil
	return nil
}

type Result struct {
	result rqlitehttp.ExecuteResult
}

func (r *Result) LastInsertId() (int64, error) {
	return r.result.LastInsertID, nil
}

func (r *Result) RowsAffected() (int64, error) {
	return r.result.RowsAffected, nil
}

type Stmt struct {
	Stmt string
	Conn *Conn
}

func (s *Stmt) Close() error {
	return nil
}

func (s *Stmt) NumInput() int {
	return -1
}

func (s *Stmt) CheckNamedValue(nv *driver.NamedValue) error {
	if _, ok := nv.Value.(json.Number); ok {
		return nil
	}
	return driver.ErrSkip
}

func (s *Stmt) Exec(args []driver.Value) (driver.Result, error) {
	named := make([]driver.NamedValue, len(args))
	for i, v := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return s.ExecContext(context.Background(), named)
}

func buildParams(args []driver.NamedValue) ([]any, map[string]any, error) {
	positionalParams := make([]any, len(args))
	namedParams := map[string]any{}
	for _, v := range args {
		if v.Ordinal < 1 || v.Ordinal > len(args) {
			return nil, nil, fmt.Errorf("%w: invalid parameter ordinal %d", ErrQuery, v.Ordinal)
		}
		if bytes, ok := v.Value.([]byte); ok {
			// encode bytes as embedded X notation
			v.Value = "x'" + hex.EncodeToString(bytes) + "'"
		}
		positionalParams[v.Ordinal-1] = v.Value
		if v.Name != "" {
			namedParams[v.Name] = v.Value
		}
	}

	return positionalParams, namedParams, nil
}

func (s *Stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	positionalParams, namedParams, err := buildParams(args)
	if err != nil {
		return nil, err
	}

	if s.Conn.inTransaction {
		s.Conn.txBuffer = append(s.Conn.txBuffer, &rqlitehttp.SQLStatement{
			SQL:              s.Stmt,
			PositionalParams: positionalParams,
			NamedParams:      namedParams,
		})
		return &Result{}, nil
	}

	er, err := s.Conn.Execute(ctx, rqlitehttp.SQLStatements{
		&rqlitehttp.SQLStatement{
			SQL:              s.Stmt,
			PositionalParams: positionalParams,
			NamedParams:      namedParams,
		},
	}, &s.Conn.execOptions)
	if err != nil {
		return nil, err
	} else if er.Error != "" {
		return nil, fmt.Errorf("%w: %s", ErrRequest, er.Error)
	} else if len(er.Results) == 0 {
		return nil, fmt.Errorf("%w: no results returned", ErrRequest)
	} else if er.Results[0].Error != nil {
		return nil, fmt.Errorf("%w: %w", ErrQuery, er.Results[0].Error)
	}
	return &Result{result: er.Results[0]}, err
}

func (s *Stmt) Query(args []driver.Value) (driver.Rows, error) {
	named := make([]driver.NamedValue, len(args))
	for i, v := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return s.QueryContext(context.Background(), named)
}

func (s *Stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if s.Conn.inTransaction && !s.Conn.allowQueryInTxn {
		return nil, fmt.Errorf("%w: Query not allowed during transaction", ErrQuery)
	}

	positionalParams, namedParams, err := buildParams(args)
	if err != nil {
		return nil, err
	}

	opts := s.Conn.queryOptions
	opts.BlobAsArray = true
	qr, err := s.Conn.Query(ctx, rqlitehttp.SQLStatements{
		&rqlitehttp.SQLStatement{
			SQL:              s.Stmt,
			PositionalParams: positionalParams,
			NamedParams:      namedParams,
		},
	}, &opts)
	if err != nil {
		return nil, err
	} else if qr.Error != "" {
		return nil, fmt.Errorf("%w: %s", ErrRequest, qr.Error)
	}

	switch res := qr.Results.(type) {
	case []rqlitehttp.QueryResult:
		if len(res) == 0 {
			return nil, fmt.Errorf("%w: no results returned", ErrQuery)
		} else if res[0].Error != nil {
			return nil, fmt.Errorf("%w: %w", ErrQuery, res[0].Error)
		}
		return &Rows{result: res[0]}, nil
	case []rqlitehttp.QueryResultAssoc:
		if len(res) == 0 {
			return nil, fmt.Errorf("%w: no results returned", ErrQuery)
		} else if res[0].Error != nil {
			return nil, fmt.Errorf("%w: %w", ErrQuery, res[0].Error)
		}
		return &Rows{result: assocToPositional(res[0])}, nil
	default:
		return nil, fmt.Errorf("%w: result type '%T' not supported", ErrQuery, qr.Results)
	}
}

// assocToPositional converts an associative result (rows as column->value maps)
// into the positional form. The associative form carries no column order, so a
// deterministic sorted order is derived from the union of all rows' keys.
func assocToPositional(r rqlitehttp.QueryResultAssoc) rqlitehttp.QueryResult {
	columnsSet := make(map[string]struct{})
	for _, row := range r.Rows {
		for col := range row {
			columnsSet[col] = struct{}{}
		}
	}
	columns := make([]string, 0, len(columnsSet))
	for col := range columnsSet {
		columns = append(columns, col)
	}
	sort.Strings(columns)

	values := make([][]any, len(r.Rows))
	for i, row := range r.Rows {
		values[i] = make([]any, len(columns))
		for j, col := range columns {
			values[i][j] = row[col]
		}
	}

	return rqlitehttp.QueryResult{
		Columns: columns,
		Values:  values,
	}
}

type Rows struct {
	result rqlitehttp.QueryResult
	next   int
}

func (r *Rows) Columns() []string {
	return r.result.Columns
}

func (r *Rows) Close() error {
	return nil
}

func (r *Rows) Next(dest []driver.Value) error {
	if r.next >= len(r.result.Values) {
		return io.EOF
	}

	for i, v := range r.result.Values[r.next] {
		if v != nil {
			if number, ok := v.(json.Number); ok {
				// Convert to a concrete numeric type. Prefer int64 for integer
				// literals (exact across the full int64 range); fall back to
				// float64 for fractional or out-of-int64-range values.
				//
				// The concrete type matters: database/sql passes the raw driver
				// value to sql.Scanner implementations (e.g. jsontime.UnixMilli
				// only accepts int kinds), and driver.Bool.ConvertValue and other
				// concrete-type checks reject json.Number. Note scanning into
				// *json.Number is not supported with this conversion.
				if i, err := number.Int64(); err == nil {
					v = i
				} else if f, err := number.Float64(); err == nil {
					v = f
				} else {
					return err
				}
			} else if anySlice, ok := v.([]any); ok {
				tmp := make([]byte, len(anySlice))

				for i, v := range anySlice {
					switch v := v.(type) {
					case json.Number:
						if j, err := v.Int64(); err != nil {
							return err
						} else {
							tmp[i] = (byte)(j)
						}
					default:
						return fmt.Errorf("unhandled type in blob slice: %T (expected json.Number)", v)
					}
				}

				v = tmp
			}
		}
		dest[i] = v
	}

	r.next++
	return nil
}
