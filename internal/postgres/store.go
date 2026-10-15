// Package postgres implements the repositories of the app package on PostgreSQL with pgx.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ronybrand/go-order-api/internal/app"
	"github.com/ronybrand/go-order-api/internal/apperr"
)

// Constraint names from migrations/0001_init.up.sql that map to a client-facing code.
const (
	taxIDIndex    = "customers_tax_id_active_uq"
	passportIndex = "customers_passport_active_uq"
)

// Postgres error codes (SQLSTATE) the repositories translate.
const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
	pgCheckViolation      = "23514"
	pgStringTooLong       = "22001"
	pgSerializationFail   = "40001"
	pgDeadlockDetected    = "40P01"
)

// db is what a repository needs from a connection: the pool and an open transaction both satisfy it, which is
// how the same repository code runs inside or outside a transaction. Begin on a transaction opens a savepoint.
type db interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Store is the app.Store backed by a connection pool.
type Store struct {
	pool *pgxpool.Pool
}

var _ app.Store = (*Store)(nil)

// NewStore wraps a pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Repos returns repositories that run each statement on its own connection.
func (s *Store) Repos() app.Repositories { return repositoriesOn(s.pool) }

// InTx runs fn in a transaction and rolls back on any error or panic.
func (s *Store) InTx(ctx context.Context, fn func(ctx context.Context, repos app.Repositories) error) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return fn(ctx, repositoriesOn(tx))
	})
	return translate(err)
}

func repositoriesOn(conn db) app.Repositories {
	return app.Repositories{Customers: &customerRepo{db: conn}, Orders: &orderRepo{db: conn}}
}

// translate converts the database errors the client should understand into apperr values and leaves
// everything else untouched (the HTTP layer reports those as INTERNAL-00 without their text).
func translate(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := apperr.As(err); ok {
		return err
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case pgUniqueViolation:
		switch pgErr.ConstraintName {
		case taxIDIndex:
			return apperr.Conflict(apperr.CodeDuplicateTaxID, "a customer with this tax_id already exists", nil)
		case passportIndex:
			return apperr.Conflict(apperr.CodeDuplicatePassport, "a customer with this passport_number already exists", nil)
		}
		return integrityError()
	case pgForeignKeyViolation, pgCheckViolation, pgStringTooLong:
		return integrityError()
	case pgSerializationFail, pgDeadlockDetected:
		return concurrentModification()
	}
	return err
}

func integrityError() error {
	return apperr.Conflict(apperr.CodeDataIntegrity, "the operation violates a data integrity rule", nil)
}

func concurrentModification() error {
	return apperr.Conflict(apperr.CodeConcurrentModification,
		"the resource was modified by someone else, reload it and try again", nil)
}

func wrap(op string, err error) error {
	if err = translate(err); err == nil {
		return nil
	}
	if _, ok := apperr.As(err); ok {
		return err
	}
	return fmt.Errorf("%s: %w", op, err)
}
