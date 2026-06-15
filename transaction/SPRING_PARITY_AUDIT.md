# Spring `spring-tx` Parity Audit — `transaction`

**Date:** 2026-06-13
**Subject:** `github.com/cgardev/gokeel/transaction`
**Reference:** `org.springframework:spring-tx` (`.tmp/spring-framework-main/spring-tx`)
**Method:** 15-dimension comparison, one agent per Spring transaction concept; every claimed
gap/divergence/bug adversarially verified against the actual source on both sides (77 agents).

**Update (2026-06-13):** the three priority findings in §3 have been addressed — §3.1 and §3.2
documented, §3.3 implemented as the `ExecutionListener` type. See the per-finding Status notes and §9.

---

## 1. Executive verdict

The Go package is a **faithful and correct** port of Spring's `@Transactional` within its declared
scope (a single datasource, single in-flight database transaction; primary backend SQLite, also tested
on PostgreSQL). After adversarial verification there are **no correctness defects** that could cause data
loss, a stuck or leaked transaction, a swallowed failure, or a wrong commit/rollback decision.

What surfaces as "wrongly implemented" is, in every confirmed case, a **deliberate behavioral
divergence** — most of them defensible and several strictly safer than Spring's default. The propagation
decision tree, the synchronization phase lifecycle, the rollback rules, and the rollback-only logic
reproduce `AbstractPlatformTransactionManager` faithfully.

### Severity distribution (verified, real findings)

| Severity | Count |
|---|---|
| Critical | 0 |
| High | 0 |
| Medium | 3 (2 from the dimensions + 1 from the completeness critic) |
| Low | 37 |
| Info | 19 |

The adversarial verification pass **refuted 3 candidate findings** that did not survive a source check
(see §6).

---

## 2. Methodology

Fifteen independent comparison agents each read the full Go source
(`manager.go`, `context.go`, `definition.go`, `errors.go`) plus the specific Spring reference files for
their dimension, and emitted structured findings classified as
`full-parity` / `intentional-omission` / `partial` / `divergent` / `missing` / `bug`.

Every non-parity finding was then handed to an **adversarial verifier** instructed to refute it: open the
cited `file:line` on both sides, confirm the claimed Spring behavior is real, confirm the Go behavior is
real and not handled elsewhere, and down-rank inflated severities. A final completeness critic searched for
Spring behaviors that no dimension had examined.

Dimensions: propagation, rollback rules, synchronization lifecycle, synchronization exception handling,
status introspection, definition attributes, isolation, timeout, read-only, rollback-only, savepoint/nested,
commit/rollback failure, empty-transaction synchronization, concurrency/resource binding, programmatic API.

---

## 3. Priority findings (action-worthy)

### 3.1 `afterCommit` panic is swallowed in Go but propagates to the caller in Spring — **medium / divergent**

- **Spring:** after a durable `doCommit`, an exception from any `afterCommit` synchronization
  **propagates out of `commit()`** to the caller, and aborts the remaining `afterCommit` callbacks.
  The behavior is explicitly documented: *"Trigger afterCommit callbacks, with an exception thrown there
  propagated to callers but the transaction still considered as committed."*
  - `AbstractPlatformTransactionManager.java:832-842`, `TransactionSynchronizationUtils.java:152-168`
- **Go:** `runAfterCommit` wraps every callback in `recoverSynchronization`, which recovers the panic,
  logs it via `slog.Error`, and continues to the next callback. The panic never reaches `Run`'s caller, and
  `Run` returns `nil` on the success path. Unlike Spring, a panic in one `afterCommit` callback does **not**
  abort the later ones.
  - `context.go:141-167`, `manager.go:189-194`; test `robustness_test.go:119-153`
- **Assessment:** deliberate and defensible (the doc comment reasons that letting an `afterCommit` failure
  escape could make a caller retry a durably-committed transaction), but **not** explained by the
  single-datasource scope, and currently undocumented as a divergence from Spring. A team porting Spring
  code that relies on an `afterCommit` failure surfacing at the call site would silently lose that signal.
- **Recommendation:** keep the swallow; document the divergence in the `Run` / `RegisterAfterCommit` doc
  comment. Optionally offer an opt-in to surface `afterCommit` panics for parity, but do not change the
  default.
- **Status (2026-06-13): Done — documented.** The divergence is now stated in the `RegisterAfterCommit`
  doc comment (`context.go`); behavior is unchanged.

### 3.2 `beforeCompletion` panic forces rollback + re-raise in Go; Spring swallows it and still commits — **medium / divergent**

- **Spring:** `triggerBeforeCompletion` wraps each callback in `try/catch(Throwable)` and only logs; the
  throw is swallowed and `processCommit` proceeds to `doCommit`, so the transaction **commits** despite the
  failing `beforeCompletion`.
  - `TransactionSynchronizationUtils.java:135-144`, `AbstractPlatformTransactionManager.java:775,993-997`
- **Go:** `runGuardedVoid` recovers the panic, sets `rollback = true`, rolls back, and **re-raises** the
  panic after settlement via `reraise(...)`. So a `beforeCompletion` panic both **vetoes the commit** and
  **propagates** — the opposite of Spring on both counts.
  - `manager.go:166-174`, `context.go:134-139`; test `robustness_test.go:95-117`
- **Assessment:** Go's behavior (fail loud, roll back, re-raise) is arguably safer, but it is a real
  fidelity gap, orthogonal to the datasource count. A `beforeCompletion` callback that returns normally
  cannot veto (the signature has no error return), so only a panic changes the outcome — a reasonable
  mapping.
- **Recommendation:** document the divergence in `RegisterBeforeCompletion`.
- **Status (2026-06-13): Done — documented.** The divergence is now stated in the
  `RegisterBeforeCompletion` doc comment (`context.go`).

### 3.3 `TransactionExecutionListener` observation hooks have no Go analog — **medium / missing** (completeness critic)

- **Spring (6.1):** a stateless observation contract distinct from synchronizations.
  `ConfigurableTransactionManager` carries a `Collection<TransactionExecutionListener>`, and the manager
  fires `beforeBegin/afterBegin` around `doBegin`, `beforeCommit/afterCommit` around `doCommit`, and
  `beforeRollback/afterRollback` around `doRollback`. The `after*` hooks receive the begin/commit/rollback
  `Throwable` (or `null`), making this the canonical seam for metrics, tracing, and structured logging.
  - `TransactionExecutionListener.java`, `AbstractPlatformTransactionManager.java:530-540,783-841,886-932,971-975`,
    `ConfigurableTransactionManager.java:30-53`
- **Go:** `Manager` exposes only a `*sql.DB`; there is no listener registration surface and no
  begin/commit/rollback observation callbacks. The only lifecycle hooks are the four data-oriented
  synchronization phases.
  - `manager.go:38-45,135-194`, `context.go:246-316`
- **Recommendation:** given the recent per-package logging effort (commit `51e1683`), an
  `afterBegin/afterCommit(err)/afterRollback(err)` hook is a natural fit. Either add a minimal listener
  slice on `Manager`, or document the absence as an intentional simplification.
- **Status (2026-06-13): Done — implemented.** Added the `ExecutionListener` type (`listener.go`) with the
  six begin/commit/rollback hooks, wired into `runNew` with Spring-faithful ordering: hooks fire only for a
  new physical transaction (never for a joining or nested savepoint `Run`) and after the matching
  synchronization phases. Each hook is panic-isolated via `recoverListener`, and listeners are registered
  at construction through the variadic `NewManager(database, listeners...)`. Covered by `listener_test.go`.

---

## 4. Missing features (absent in Go)

| Spring feature | Status in Go | Severity | Evidence (Go / Spring) |
|---|---|---|---|
| Positive `rollbackFor` rule (`RollbackRuleAttribute`) | Only `NoRollbackFor` exists; Go already rolls back on *every* error by default, so impact is low | low / partial | `definition.go:85-95,136-152` / `RuleBasedTransactionAttribute.java`, `RollbackRuleAttribute.java` |
| `savepoint()` / `savepointRollback()` synchronization callbacks | Absent although savepoints are implemented | low / missing | `context.go:91-101` / `TransactionSynchronization.java` |
| `isCompleted()` + double-settle / post-settlement mutation guard | No `completed` flag; `SetRollbackOnly` on a status retained into a completion callback mutates a dead unit with no error | low / missing | `context.go:48-72,104-114` / `TransactionExecution.java:120-127`, `AbstractPlatformTransactionManager.java:735-738,859-862` |
| `isCurrentTransactionReadOnly()` / read-only exposure | `def.readOnly` is not observable; no `IsReadOnly()` on `TransactionStatus` | low/info / missing | `context.go:48-72` / `AbstractPlatformTransactionManager.java`, `TransactionSynchronizationManager.java` |
| Typed exception hierarchy (`TransactionSystemException`, `CannotCreateTransactionException`) | Flat sentinels via `errors.Join`; `BeginTx`/`Commit` failures surface as raw driver errors | low / partial | `errors.go`, `manager.go:135-138,185-188` / `TransactionSystemException.java`, `CannotCreateTransactionException.java` |
| Empty-transaction synchronization (`SUPPORTS`/`NEVER` without a tx) | `Register*` return `false`; callbacks do **not** fire and no `afterCompletion` runs | low / divergent | `manager.go:64-78`, `context.go:246-316` / `AbstractPlatformTransactionManager.java` (`SYNCHRONIZATION_ALWAYS`) |
| `getCurrentTransactionName()` for logging outside a held status | Name reachable only via `StatusFromContext` while a unit is active; not on the detached completion context | low / partial | `context.go:55-57,175-207` / `AbstractPlatformTransactionManager.java:582`, `TransactionSynchronizationManager.java:390-408` |
| Synchronization de-duplication (`LinkedHashSet`) | `append` to a slice with no identity check; the same callback registered twice fires twice | low / divergent | `context.go:98-101,255-256,273-274,294-295,312-313` / `TransactionSynchronizationManager.java:320,333-342` |
| `Run` returning a value `T` (`TransactionTemplate.execute`) | `work` returns only `error`; no generic `RunResult[T]` | low / divergent | `manager.go:52-54` / `TransactionTemplate.java`, `TransactionCallback.java` |
| `failEarlyOnGlobalRollbackOnly` | Absent (Go matches Spring's default `false`) | info / partial | `manager.go:92-106` / `AbstractPlatformTransactionManager.java` |
| `TransactionOperations.withoutTransaction()` (no-op operations) | No counterpart | info / missing | — / `TransactionOperations.java`, `WithoutTransactionOperations.java` |

**Update (2026-06-13): most of these are implemented; the rest are documented non-goals.**

Implemented:
- Positive rollback rules — `RollbackForError` / `RollbackForFunc`, with a rollback rule winning over a
  no-rollback rule (`definition.go`).
- `savepoint()` / `savepointRollback()` synchronization callbacks — `RegisterSavepoint` /
  `RegisterSavepointRollback`, fired around the savepoint of a `Nested` Run (`context.go`, `manager.go`).
- `IsCompleted()` plus a post-settlement guard that makes `SetRollbackOnly` a no-op after completion
  (`context.go`, `manager.go`).
- `IsReadOnly()` accessor on `TransactionStatus` (`context.go`).
- Typed infrastructure errors — `ErrBeginFailed` (begin) and `ErrTransactionSystem` (commit, rollback,
  savepoint), wrapping the driver error while remaining `errors.Is`-matchable (`errors.go`, `manager.go`).
- `CurrentTransactionName(ctx)` free function for logging correlation (`context.go`).
- `RunResult[T]` generic, the result-returning counterpart of `Run` (`manager.go`).

Documented as non-goals (no code change, following each finding's own conclusion):
- Empty-transaction synchronization for `SUPPORTS` / `NEVER` without an active transaction — documented in
  the package doc: callbacks require an active unit of work.
- Synchronization de-duplication — documented: registration is multiplicity-preserving.
- `failEarlyOnGlobalRollbackOnly` — Go matches Spring's default (off); not added.
- `TransactionOperations.withoutTransaction()` — a no-op transactor is trivially the work function called
  directly; not added.

Covered by `features_test.go`.

---

## 5. Behavioral divergences (potential "wrong", mostly defensible)

- **Default synchronization order.** Spring defaults to `LOWEST_PRECEDENCE` (callbacks run *last*); Go
  defaults to `0` (middle). Mixing ordered and unordered callbacks yields a different relative order than
  Spring. → For parity, the default should be `MaxInt`. `context.go:224-231` / `sync-lifecycle`.
- **Join validation stricter than Spring's default.** `validateJoin` always rejects a different explicit
  isolation level or a read-only request over a read-write transaction; Spring with the default
  `validateExistingTransaction=false` silently overrides the inner settings. Go equals Spring configured
  with `validateExistingTransaction=true`, which Spring's own javadoc recommends. *Safer, not worse.*
  `manager.go:108-121`.
- **Read-only join direction is inverted.** Spring guards against a read-write child corrupting a
  read-only-declared scope; Go forbids *tightening* to read-only when joining a read-write transaction.
  Defensible (the guarantee would otherwise be a lie), but the opposite direction. `manager.go:116-119`.
- **`NESTED` also runs join validation in Go;** Spring never validates isolation for `NESTED`. Minor.
- **Timeout.** ~~No validation of negative values; on expiry surfaces raw `context.DeadlineExceeded`~~
  **resolved (2026-06-13)** — a negative timeout fails fast with `ErrInvalidTimeout` (Spring's
  `InvalidTimeoutException`), and on expiry the work error is wrapped with `ErrTransactionTimedOut`
  (Spring's `TransactionTimedOutException`), a stable `errors.Is` target, via `context.WithTimeoutCause` so
  a cancellation of the caller's own context is not misreported as a timeout. Granularity is `time.Duration`
  vs Spring's whole seconds (idiomatic, intended). `definition.go`, `errors.go`, `manager.go`.
- **Local vs global rollback-only collapsed** into one boolean: cannot distinguish "I asked to roll back"
  from "a participant failed and I swallowed its error". Acceptable for a single-process model.
  `context.go:91-114`.
- **`globalRollbackOnParticipationFailure` hard-wired on**, with no opt-out for the originator. Go matches
  Spring's default; no early-fail (`failEarlyOnGlobalRollbackOnly`) mode. `manager.go:92-106`.
- **`RELEASE SAVEPOINT` after `ROLLBACK TO SAVEPOINT`.** Go issues `RELEASE` after rolling back to the
  savepoint. A closer reading of Spring (`AbstractTransactionStatus.rollbackToHeldSavepoint`,
  `AbstractTransactionStatus.java:154-164`) shows it rolls back **and releases** the savepoint as well, so
  this is **not** a divergence — the original audit note misread it. **Resolved (2026-06-13):** the only
  real issue was the unconditionally swallowed `RELEASE` error; it is now logged with `slog.Warn` so a
  genuine driver or connection failure stays observable (`manager.go`).
- **Failed `ROLLBACK TO SAVEPOINT` escalates to `markRollbackOnly`** rather than a typed system exception.
  Safe, idiomatic; defers the abort to the outer commit. `manager.go:241-247`.
- **Default rollback policy inverted.** Go rolls back on every error; Spring commits on checked exceptions.
  Low impact because Go has no checked/unchecked distinction. `definition.go:82-95`.
- **Registration returns `false`** when no transaction is active, instead of throwing Spring's
  `IllegalStateException`. Cleaner for the context model but requires callers to check the boolean.
  `context.go:246-316`.

---

## 6. Refuted candidate findings (false positives caught by verification)

1. **"No most-specific-wins arbitration between opposite rollback rules"** — *not real.* Go exposes only
   single-polarity (no-rollback) rules, so Spring's depth-based tie-break is unreachable; the outcome is
   identical. (`RuleBasedTransactionAttribute.java:122-143`, `RollbackRuleAttribute.java:165-186`)
2. **"Savepoint name via string concatenation = injection risk"** — *not real.* The name is fully internal
   (`foundationtransaction_savepoint_` + counter), mirroring Spring's own `ConnectionHolder` scheme;
   `database/sql` has no parameterized savepoint API, so concatenation is the only mechanism.
   (`manager.go:231,242,252`, `context.go:116-122` / `ConnectionHolder.java:48,182-184`)
3. **"Callback draining is not snapshot-guarded against re-entrant registration"** — *not real.* Spring
   exhibits the same drop (the snapshot is a no-`ConcurrentModificationException` guarantee, not a
   run-it-this-phase guarantee). Full behavioral parity. (`TransactionSynchronizationManager.java:351-371`)

---

## 7. Intentional omissions — correct

- **`REQUIRES_NEW` and `NOT_SUPPORTED`** are omitted by design (they need a second concurrent transaction
  or suspension, which would deadlock the single SQLite writer). The handling is **structurally safe**:
  the values are absent from the enum (no silent degradation to `REQUIRED`), and any out-of-range
  `Propagation` hits the loud `default` case in `Run`. `definition.go:10-39`, `manager.go:84-86`.
  *Suggested doc note:* callers needing an independent commit must run outside the unit of work.
- **Programmatic savepoint control (`SavepointManager`)** — correctly replaced by declarative `Nested`
  propagation; the manager owns the savepoint lifecycle.
- **`flush()`, `suspend()`, `resume()` synchronization callbacks** — no session-backed resource / no
  suspension in scope.
- **`HeuristicCompletionException`, `NestedTransactionNotSupportedException`,
  `InvalidIsolationLevelException`** — JTA / driver territory; unsupported isolation is rejected by the
  driver at `BeginTx`.
- **Keyed resource map** (`getResourceMap`/`bindResource`/`unbindResource`) replaced by a single
  context-bound `*sql.Tx` — correct for one datasource.
- **`afterCompletion(STATUS_UNKNOWN)` foreign-transaction fallback** — unnecessary; Go has a single owner
  driving `afterCompletion` with a concrete status.
- **Context + mutex model** instead of `ThreadLocal` — idiomatic and goroutine-safe by construction
  (verified against `concurrency_test.go`, `robustness_test.go`).

---

## 8. Confirmed full-parity behaviors

These were checked and match Spring faithfully (parity is a result, recorded for completeness).

**Propagation:** REQUIRED join-or-begin; SUPPORTS join-or-non-transactional; MANDATORY errors with no tx;
NEVER errors with a tx; NESTED savepoint-or-fallback.

**Synchronization lifecycle:** core phase ordering on commit; `beforeCommit` runs only when not rolling
back while `beforeCompletion` always runs; `beforeCommit` veto/error semantics; `afterCommit` only on clean
commit, `afterCompletion` always with a status; post-commit failures swallowed-and-logged; equal-order
callbacks keep registration order (stable sort).

**Synchronization exceptions:** `beforeCommit` panic/veto both force rollback and (for panic) re-raise;
`afterCompletion` panic recovered+logged, not propagated; the pre-settlement guard/`reraise` logic in
`runNew` is correct — no transaction can leak open, and panics are re-raised only after settlement.

**Status introspection:** `Name()`, `IsNewTransaction()`, `HasSavepoint()`, `IsRollbackOnly()` (incl.
global flag) have full parity; `StatusFromContext` / `MarkRollbackOnly` add a context-idiomatic path.

**Definition attributes:** defaults match (`Required`, `LevelDefault`, read-write, empty name); option
construction equivalent to `DefaultTransactionDefinition`.

**Isolation / timeout:** custom isolation applied via driver `TxOptions`; `LevelDefault` mismatch on join
tolerated; timeout via context cancellation covers the same and more cases; only new transactions get a
timeout, joined/nested inherit the outer deadline.

**Rollback-only:** a swallowed inner participating failure rolls back the whole transaction and surfaces
`ErrRollbackOnly`, returned only when no other error is present (matches Spring's silent-rollback signal).

**Savepoint / nested:** nested rollback preserves the outer transaction; savepoint released on success and
after rollback-to; monotonic counter naming matches `ConnectionHolder`; synchronizations attributed to the
outermost transaction; a panic in nested work rolls back the whole transaction.

**Commit failure:** commit-failure path runs `afterCompletion(StatusUnknown)`, skips `afterCommit`, does
not roll back (matches Spring's default); `StatusUnknown` maps to `STATUS_UNKNOWN`; the transaction is left
in a defined state after a failed commit.

**Concurrency:** context-bound `unitOfWork` replaces `ThreadLocal`; the `sync.Mutex` is necessary,
sufficient, and correctly scoped; `*sql.Tx` is never shared across goroutines; cleanup is structural and
leak-free.

**Programmatic API:** callback-scoped demarcation with runtime-error-triggers-rollback matches
`TransactionTemplate.execute`; `IllegalTransactionStateException` maps to
`ErrTransactionRequired`/`ErrTransactionNotAllowed`; `UnexpectedRollbackException` maps to
`ErrRollbackOnly`.

---

## 9. Prioritized recommendations

Nothing is urgent. In descending order of value-to-cost:

1. ~~**Document the two panic-handling divergences** (§3.1, §3.2) in the `Run` / `RegisterAfterCommit` /
   `RegisterBeforeCompletion` doc comments.~~ **Done (2026-06-13).**
2. ~~**Add lifecycle observation hooks** (a minimal `TransactionExecutionListener` analog — §3.3) if
   metrics/tracing of begin/commit/rollback are wanted.~~ **Done (2026-06-13)** — `ExecutionListener`.
3. **Harden details:** ~~expose `IsReadOnly()` and `IsCompleted()`; validate a negative timeout~~ **done
   (2026-06-13)** — a negative timeout now fails fast with `ErrInvalidTimeout`. The "skip the redundant
   `RELEASE` after `ROLLBACK TO SAVEPOINT`" idea was **dropped after checking the source**: Spring's
   `rollbackToHeldSavepoint` (`AbstractTransactionStatus.java:154-164`) rolls back **and releases** the
   savepoint, so the current `ROLLBACK TO` + `RELEASE` already matches Spring. Instead the swallowed
   `RELEASE` error is now logged (`slog.Warn`) so a genuine driver failure stays observable.
4. ~~**Decide the default synchronization order** (`0` vs `MaxInt`).~~ **Decided (2026-06-13): keep `0`** and
   document the divergence in `WithOrder`. Changing the default would break the documented contract and the
   ordering tests for marginal benefit; the current model (negative = earlier, zero = baseline, positive =
   later) is more intuitive than Spring's "unordered runs last."

---

## 10. Appendix — full finding inventory

Counts of verified, real (non-refuted) findings per dimension: `real` = confirmed; `refuted` = dismissed by
the verifier; `parity` = behaviors confirmed equivalent.

| Dimension | real | refuted | parity |
|---|---|---|---|
| propagation | 3 | 0 | 5 |
| rollback-rules | 2 | 1 | 2 |
| sync-lifecycle | 6 | 0 | 6 |
| sync-exceptions | 2 | 0 | 3 |
| status-introspection | 6 | 0 | 5 |
| definition-attributes | 4 | 0 | 5 |
| isolation | 4 | 0 | 2 |
| timeout | 3 | 0 | 2 |
| readonly | 3 | 0 | 1 |
| rollback-only | 4 | 0 | 2 |
| savepoint-nested | 5 | 1 | 5 |
| commit-failure | 3 | 0 | 4 |
| empty-tx-sync | 3 | 0 | 0 |
| concurrency-binding | 4 | 1 | 4 |
| programmatic-api | 6 | 0 | 3 |

### Info-level findings (recorded, low impact)

- **rollback-rules:** default policy inverted — Go rolls back on every error vs Spring committing on checked
  exceptions.
- **sync-lifecycle:** `flush()` absent; `suspend()`/`resume()` absent (intentional).
- **status-introspection:** `isCompleted` no analog; `isReadOnly` not exposed despite being tracked;
  `flush()` not exposed (intentional).
- **definition-attributes:** no validation of propagation/isolation constant ranges that Spring enforces at
  set time.
- **isolation:** `NESTED` also runs join validation (Spring does not); no `InvalidIsolationLevelException`
  analog (driver rejects at `BeginTx`).
- **timeout:** granularity is `time.Duration` vs whole seconds.
- **rollback-only:** no "already completed" double-settle guard (intentional).
- **savepoint-nested:** no programmatic `SavepointManager` surface; no savepoint-specific synchronization
  phase; no `NestedTransactionNotSupportedException` gate (intentional).
- **commit-failure:** optional `rollbackOnCommitFailure` mode and rollback-overrides-commit-exception
  escalation not reproduced; no `HeuristicCompletionException` analog (intentional).
- **concurrency-binding:** single `*sql.Tx` replaces the keyed resource map (intentional); transaction
  characteristics exposed via the `TransactionStatus` value rather than separate context-bound accessors.
- **programmatic-api:** `TransactionOperations.withoutTransaction()` has no counterpart.
