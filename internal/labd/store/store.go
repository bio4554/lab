// Package store provides typed access to the lab schema. All queries
// are plain pgx, schema-qualified, no ORM.
package store

import (
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a query targets a row that does not
// exist (or is not in the state the operation requires, e.g. ending a
// session that already ended).
var ErrNotFound = errors.New("store: not found")

// ErrDuplicateName is returned when an insert collides with a
// uniqueness constraint on a caller-chosen name (agent name within a
// project, project name). The API layer maps it to 409.
var ErrDuplicateName = errors.New("store: duplicate name")

// Store wraps a pgx pool with typed queries for the lab schema.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store backed by the given pool. The Store does not own
// the pool; the caller closes it.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}
