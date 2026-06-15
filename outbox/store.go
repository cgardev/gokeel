package outbox

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"
)

// Querier is the minimal execution surface the store runs its statements
// against. It is satisfied by *sql.DB, *sql.Tx, and *sql.Conn, and so by the
// querier transaction resolves from the context.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Store defines the outbound port for persisting event publications.
//
// Create receives the querier of the caller, so the publication is written
// inside the same transaction as the business change it originates from.
// Every other method runs on its own connection, because publication
// outcomes must be settled independently of any business transaction.
//
// ClaimProcessing reports whether the caller obtained the publication for
// delivery: it returns false when another dispatcher already holds it or
// has completed it, which deduplicates concurrent dispatch attempts.
type Store interface {
	Initialize(ctx context.Context) error
	Create(ctx context.Context, querier Querier, publication Publication) error
	ClaimProcessing(ctx context.Context, id uuid.UUID) (bool, error)
	MarkCompleted(ctx context.Context, id uuid.UUID, completionDate time.Time) error
	MarkFailed(ctx context.Context, id uuid.UUID) error
	MarkResubmitted(ctx context.Context, id uuid.UUID, resubmissionDate time.Time) (bool, error)
	FindIncomplete(ctx context.Context) ([]Publication, error)
	FindIncompletePublishedBefore(ctx context.Context, reference time.Time) ([]Publication, error)
}
