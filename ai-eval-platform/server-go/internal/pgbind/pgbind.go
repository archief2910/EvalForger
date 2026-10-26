// Package pgbind adapts pgx to the placeholder style this codebase writes.
//
// Every SQL statement in the server uses `?`, because that is what SQLite and
// DuckDB accept and it keeps the telemetry queries readable next to the control
// queries. PostgreSQL's protocol only understands numbered placeholders, so the
// conversion happens once, at the driver boundary, instead of in ninety
// statements — including the ones inside transactions, which database/sql
// hands straight to the driver's Tx.
//
// Rebind ignores question marks inside string literals, quoted identifiers and
// comments. It does not support PostgreSQL's `?` jsonb operator; no statement
// here uses it.
package pgbind

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/stdlib"
)

// Open returns a database handle for a PostgreSQL DSN whose statements accept
// `?` placeholders. The caller owns Close.
func Open(dsn string) *sql.DB {
	return sql.OpenDB(connector{base: stdlib.GetDefaultDriver(), dsn: dsn})
}

type connector struct {
	base driver.Driver
	dsn  string
}

func (c connector) Connect(ctx context.Context) (driver.Conn, error) {
	raw, err := c.base.(driver.DriverContext).OpenConnector(c.dsn)
	if err != nil {
		return nil, err
	}
	conn, err := raw.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &pgConn{Conn: conn}, nil
}

func (c connector) Driver() driver.Driver { return pgDriver{c.base} }

type pgDriver struct{ base driver.Driver }

func (d pgDriver) Open(name string) (driver.Conn, error) {
	c, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	return &pgConn{Conn: c}, nil
}

// pgConn delegates to pgx and rewrites the query before it is prepared.
type pgConn struct {
	driver.Conn
}

func (c *pgConn) Prepare(query string) (driver.Stmt, error) {
	return c.Conn.Prepare(Rebind(query))
}

func (c *pgConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if p, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return p.PrepareContext(ctx, Rebind(query))
	}
	return c.Conn.Prepare(Rebind(query))
}

func (c *pgConn) Begin() (driver.Tx, error) {
	txi, err := c.Conn.Begin()
	if err != nil {
		return nil, err
	}
	return &pgTx{base: txi}, nil
}

func (c *pgConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	beginner, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		return c.Begin()
	}
	txi, err := beginner.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &pgTx{base: txi}, nil
}

func (c *pgConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	execer, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return execFallback(ctx, c, query, args)
	}
	return execer.ExecContext(ctx, Rebind(query), args)
}

func (c *pgConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return queryFallback(ctx, c, query, args)
	}
	return queryer.QueryContext(ctx, Rebind(query), args)
}

func (c *pgConn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *pgConn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c *pgConn) IsValid() bool {
	if v, ok := c.Conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

// pgTx rebinds statements executed inside a transaction. database/sql routes
// Tx.ExecContext and Tx.QueryContext straight to these methods when present.
type pgTx struct {
	base driver.Tx
}

func (t *pgTx) Commit() error   { return t.base.Commit() }
func (t *pgTx) Rollback() error { return t.base.Rollback() }

func (t *pgTx) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	execer, ok := t.base.(driver.ExecerContext)
	if !ok {
		return nil, fmt.Errorf("pgbind: transaction does not support ExecContext")
	}
	return execer.ExecContext(ctx, Rebind(query), args)
}

func (t *pgTx) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := t.base.(driver.QueryerContext)
	if !ok {
		return nil, fmt.Errorf("pgbind: transaction does not support QueryContext")
	}
	return queryer.QueryContext(ctx, Rebind(query), args)
}

func execFallback(ctx context.Context, c *pgConn, query string, args []driver.NamedValue) (driver.Result, error) {
	stmt, err := c.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	return stmt.Exec(argsToValues(args))
}

func queryFallback(ctx context.Context, c *pgConn, query string, args []driver.NamedValue) (driver.Rows, error) {
	stmt, err := c.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	return stmt.Query(argsToValues(args))
}

func argsToValues(args []driver.NamedValue) []driver.Value {
	values := make([]driver.Value, len(args))
	for i, a := range args {
		values[i] = a.Value
	}
	return values
}

// Rebind turns `?` placeholders into $1..$n, leaving quoted strings, quoted
// identifiers and comments untouched.
func Rebind(query string) string {
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	for i := 0; i < len(query); i++ {
		ch := query[i]
		switch ch {
		case '\'':
			j := i + 1
			for j < len(query) {
				if query[j] == '\'' {
					if j+1 < len(query) && query[j+1] == '\'' {
						j += 2
						continue
					}
					break
				}
				j++
			}
			end := j + 1
			if end > len(query) {
				end = len(query)
			}
			b.WriteString(query[i:end])
			i = end - 1
		case '"':
			j := i + 1
			for j < len(query) {
				if query[j] == '"' {
					if j+1 < len(query) && query[j+1] == '"' {
						j += 2
						continue
					}
					break
				}
				j++
			}
			end := j + 1
			if end > len(query) {
				end = len(query)
			}
			b.WriteString(query[i:end])
			i = end - 1
		case '-':
			if i+1 < len(query) && query[i+1] == '-' {
				j := strings.IndexByte(query[i:], '\n')
				if j < 0 {
					b.WriteString(query[i:])
					i = len(query)
					break
				}
				b.WriteString(query[i : i+j+1])
				i += j
				continue
			}
			b.WriteByte(ch)
		case '/':
			if i+1 < len(query) && query[i+1] == '*' {
				depth, j := 1, i+2
				for j < len(query) && depth > 0 {
					switch {
					case j+1 < len(query) && query[j] == '/' && query[j+1] == '*':
						depth++
						j += 2
					case j+1 < len(query) && query[j] == '*' && query[j+1] == '/':
						depth--
						j += 2
					default:
						j++
					}
				}
				b.WriteString(query[i:j])
				i = j - 1
				continue
			}
			b.WriteByte(ch)
		case '?':
			n++
			b.WriteString(fmt.Sprintf("$%d", n))
		default:
			b.WriteByte(ch)
		}
	}
	return b.String()
}
