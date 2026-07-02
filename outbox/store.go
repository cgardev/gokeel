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
// The lifecycle mirrors the Spring Modulith event publication registry: a
// publication is created as published with one completion attempt already
// counted, transitions to processing when a dispatcher claims it, and settles
// as completed or failed; a failed publication re-enters delivery only through
// the resubmitted state, which increments the attempt counter.
//
// The attempt counter doubles as a fencing generation: every transition a
// dispatcher performs is guarded by the attempts value it holds, so the
// settlement of a dispatcher that lost its publication to a resubmission
// affects zero rows instead of overwriting the outcome of the current holder.
// This closes the unguarded-completion races the Spring Modulith repository
// is subject to.
type Store interface {
	Initialize(ctx context.Context) error
	Create(ctx context.Context, querier Querier, publication Publication) error

	// ClaimProcessing atomically transitions a published or resubmitted
	// publication of the given attempt generation into processing, stamping
	// the start of the attempt so the resubmitter grace protects the claimed
	// delivery. It reports false when another dispatcher already claimed,
	// resubmitted, or settled the publication, which makes the claim succeed
	// for exactly one of any number of concurrent dispatchers. A failed
	// publication is not claimable: it re-enters delivery only through
	// MarkResubmitted.
	ClaimProcessing(ctx context.Context, id uuid.UUID, attempts int) (bool, error)

	// MarkCompleted settles a processing publication of the given attempt
	// generation according to the completion mode of the store. It reports
	// false when the publication was fenced out: the claim was lost to a
	// resubmission, so the outcome of the current holder is preserved.
	MarkCompleted(ctx context.Context, id uuid.UUID, attempts int, completionDate time.Time) (bool, error)

	// MarkFailed records that processing the publication of the given attempt
	// generation failed, leaving it for a later resubmission. It reports false
	// when the publication was fenced out, so a lost dispatcher cannot flip a
	// row that has been reclaimed or settled by another one.
	MarkFailed(ctx context.Context, id uuid.UUID, attempts int) (bool, error)

	// MarkResubmitted transitions a published, processing, or failed
	// publication of the given attempt generation back into delivery,
	// incrementing the attempt counter and recording the resubmission date.
	// It returns the new attempt count and reports false when another caller
	// resubmitted or settled the publication first: the compare-and-set
	// re-checks the status and the attempt counter the caller observed, so a
	// staleness decision made against that observation cannot fence an
	// attempt that started after it, and two concurrent resubmissions of the
	// same entry can never both succeed.
	MarkResubmitted(ctx context.Context, id uuid.UUID, attempts int, resubmissionDate time.Time) (int, bool, error)

	FindIncomplete(ctx context.Context) ([]Publication, error)

	// FindIncompletePublishedBefore returns the incomplete publications whose
	// latest delivery attempt started before the reference time. The age of a
	// publication is measured against its last resubmission date, so the grace
	// window protects every in-flight attempt, not only the first dispatch.
	FindIncompletePublishedBefore(ctx context.Context, reference time.Time) ([]Publication, error)
}
