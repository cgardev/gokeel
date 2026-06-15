package transaction

import (
	"cmp"
	"context"
	"database/sql"
	"log/slog"
	"slices"
	"strconv"
	"sync"
)

// Status reports how a unit of work completed, mirroring the completion
// statuses a Spring transaction synchronization receives.
type Status int

const (
	StatusCommitted Status = iota
	StatusRolledBack

	// StatusUnknown reports a completion whose outcome could not be
	// determined, such as a commit that failed midway.
	StatusUnknown
)

// String renders the status for logs and test failures.
func (s Status) String() string {
	switch s {
	case StatusCommitted:
		return "Committed"
	case StatusRolledBack:
		return "RolledBack"
	case StatusUnknown:
		return "Unknown"
	default:
		return "Status(" + strconv.Itoa(int(s)) + ")"
	}
}

// contextKey is the unexported key under which the status of the active unit of
// work is bound to a context. It is the analog of the DataSource key Spring
// uses for its thread-bound resources.
type contextKey struct{}

// TransactionStatus exposes the live state of the unit of work bound to the
// current context, the introspection analog of Spring's TransactionStatus. It
// is obtained through StatusFromContext.
type TransactionStatus struct {
	unit           *unitOfWork
	name           string
	newTransaction bool
	savepoint      bool
}

// Name returns the label given to the transaction through WithName, or the
// empty string when none was set.
func (s TransactionStatus) Name() string { return s.name }

// IsNewTransaction reports whether the current Run began the transaction rather
// than joining or nesting within an existing one.
func (s TransactionStatus) IsNewTransaction() bool { return s.newTransaction }

// HasSavepoint reports whether the current Run runs inside a savepoint, that is
// under Nested propagation within an existing transaction.
func (s TransactionStatus) HasSavepoint() bool { return s.savepoint }

// IsReadOnly reports whether the transaction was begun read only.
func (s TransactionStatus) IsReadOnly() bool { return s.unit.readOnly }

// IsCompleted reports whether the transaction has settled, that is committed or
// rolled back. Once completed, SetRollbackOnly has no effect.
func (s TransactionStatus) IsCompleted() bool { return s.unit.isCompleted() }

// IsRollbackOnly reports whether the transaction has been marked rollback only.
func (s TransactionStatus) IsRollbackOnly() bool { return s.unit.isRollbackOnly() }

// SetRollbackOnly marks the transaction so the outermost Run rolls it back even
// when work returns nil. It has no effect once the transaction has completed.
func (s TransactionStatus) SetRollbackOnly() { s.unit.markRollbackOnly() }

// orderedCallback pairs a synchronization callback with its order. Lower orders
// run first; equal orders keep their registration order.
type orderedCallback[F any] struct {
	order int
	call  F
}

// unitOfWork holds the transaction bound to the current call chain, the options
// it was begun with, its savepoint counter, and the callbacks deferred to its
// synchronization phases. One instance is created by the outermost Run and
// shared by every joining or nested Run through the context.
//
// A unit of work runs its statements and registers its callbacks from the one
// goroutine executing the work, which returns before the manager drains the
// callbacks. The mutex is a low-cost safeguard around the mutable fields, not a
// license to share one transaction across goroutines, which database/sql
// forbids for a *sql.Tx.
type unitOfWork struct {
	transaction       *sql.Tx
	isolation         sql.IsolationLevel
	readOnly          bool
	mutex             sync.Mutex
	rollbackOnly      bool
	completed         bool
	savepointCounter  int
	beforeCommit      []orderedCallback[func(ctx context.Context) error]
	beforeCompletion  []orderedCallback[func(ctx context.Context)]
	afterCommit       []orderedCallback[func(ctx context.Context)]
	afterCompletion   []orderedCallback[func(ctx context.Context, status Status)]
	savepoint         []orderedCallback[func(ctx context.Context, savepoint string)]
	savepointRollback []orderedCallback[func(ctx context.Context, savepoint string)]
}

func (unit *unitOfWork) markRollbackOnly() {
	unit.mutex.Lock()
	defer unit.mutex.Unlock()
	if unit.completed {
		return
	}
	unit.rollbackOnly = true
}

func (unit *unitOfWork) isRollbackOnly() bool {
	unit.mutex.Lock()
	defer unit.mutex.Unlock()
	return unit.rollbackOnly
}

func (unit *unitOfWork) markCompleted() {
	unit.mutex.Lock()
	unit.completed = true
	unit.mutex.Unlock()
}

func (unit *unitOfWork) isCompleted() bool {
	unit.mutex.Lock()
	defer unit.mutex.Unlock()
	return unit.completed
}

func (unit *unitOfWork) nextSavepointName() string {
	unit.mutex.Lock()
	unit.savepointCounter++
	counter := unit.savepointCounter
	unit.mutex.Unlock()
	return "transaction_savepoint_" + strconv.Itoa(counter)
}

func (unit *unitOfWork) runBeforeCommit(ctx context.Context) error {
	sortByOrder(unit.beforeCommit)
	for _, callback := range unit.beforeCommit {
		if err := callback.call(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (unit *unitOfWork) runBeforeCompletion(ctx context.Context) {
	sortByOrder(unit.beforeCompletion)
	for _, callback := range unit.beforeCompletion {
		callback.call(ctx)
	}
}

func (unit *unitOfWork) runAfterCommit(ctx context.Context) {
	sortByOrder(unit.afterCommit)
	for _, callback := range unit.afterCommit {
		recoverSynchronization("after-commit", func() { callback.call(ctx) })
	}
}

func (unit *unitOfWork) runAfterCompletion(ctx context.Context, status Status) {
	sortByOrder(unit.afterCompletion)
	for _, callback := range unit.afterCompletion {
		recoverSynchronization("after-completion", func() { callback.call(ctx, status) })
	}
}

// runSavepoint runs the callbacks registered for the creation of a savepoint,
// just after it is taken. They run inside the open transaction, so a panic
// propagates to the guard of the outermost transaction, which rolls it back.
func (unit *unitOfWork) runSavepoint(ctx context.Context, savepoint string) {
	sortByOrder(unit.savepoint)
	for _, callback := range unit.savepoint {
		callback.call(ctx, savepoint)
	}
}

// runSavepointRollback runs the callbacks registered for a rollback to a
// savepoint, just before it is rolled back. They run inside the open
// transaction, so a panic propagates to the guard of the outermost transaction.
func (unit *unitOfWork) runSavepointRollback(ctx context.Context, savepoint string) {
	sortByOrder(unit.savepointRollback)
	for _, callback := range unit.savepointRollback {
		callback.call(ctx, savepoint)
	}
}

// recoverSynchronization runs a post-completion callback, recovering and
// logging a panic so one faulty callback neither aborts the remaining ones nor
// surfaces as a failure of an already-settled transaction. Letting it escape
// could make a caller retry a durably committed transaction.
func recoverSynchronization(phase string, callback func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("synchronization callback panicked",
				"phase", phase, "panic", recovered)
		}
	}()
	callback()
}

func sortByOrder[F any](callbacks []orderedCallback[F]) {
	slices.SortStableFunc(callbacks, func(a, b orderedCallback[F]) int {
		return cmp.Compare(a.order, b.order)
	})
}

func withStatus(ctx context.Context, status *TransactionStatus) context.Context {
	return context.WithValue(ctx, contextKey{}, status)
}

// withoutUnit detaches the bound transaction so completion callbacks resolve
// their querier to the database rather than to the closed transaction.
func withoutUnit(ctx context.Context) context.Context {
	return context.WithValue(ctx, contextKey{}, (*TransactionStatus)(nil))
}

func statusFromContext(ctx context.Context) *TransactionStatus {
	status, _ := ctx.Value(contextKey{}).(*TransactionStatus)
	if status == nil || status.unit == nil || status.unit.transaction == nil {
		return nil
	}
	return status
}

func fromContext(ctx context.Context) *unitOfWork {
	if status := statusFromContext(ctx); status != nil {
		return status.unit
	}
	return nil
}

// StatusFromContext returns the status of the active unit of work and true, or
// a zero status and false when no transaction is active.
func StatusFromContext(ctx context.Context) (TransactionStatus, bool) {
	if status := statusFromContext(ctx); status != nil {
		return *status, true
	}
	return TransactionStatus{}, false
}

// CurrentTransactionName returns the name of the active transaction and true, or
// the empty string and false when no transaction is active. It is the analog of
// Spring's getCurrentTransactionName, letting logging or monitoring code read
// the name without holding a TransactionStatus.
func CurrentTransactionName(ctx context.Context) (string, bool) {
	if status := statusFromContext(ctx); status != nil {
		return status.name, true
	}
	return "", false
}

// MarkRollbackOnly marks the active transaction so the outermost Run rolls it
// back even when work returns nil. It reports false when no transaction is
// active. It is the free-function form of TransactionStatus.SetRollbackOnly.
func MarkRollbackOnly(ctx context.Context) bool {
	unit := fromContext(ctx)
	if unit == nil {
		return false
	}
	unit.markRollbackOnly()
	return true
}

// RegisterOption configures a synchronization registration.
type RegisterOption func(*registration)

type registration struct{ order int }

// WithOrder sets the order of a synchronization callback within its phase.
// Lower orders run first; callbacks with equal orders run in registration
// order. The default order is zero, so an unordered callback runs before any
// positive-order callback and after any negative-order one. This differs from
// Spring, whose default order is the lowest precedence, running unordered
// synchronizations last.
func WithOrder(order int) RegisterOption {
	return func(r *registration) { r.order = order }
}

func newRegistration(options []RegisterOption) registration {
	var resolved registration
	for _, option := range options {
		option(&resolved)
	}
	return resolved
}

// RegisterBeforeCommit schedules callback to run inside the transaction, just
// before the outermost commit. Returning an error vetoes the commit and forces
// a rollback. It reports false when no unit of work is active. The callback
// receives the in-transaction context, so its querier resolves to the
// transaction.
func RegisterBeforeCommit(
	ctx context.Context, callback func(ctx context.Context) error, opts ...RegisterOption,
) bool {
	unit := fromContext(ctx)
	if unit == nil {
		return false
	}
	reg := newRegistration(opts)
	unit.mutex.Lock()
	unit.beforeCommit = append(unit.beforeCommit,
		orderedCallback[func(context.Context) error]{order: reg.order, call: callback})
	unit.mutex.Unlock()
	return true
}

// RegisterBeforeCompletion schedules callback to run just before the outermost
// transaction commits or rolls back, while the transaction is still bound. It
// reports false when no unit of work is active.
//
// The callback cannot veto by returning, having no error result, but a panic
// forces a rollback and is re-raised to the caller once the transaction has been
// settled. This diverges from Spring, where a beforeCompletion exception is
// logged and the transaction still commits.
func RegisterBeforeCompletion(
	ctx context.Context, callback func(ctx context.Context), opts ...RegisterOption,
) bool {
	unit := fromContext(ctx)
	if unit == nil {
		return false
	}
	reg := newRegistration(opts)
	unit.mutex.Lock()
	unit.beforeCompletion = append(unit.beforeCompletion,
		orderedCallback[func(context.Context)]{order: reg.order, call: callback})
	unit.mutex.Unlock()
	return true
}

// RegisterAfterCommit schedules callback to run after the outermost
// transaction commits successfully. It reports false when no unit of work is
// active, so the caller can fall back to immediate execution on the
// auto-commit path. The callback receives a context whose transaction has been
// detached, so any database work it performs runs on the database, never on
// the closed transaction.
//
// A panic from the callback is recovered and logged, not propagated: the
// transaction is already durably committed, so an observation failure must not
// surface to the caller as a failed transaction, which could trigger a retry of
// committed work, and must not abort the remaining after-commit callbacks. This
// diverges from Spring, where an afterCommit exception propagates out of the
// commit to the caller and aborts the remaining callbacks.
func RegisterAfterCommit(
	ctx context.Context, callback func(ctx context.Context), opts ...RegisterOption,
) bool {
	unit := fromContext(ctx)
	if unit == nil {
		return false
	}
	reg := newRegistration(opts)
	unit.mutex.Lock()
	unit.afterCommit = append(unit.afterCommit,
		orderedCallback[func(context.Context)]{order: reg.order, call: callback})
	unit.mutex.Unlock()
	return true
}

// RegisterAfterCompletion schedules callback to run after the outermost
// transaction commits or rolls back, receiving the final Status. It reports
// false when no unit of work is active.
func RegisterAfterCompletion(
	ctx context.Context, callback func(ctx context.Context, status Status), opts ...RegisterOption,
) bool {
	unit := fromContext(ctx)
	if unit == nil {
		return false
	}
	reg := newRegistration(opts)
	unit.mutex.Lock()
	unit.afterCompletion = append(unit.afterCompletion,
		orderedCallback[func(context.Context, Status)]{order: reg.order, call: callback})
	unit.mutex.Unlock()
	return true
}

// RegisterSavepoint schedules callback to run just after a savepoint is created,
// that is when a Nested Run takes a savepoint of the active transaction. The
// savepoint argument identifies the savepoint. It reports false when no unit of
// work is active. A panic from the callback rolls the whole transaction back.
func RegisterSavepoint(
	ctx context.Context, callback func(ctx context.Context, savepoint string), opts ...RegisterOption,
) bool {
	unit := fromContext(ctx)
	if unit == nil {
		return false
	}
	reg := newRegistration(opts)
	unit.mutex.Lock()
	unit.savepoint = append(unit.savepoint,
		orderedCallback[func(context.Context, string)]{order: reg.order, call: callback})
	unit.mutex.Unlock()
	return true
}

// RegisterSavepointRollback schedules callback to run just before a rollback to
// a savepoint, that is when a Nested Run rolls its savepoint back. The savepoint
// argument identifies the savepoint. It reports false when no unit of work is
// active. A panic from the callback rolls the whole transaction back.
func RegisterSavepointRollback(
	ctx context.Context, callback func(ctx context.Context, savepoint string), opts ...RegisterOption,
) bool {
	unit := fromContext(ctx)
	if unit == nil {
		return false
	}
	reg := newRegistration(opts)
	unit.mutex.Lock()
	unit.savepointRollback = append(unit.savepointRollback,
		orderedCallback[func(context.Context, string)]{order: reg.order, call: callback})
	unit.mutex.Unlock()
	return true
}
