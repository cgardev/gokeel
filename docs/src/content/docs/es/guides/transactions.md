---
title: Transacciones
description: Ejecute una unidad de trabajo vinculada al contexto con el Manager, resuelva el querier desde el contexto y deje que el valor de retorno dirija el commit o rollback.
---

Una transacción en gokeel es una unidad de trabajo vinculada al contexto. Un `Manager` es el propietario del ciclo de vida de la transacción de la base de datos, y sus stores resuelven el querier contra el que se ejecutan a partir de `context.Context` en lugar de recibir un `*sql.Tx` a través de sus firmas. Abre una transacción una vez, el trabajo dentro de ella hace commit o rollback como uno solo, y nunca pasa una referencia de transacción a mano.

## El Manager

Un `Manager` inicia, hace commit y hace rollback de las transacciones en las que se ejecutan sus unidades de trabajo. Construya uno sobre un `*sql.DB` abierto con `NewManager`:

```go
import "github.com/cgardev/gokeel/transaction"

manager := transaction.NewManager(database)
```

`NewManager` también acepta valores opcionales `ExecutionListener` que observan el inicio, commit y rollback de cada nueva transacción que el Manager dirige. Un Manager es inmutable después de su construcción y seguro para su uso concurrente:

```go
func NewManager(database *sql.DB, listeners ...ExecutionListener) *Manager
```

## Ejecución de una unidad de trabajo

`Run` ejecuta una función como una unidad de trabajo. Sin opciones, inicia una transacción (o se une a una que ya esté vinculada al contexto), ejecuta la función y resuelve la transacción según la forma en que la función retorna:

```go
err := manager.Run(ctx, func(ctx context.Context) error {
	// ... read and write through the resolved querier ...
	return nil
})
```

La firma pasa el contexto a su función para que la transacción viaje con él:

```go
func (manager *Manager) Run(
	ctx context.Context,
	work func(ctx context.Context) error,
	opts ...Option,
) error
```

La forma en que retorna su función determina la decisión. Existen exactamente tres salidas:

```text
work returns nil    → COMMIT
work returns error  → ROLLBACK   (the error is still returned to you)
work panics         → ROLLBACK, then the panic is re-raised once it is safe
```

Un pánico hace rollback y se vuelve a lanzar (re-raise) solo después de que la transacción se haya resuelto, de modo que una transacción a medio abrir nunca puede escapar. Usted nunca llama a `Commit` o `Rollback` por sí mismo.

## Resolución del querier

Dentro de la unidad de trabajo, un store no decide si una transacción está en curso; lo pregunta. `Querier(ctx)` devuelve el ejecutor para el contexto actual: el `*sql.Tx` activo cuando hay una unidad de trabajo en progreso, o el `*sql.DB` simple para una sentencia de commit automático (auto-commit) cuando no la hay.

```go
func (s *OrderStore) Insert(ctx context.Context, order Order) error {
	return s.transactions.Run(ctx, func(ctx context.Context) error {
		querier := s.transactions.Querier(ctx) // the live *sql.Tx, resolved from ctx
		_, err := querier.ExecContext(ctx,
			`INSERT INTO orders (id, customer_email, total_cents) VALUES (?, ?, ?)`,
			order.ID, order.CustomerEmail, order.TotalCents)
		return err // nil commits this transaction; an error rolls it back
	})
}
```

Una lectura es la misma idea sin abrir un `Run`: resuelve el querier y lee en base a lo que sea que lleve el contexto.

```go
func (s *OrderStore) Total(ctx context.Context, id string) (int64, error) {
	rows, err := s.transactions.Querier(ctx).QueryContext(ctx,
		`SELECT total_cents FROM orders WHERE id = ?`, id)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	if !rows.Next() {
		return 0, rows.Err()
	}
	var total int64
	return total, rows.Scan(&total)
}
```

## La interfaz Querier

`Querier` es la superficie mínima de ejecución contra la que un store ejecuta sus sentencias. Es satisfecha por `*sql.DB`, `*sql.Tx` y `*sql.Conn`, por lo que el valor resuelto se puede pasar directamente a un constructor de consultas (query builder):

```go
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}
```

Debido a que la interfaz es exactamente `QueryContext` and `ExecContext`, el valor que devuelve `Querier(ctx)` es lo que sea que un constructor de consultas esté dispuesto a recibir, sin que este paquete tenga que importar el constructor.

## Dependencia del Transactor

Un store rara vez necesita todo el `Manager`. Necesita la parte del mismo que ejecuta una unidad de trabajo y resuelve el querier: la interfaz `Transactor`, que `*Manager` satisface:

```go
type Transactor interface {
	Run(ctx context.Context, work func(ctx context.Context) error, opts ...Option) error
	Querier(ctx context.Context) Querier
}
```

Dependa de `Transactor` en sus stores e inyecte el Manager una sola vez al momento de realizar la conexión (wiring):

```go
type OrderStore struct {
	transactions transaction.Transactor // just Run + Querier
}

func NewOrderStore(transactions transaction.Transactor) *OrderStore {
	return &OrderStore{transactions: transactions}
}

manager := transaction.NewManager(database)
orders := NewOrderStore(manager)
```

## Muchos stores, una transacción

La transacción reside en el contexto, por lo que cuando un caso de uso abre un `Run` externo y llama a varios stores dentro de él, sus `Run` internos ven la unidad de trabajo que ya está allí y se unen a ella. Ningún store pasa un `*sql.Tx`, y toda la secuencia hace commit — o rollback — en conjunto.

```go
func (uc *PlaceOrder) Do(ctx context.Context, order Order) error {
	return uc.manager.Run(ctx, func(ctx context.Context) error {
		if err := uc.orders.Insert(ctx, order); err != nil {
			return err // rolls back: no inventory was touched
		}
		for _, line := range order.Lines {
			if err := uc.inventory.Reserve(ctx, line.SKU, line.Quantity); err != nil {
				return err // rolls back the WHOLE transaction, including the order
			}
		}
		return nil // commits the order and every reservation together
	})
}
```

```text
PlaceOrder.Do
└─ manager.Run            BEGIN  (one transaction, bound to ctx)
   ├─ orders.Insert
   │   └─ Run  ──────────▶ JOINS  (no BEGIN, no COMMIT)
   ├─ inventory.Reserve
   │   └─ Run  ──────────▶ JOINS  (no BEGIN, no COMMIT)
   └─ return nil          COMMIT  (order + reservations together)
        return error      ROLLBACK (both undone)
```

Cuando una llamada de unión devuelve un error que las reglas de rollback tratan como fatal, la unidad compartida se marca como solo rollback (rollback-only) antes de que se devuelva el error. Incluso si un llamador absorbe ese error, el `Run` más externo se niega a hacer commit: hace rollback y devuelve `transaction.ErrRollbackOnly` al nivel superior.

## Retorno de un valor

La función de `Run` devuelve únicamente un `error`, lo cual es limitante cuando necesita el valor que el trabajo produjo; por ejemplo, un identificador generado. Utilice `RunResult[T]`, una función libre (los métodos de Go no pueden ser genéricos) que se ejecuta exactamente igual que `Run` pero permite que el trabajo devuelva `(T, error)`:

```go
order, err := transaction.RunResult(ctx, manager,
	func(ctx context.Context) (Order, error) {
		built := draft
		built.ID = uuid.NewString()
		if err := orders.Insert(ctx, built); err != nil {
			return Order{}, err // rolled back; the returned Order is the zero value
		}
		return built, nil
	})
```

El valor que el trabajo produjo se devuelve junto con el error que informa `Run`. En una transacción fallida o en la que se haya hecho rollback, consulte el error en lugar del valor:

```go
func RunResult[T any](
	ctx context.Context,
	transactor Transactor,
	work func(ctx context.Context) (T, error),
	opts ...Option,
) (T, error)
```

## Ajuste de un Run

`Run` y `RunResult` aceptan los mismos valores de `Option`, que configuran la transacción que se inicia:

| Option | Qué hace |
| --- | --- |
| `WithPropagation(p)` | cómo se relaciona la llamada con una transacción activa |
| `WithIsolation(level)` | nivel de aislamiento para una transacción completamente nueva |
| `ReadOnly()` | inicia la transacción como solo lectura |
| `WithTimeout(d)` | cancela el contexto de la transacción después de `d`; al expirar, el error envuelve a `ErrTransactionTimedOut`. Un `d` negativo falla con `ErrInvalidTimeout` |
| `WithName(name)` | etiqueta la unidad de trabajo, útil para el registro (logging) |

```go
err := manager.Run(ctx, placeOrderWork,
	transaction.WithName("place-order"),
	transaction.WithTimeout(2*time.Second))
```

Las opciones configuran la transacción que se inicia; un `Run` que se une a una transacción existente hereda lo que haya decidido la más externa.

Consulte [Propagación y sincronizaciones](/gokeel/es/guides/propagation-and-synchronizations/) para ver cómo se relaciona una llamada con una transacción activa y cómo engancharse (hook) en el ciclo de vida de commit y rollback.
