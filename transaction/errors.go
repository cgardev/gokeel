package transaction

import "errors"

var (
	// ErrRollbackOnly is returned by the outermost Run when a joining call
	// failed and marked the unit rollback only, even though that call's error
	// was handled by its own caller. It is the analog of Spring's
	// globalRollbackOnly.
	ErrRollbackOnly = errors.New("unit of work marked rollback only")

	// ErrTransactionRequired is returned by Run with Mandatory propagation when
	// no transaction is active.
	ErrTransactionRequired = errors.New("mandatory propagation requires an active transaction")

	// ErrTransactionNotAllowed is returned by Run with Never propagation when a
	// transaction is already active.
	ErrTransactionNotAllowed = errors.New("never propagation forbids an active transaction")

	// ErrIncompatibleJoin is returned when a call joins an active transaction
	// with options the active transaction cannot honor, such as a stricter
	// isolation level or a read-only guarantee over a read-write transaction.
	ErrIncompatibleJoin = errors.New("requested options are incompatible with the active transaction")

	// ErrBeginFailed wraps the driver error when a transaction cannot be begun.
	// It is the analog of Spring's CannotCreateTransactionException; the wrapped
	// driver error remains reachable through errors.Is and errors.As.
	ErrBeginFailed = errors.New("could not begin the transaction")

	// ErrTransactionSystem wraps the driver error when an infrastructure step of
	// the transaction fails: a commit, a rollback, or a savepoint operation. It
	// is the analog of Spring's TransactionSystemException and lets a caller tell
	// an infrastructure failure apart from the business error work returned. The
	// wrapped driver error remains reachable through errors.Is and errors.As.
	ErrTransactionSystem = errors.New("transaction system failure")

	// ErrInvalidTimeout is returned by Run when a negative timeout is configured
	// through WithTimeout. The default, a zero duration, means no timeout. It is
	// the analog of Spring's InvalidTimeoutException.
	ErrInvalidTimeout = errors.New("timeout must not be negative")

	// ErrTransactionTimedOut wraps the work error when a transaction's own
	// timeout, configured through WithTimeout, expires before work completes. It
	// is the analog of Spring's TransactionTimedOutException and gives callers one
	// stable errors.Is target regardless of the driver's context-cancellation
	// error. A cancellation of the caller's own context is not reported as a
	// timeout.
	ErrTransactionTimedOut = errors.New("transaction timed out")
)
