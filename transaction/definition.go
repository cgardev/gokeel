package transaction

import (
	"database/sql"
	"errors"
	"strconv"
	"time"
)

// Propagation selects how Run relates to a transaction that may already be
// bound to the context, mirroring Spring's propagation behaviors. The two
// behaviors that suspend the active transaction or open a second concurrent
// one (REQUIRES_NEW and NOT_SUPPORTED) are intentionally omitted: on a
// single-writer SQLite database a second concurrent transaction would deadlock
// against the write lock the first one already holds.
type Propagation int

const (
	// Required joins an active transaction or begins a new one. It is the
	// default and the propagation most callers need.
	Required Propagation = iota

	// Supports joins an active transaction when one exists, and otherwise runs
	// without a transaction.
	Supports

	// Mandatory joins an active transaction and fails with
	// ErrTransactionRequired when none exists.
	Mandatory

	// Never runs without a transaction and fails with ErrTransactionNotAllowed
	// when one is already active.
	Never

	// Nested runs within a savepoint of the active transaction, so its work can
	// roll back to the savepoint without aborting the outer transaction. It
	// begins a new transaction when none is active.
	Nested
)

// String renders the propagation for logs and test failures.
func (p Propagation) String() string {
	switch p {
	case Required:
		return "Required"
	case Supports:
		return "Supports"
	case Mandatory:
		return "Mandatory"
	case Never:
		return "Never"
	case Nested:
		return "Nested"
	default:
		return "Propagation(" + strconv.Itoa(int(p)) + ")"
	}
}

// definition is the resolved configuration of one Run, the analog of Spring's
// TransactionDefinition. It is assembled from the options passed to Run.
type definition struct {
	propagation   Propagation
	isolation     sql.IsolationLevel
	readOnly      bool
	timeout       time.Duration
	name          string
	rollbackFor   []func(error) bool
	noRollbackFor []func(error) bool
}

func newDefinition(options []Option) definition {
	resolved := definition{propagation: Required, isolation: sql.LevelDefault}
	for _, option := range options {
		option(&resolved)
	}
	return resolved
}

func (d definition) transactionOptions() *sql.TxOptions {
	return &sql.TxOptions{Isolation: d.isolation, ReadOnly: d.readOnly}
}

// shouldRollback decides whether a non-nil work error must roll the
// transaction back. By default every error does; an error matched by a
// no-rollback rule commits instead, while still being returned to the caller. A
// rollback rule takes precedence over a no-rollback rule, so an error matched by
// both still rolls back: it re-includes an error the broader no-rollback rule
// would otherwise have excused. This is the Go counterpart of Spring's
// rollbackFor winning over noRollbackFor.
func (d definition) shouldRollback(err error) bool {
	if err == nil {
		return false
	}
	for _, matches := range d.rollbackFor {
		if matches(err) {
			return true
		}
	}
	for _, matches := range d.noRollbackFor {
		if matches(err) {
			return false
		}
	}
	return true
}

// Option configures a single Run. Options are the programmatic equivalent of
// the attributes of Spring's @Transactional.
type Option func(*definition)

// WithPropagation selects the propagation behavior. The default is Required.
func WithPropagation(propagation Propagation) Option {
	return func(d *definition) { d.propagation = propagation }
}

// WithIsolation sets the isolation level a newly begun transaction requests of
// the driver. It has no effect when the call joins an existing transaction.
func WithIsolation(level sql.IsolationLevel) Option {
	return func(d *definition) { d.isolation = level }
}

// ReadOnly marks a newly begun transaction read only, a hint the driver may
// use to optimize or to refuse writes. It has no effect when the call joins an
// existing transaction.
func ReadOnly() Option {
	return func(d *definition) { d.readOnly = true }
}

// WithTimeout bounds the duration of a newly begun transaction: its context is
// cancelled once the timeout elapses, so a statement that overruns fails and
// the transaction rolls back. It has no effect when the call joins an existing
// transaction. A zero duration, the default, means no timeout; a negative
// duration is invalid and makes Run fail with ErrInvalidTimeout. When the
// timeout elapses, the error Run returns wraps ErrTransactionTimedOut.
func WithTimeout(timeout time.Duration) Option {
	return func(d *definition) { d.timeout = timeout }
}

// WithName labels the unit of work, surfaced through TransactionStatus.Name for
// logging and monitoring. It has no effect on the transaction itself.
func WithName(name string) Option {
	return func(d *definition) { d.name = name }
}

// RollbackForError forces a rollback when work returns an error matching,
// through errors.Is, any of the given sentinels, overriding any no-rollback rule
// that would otherwise excuse it. It is redundant with the default, which rolls
// back on every error, and is only useful to re-include an error a broader
// NoRollbackForError or NoRollbackForFunc rule would have committed.
func RollbackForError(targets ...error) Option {
	return func(d *definition) {
		for _, target := range targets {
			d.rollbackFor = append(d.rollbackFor, func(err error) bool {
				return errors.Is(err, target)
			})
		}
	}
}

// RollbackForFunc forces a rollback when predicate reports true for the error
// work returned, overriding any no-rollback rule that would otherwise excuse it.
func RollbackForFunc(predicate func(error) bool) Option {
	return func(d *definition) {
		d.rollbackFor = append(d.rollbackFor, predicate)
	}
}

// NoRollbackForError keeps the transaction committable when work returns an
// error matching, through errors.Is, any of the given sentinels, unless a
// RollbackForError or RollbackForFunc rule also matches. The error is still
// returned to the caller.
func NoRollbackForError(targets ...error) Option {
	return func(d *definition) {
		for _, target := range targets {
			d.noRollbackFor = append(d.noRollbackFor, func(err error) bool {
				return errors.Is(err, target)
			})
		}
	}
}

// NoRollbackForFunc keeps the transaction committable when predicate reports
// true for the error work returned. The error is still returned to the caller.
func NoRollbackForFunc(predicate func(error) bool) Option {
	return func(d *definition) {
		d.noRollbackFor = append(d.noRollbackFor, predicate)
	}
}
