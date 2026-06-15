---
title: Análisis de Spring y propuesta de equivalente en Go
description: Las ideas de Spring y Spring Modulith que inspiran gokeel, el hueco en el ecosistema Go y cómo el proyecto lo cubre con un núcleo de biblioteca estándar.
---

# Análisis de Spring y propuesta de equivalente en Go

## 1. Síntesis de Spring — la transacción declarativa y el outbox

### 1.1 Dos ideas que gokeel reescribe

gokeel toma prestadas dos piezas que Spring y Spring Modulith hicieron familiares,
y las reimplementa en Go sobre la biblioteca estándar:

- **`@Transactional` (Spring Framework)** — demarcación declarativa de
  transacciones: el método no abre ni cierra la conexión, sólo declara que su
  trabajo es una unidad atómica; el contenedor decide *propagación*, *aislamiento*,
  *reglas de rollback* y *sincronizaciones*.
- **El registro de publicación de eventos (Spring Modulith)** — el patrón
  *transactional outbox*: el evento se escribe en la misma transacción que el
  cambio de negocio y se entrega *después* del commit, de modo que no puede haber
  un evento publicado sin que su cambio haya quedado en disco, ni un cambio sin su
  evento.

Módulos clave de gokeel: `github.com/cgardev/gokeel/transaction`,
`github.com/cgardev/gokeel/eventbus`, `github.com/cgardev/gokeel/outbox`.

### 1.2 El patrón "la transacción viaja con el contexto" — el corazón del diseño

En Spring, `@Transactional` se apoya en un `ThreadLocal`
(`TransactionSynchronizationManager`) para que el método no tenga que recibir la
conexión como parámetro. gokeel reemplaza el `ThreadLocal` por el
`context.Context`: la unidad de trabajo se enlaza al contexto, y cada *store*
**pregunta** por su ejecutor en lugar de recibirlo.

Hay exactamente dos jugadores. Un caso de uso abre la unidad con `Run`; cada store
pide su ejecutor con `Querier(ctx)`:

```go
manager := transaction.NewManager(database)

_ = manager.Run(ctx, func(ctx context.Context) error {
	querier := manager.Querier(ctx) // el *sql.Tx vivo, resuelto del contexto
	_, err := querier.ExecContext(ctx,
		`INSERT INTO orders (id, customer_email) VALUES (?, ?)`, id, email)
	return err // nil hace COMMIT; un error hace ROLLBACK
})
```

Lo importante: la firma de un store **no** lleva un `*sql.Tx`, y nadie llama a
`Commit` ni a `Rollback`. La forma en que la función `work` *sale* es la decisión:

```
work devuelve nil    → COMMIT
work devuelve error  → ROLLBACK   (el error se devuelve igualmente)
work hace panic      → ROLLBACK, y el panic se relanza ya a salvo
```

`Querier(ctx)` es el equivalente de `DataSourceUtils.getConnection`: no *decides*
si estás en una transacción, lo *preguntas*, y el framework ya lo sabe. La
interfaz `Querier` es sólo `QueryContext` / `ExecContext`, así que es lo que
cualquier query builder está contento de recibir.

### 1.3 Propagación — qué hacer si ya hay una transacción

La propagación es la respuesta a una sola pregunta: **"¿qué hago si ya hay una
transacción corriendo?"**. Es la decisión que convierte varias llamadas a stores,
cada una auto-transaccional, en una sola transacción atómica: el `Run` interno
*se une* (JOINS) a la unidad de trabajo del contexto en lugar de abrir una segunda.

```go
manager.Run(ctx, work, transaction.WithPropagation(transaction.Nested))
```

| Propagación            | Ya hay una transacción                 | No hay ninguna                          |
| ---------------------- | -------------------------------------- | --------------------------------------- |
| `Required` *(default)* | se une a ella                          | abre una nueva                          |
| `Supports`             | se une a ella                          | corre sin transacción                   |
| `Mandatory`            | se une a ella                          | falla con `ErrTransactionRequired`      |
| `Never`                | falla con `ErrTransactionNotAllowed`   | corre sin transacción                   |
| `Nested`               | toma un **savepoint** de ella          | abre una nueva                          |

Una ausencia honesta: no hay `REQUIRES_NEW` ni `NOT_SUPPORTED`. Ambas necesitan
una *segunda* transacción concurrente o una suspensión, y sobre un escritor único
(SQLite) la segunda haría deadlock contra el bloqueo de escritura que la primera ya
tiene. La omisión es un ajuste deliberado al datasource, no un olvido.

### 1.4 Reglas de rollback — confirmar a pesar de un error

Spring distingue entre excepciones que abortan y excepciones que no
(`@Transactional(noRollbackFor = …, rollbackFor = …)`). gokeel reproduce ambas
direcciones, con la regla de rollback ganando cuando ambas coinciden:

```go
return manager.Run(ctx, func(ctx context.Context) error {
	// ... escribir el pedido, reservar stock (que fallen SÍ es fatal) ...
	return loyalty.Award(ctx, order.CustomerEmail, order.TotalCents) // best-effort
},
	transaction.NoRollbackForError(ErrLoyaltyServiceDown), // confirma igualmente
	transaction.RollbackForError(ErrLoyaltyFraud),         // gana y fuerza rollback
)
```

```
error devuelto por work
   ├─ ¿coincide con una RollbackForError?   ── sí ─▶ ROLLBACK  (gana)
   ├─ ¿coincide con una NoRollbackForError? ── sí ─▶ COMMIT  (el error se devuelve)
   └─ en otro caso ─────────────────────────────▶ ROLLBACK  (default)
```

Hay también formas con predicado (`NoRollbackForFunc`, `RollbackForFunc`) cuando un
error centinela no basta.

### 1.5 Sincronizaciones — engancharse al ciclo de vida

Las cuatro fases de sincronización de Spring se reproducen como una familia
`Register*`, registradas *desde dentro* del `work` y atribuidas a la unidad de
trabajo más externa:

```
work() → [before-commit: puede VETAR] → [before-completion]
                                            │
                                ┌───────────┴───────────┐
                            COMMIT                   ROLLBACK
                                │                       │
                         [after-commit]                 │   (after-commit se omite)
                                └───────────┬───────────┘
                                    [after-completion]  ← siempre, recibe el Status
```

El caballo de batalla es **after-commit**: el correo de "tu pedido está
confirmado" debe salir exactamente una vez, sólo cuando las filas estén de verdad
en disco.

```go
scheduled := transaction.RegisterAfterCommit(ctx, func(ctx context.Context) {
	// La transacción está DESLIGADA de este ctx: el trabajo de BD aquí
	// auto-commitea contra el *sql.DB. Un panic se recupera y se registra.
	_ = notifier.SendConfirmation(ctx, order.CustomerEmail, order.ID)
})
if !scheduled {
	// No había unidad de trabajo activa (camino auto-commit): hazlo ahora.
	_ = notifier.SendConfirmation(ctx, order.CustomerEmail, order.ID)
}
```

Las otras fases resuelven lo que after-commit no puede:
`RegisterBeforeCommit` corre dentro de la transacción aún abierta y puede *vetar*
(devolver un error la deshace entera); `RegisterAfterCompletion` corre en commit
*y* en rollback, recibe el `Status` final, y es el sitio para registrar el
desenlace, nunca para enviar el correo. La propagación `Nested` tiene su propio par,
`RegisterSavepoint` / `RegisterSavepointRollback`. El orden dentro de una fase lo
fija `WithOrder`, no el orden de registro.

### 1.6 Observar versus participar

gokeel separa con cuidado dos contratos que Spring también separó en la versión 6.1:

- **Sincronizaciones** (la familia `Register*`) **participan**: se registran desde
  dentro del trabajo, pertenecen a una unidad y pueden tener efectos de negocio.
- Un **`ExecutionListener`** **observa**: se entrega a `NewManager` una sola vez al
  cablear, no tiene estado, y dispara alrededor del begin/commit/rollback *físico*
  de cada transacción —es el análogo de `TransactionExecutionListener` y el sitio
  para métricas y trazas.

```go
observer := transaction.ExecutionListener{
	BeforeBegin: func(ctx context.Context, status transaction.TransactionStatus) {
		slog.InfoContext(ctx, "transaction begin", "name", status.Name())
	},
	AfterCommit: func(ctx context.Context, status transaction.TransactionStatus, err error) {
		metrics.RecordOutcome(status.Name(), "committed", err)
	},
}
manager := transaction.NewManager(database, observer)
```

Un listener dispara sólo para una transacción *físicamente* abierta: un `Run`
interno que se une, o un savepoint `Nested`, no produce callback. Un evento por
transacción real, no por `Run`.

### 1.7 El outbox de Spring Modulith

Spring Modulith decora la infraestructura de eventos del framework con un registro
persistente: el evento se guarda en la transacción del cambio y se entrega tras el
commit. gokeel hace exactamente eso componiendo `transaction` (la fase after-commit)
y `eventbus` (la entrega en memoria). El `Publisher` es el puente:

```go
publisher.Publish(ctx, OrderPlaced{ID: order.ID}) // dentro del Run del caso de uso
```

`Publish` escribe una fila de publicación por cada listener suscrito a través del
querier de la transacción en curso —de modo que las filas se unen al cambio de
negocio— y registra la entrega para *después* del commit con
`transaction.RegisterAfterCommit`. Si no hay unidad de trabajo activa, la escritura
ya auto-commiteó, así que la entrega ocurre de inmediato. La entrega es
*at-least-once*: una publicación se asienta sólo cuando su listener tiene éxito, y
las incompletas se recuperan con `ResubmitIncomplete` —por lo que los listeners
deben ser idempotentes.

---

## 2. Estado del arte en Go

Resumen del relevamiento de cómo se hacen hoy estas dos cosas en Go:

| Enfoque                 | Transacción                          | Propagación / nesting | Outbox                     | Dependencias |
|-------------------------|--------------------------------------|-----------------------|----------------------------|--------------|
| **`database/sql` crudo**| `*sql.Tx` pasado a mano              | ninguna               | a mano                     | ninguna      |
| **`sqlx` / `sqlc`**     | `*sql.Tx` pasado a mano              | ninguna               | a mano                     | mínimas      |
| **GORM**                | `db.Transaction(fn)` (closure)       | `SavePoint`/`RollbackTo` manual | hook `AfterCommit` parcial | grande |
| **ent**                 | `client.Tx(ctx)` explícito           | ninguna               | hooks, no outbox           | grande       |
| **watermill** (mensajería) | n/a                              | n/a                   | outbox como middleware     | grande       |
| **gokeel**              | `Run` declarativo, ligado a `ctx`    | 5 propagaciones + savepoints | registro persistente sobre el bus | sólo std lib en el núcleo |

El patrón dominante en Go es **el closure transaccional**: `db.Transaction(func(tx)
error { … })`. Resuelve el rollback olvidado, pero deja tres huecos:

- **La transacción sigue siendo un parámetro.** El `tx` del closure hay que
  enhebrarlo a cada método de store; no viaja con el contexto, así que una llamada
  anidada no sabe que ya hay una transacción y abre otra (o el programador la
  reenhebra a mano).
- **No hay propagación declarativa.** No existe un equivalente de `Required` /
  `Mandatory` / `Nested`; el anidamiento con savepoints se gestiona a mano si es que
  se gestiona.
- **No hay outbox de primera clase ligado al commit.** La pieza de Spring Modulith
  —escribir el evento en la transacción y entregarlo en el after-commit— no tiene un
  equivalente compacto y sin dependencias.

**El hueco real**: no existe en Go una familia de bloques que combine
*demarcación declarativa ligada al contexto* + *propagación al estilo Spring* +
*sincronizaciones de ciclo de vida* + *outbox transaccional*, todo con un **núcleo
de sólo biblioteca estándar**. Esa es exactamente la franja que gokeel ocupa.

---

## 3. Cómo lo implementa gokeel en Go

### 3.1 La unidad de trabajo en el contexto

El `Manager` posee el `*sql.DB` real y resuelve el ejecutor desde el contexto. Las
dos operaciones que un store ve están detrás de la interfaz `Transactor`:

```go
type Transactor interface {
	Run(ctx context.Context, work func(ctx context.Context) error, options ...Option) error
	Querier(ctx context.Context) Querier
}

type Querier interface {                                  // *sql.DB y *sql.Tx lo cumplen
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}
```

El store sólo depende de `Transactor`, así que recibe el `Manager` por inyección y
nunca toca el `*sql.DB` ni el `*sql.Tx`:

```go
type OrderStore struct{ transactions transaction.Transactor }

func (s *OrderStore) Insert(ctx context.Context, order Order) error {
	return s.transactions.Run(ctx, func(ctx context.Context) error {
		_, err := s.transactions.Querier(ctx).ExecContext(ctx,
			`INSERT INTO orders (id, customer_email) VALUES (?, ?)`, order.ID, order.CustomerEmail)
		return err
	})
}
```

Como la transacción vive en el contexto, un `Run` externo en el caso de uso vuelve
atómicas a varias llamadas a stores: sus `Run` internos *se unen* en vez de empezar
los suyos.

```
PlaceOrder.Do
└─ manager.Run            BEGIN  (una transacción, ligada a ctx)
   ├─ orders.Insert
   │   └─ Run  ──────────▶ JOINS  (sin BEGIN, sin COMMIT)
   ├─ inventory.Reserve
   │   └─ Run  ──────────▶ JOINS  (sin BEGIN, sin COMMIT)
   └─ return nil          COMMIT  (pedido + reservas a la vez)
```

### 3.2 Devolver un valor: `RunResult[T]`

Los métodos de Go no pueden ser genéricos, así que el `work` de `Run` sólo devuelve
`error`. Cuando el caso de uso necesita devolver el `Order` construido —con su ID y
su timestamp— se usa la función libre `RunResult[T]`, el equivalente de
`TransactionTemplate.execute` que devuelve `(T, error)`:

```go
return transaction.RunResult(ctx, transactions,
	func(ctx context.Context) (Order, error) {
		order := draft
		order.ID = uuid.NewString()
		if err := orders.Insert(ctx, order); err != nil {
			return Order{}, err // rollback: el Order devuelto es el valor cero
		}
		return order, nil
	},
	transaction.WithName("place-order"))
```

### 3.3 Afinar la transacción

El resto de los atributos de `@Transactional` caen como un pequeño menú de opciones,
que sólo importan para la transacción que *empieza* (el `Run` más externo); un `Run`
que se une hereda lo que decidió el externo:

| Opción                 | Qué hace                                                                |
| ---------------------- | ----------------------------------------------------------------------- |
| `WithPropagation(p)`   | la respuesta a "¿ya hay una transacción?" (ver 1.3)                      |
| `WithIsolation(level)` | nivel de aislamiento de una transacción nueva                           |
| `ReadOnly()`           | abre la transacción en modo sólo lectura                                |
| `WithTimeout(d)`       | cancela el contexto tras `d`; al expirar el error envuelve `ErrTransactionTimedOut`; una `d` negativa falla con `ErrInvalidTimeout` |
| `WithName(name)`       | etiqueta la unidad de trabajo, útil para logging                        |

```go
return manager.Run(ctx, placeOrderWork,
	transaction.WithName("place-order"),
	transaction.WithTimeout(2*time.Second))
```

### 3.4 Errores con nombre

Cada forma en que un `Run` puede fallar tiene nombre, y se distinguen con
`errors.Is`; las que envuelven mantienen el error del driver alcanzable con
`errors.As`:

```go
switch {
case errors.Is(err, transaction.ErrInvalidTimeout):
	// bug de configuración: timeout negativo, atrapado antes de correr nada
case errors.Is(err, transaction.ErrTransactionTimedOut):
	// la transacción reventó su propio deadline y se deshizo
case errors.Is(err, transaction.ErrRollbackOnly):
	// una llamada unida condenó la unidad, aunque su error se tragara
case errors.Is(err, transaction.ErrTransactionSystem):
	// un commit, rollback o savepoint falló (envuelve el error del driver)
case errors.Is(err, transaction.ErrBeginFailed):
	// la transacción no pudo abrirse (envuelve el error del driver)
default:
	// un error de negocio normal, p. ej. ErrOutOfStock — trátalo por sus méritos
}
```

`ErrTransactionTimedOut` es un objetivo *estable*: coincide diga lo que diga el
driver ("context canceled", "deadline exceeded"), de modo que un solo `errors.Is`
funciona en SQLite y en PostgreSQL. Una cancelación del contexto del propio llamante
*no* se reporta como timeout, para que la comprobación siga teniendo sentido.

### 3.5 El outbox: tres colaboradores

El outbox se monta con piezas pequeñas y testeables, cada una con una sola
responsabilidad. El `Registry` coordina; el `Publisher` lo liga al commit; el
`Store` persiste; el `Serializer` convierte eventos a su forma persistente; el bus
entrega.

```go
bus := eventbus.NewBus()
_ = eventbus.SubscribeTo(bus, "warehouse",
	func(ctx context.Context, e OrderPlaced) error { return pick.Enqueue(ctx, e.ID) })

serializer := outbox.NewJSONSerializer()
_ = outbox.RegisterEventType[OrderPlaced](serializer, "order.placed") // nombre estable

store := outbox.NewPostgresStore(database, outbox.CompletionModeUpdate)
_ = store.Initialize(ctx) // aplica el esquema con el NativeMigrator (sólo std lib)

registry := outbox.NewRegistry(store, bus, serializer)
publisher := outbox.NewPublisher(registry, manager) // manager satisface QuerierSource
```

`RegisterEventType[T]` ata el tipo Go a un nombre persistente estable, lo que
desacopla la representación guardada del nombre del tipo a través de refactorizaciones.
`CompletionMode` elige cómo se asienta una publicación completada
(`CompletionModeUpdate` conserva la fila, `CompletionModeDelete` la borra,
`CompletionModeArchive` la mueve a la tabla de archivo).

El ciclo de una publicación es una máquina de estados explícita
—`StatusPublished` → `StatusProcessing` → `StatusCompleted` / `StatusFailed`, con
`StatusResubmitted` para los reintentos— y el reparto concurrente se deduplica con
`ClaimProcessing`, que devuelve `false` cuando otro despachador ya tiene la fila.

### 3.6 Recuperación: el resubmitter

Como la entrega es *at-least-once*, una publicación que falló (porque su
colaborador estaba caído un momento) se queda incompleta y debe reintentarse. El
`Resubmitter` corre ese reintento en segundo plano mientras la aplicación vive,
considerando sólo publicaciones más viejas que `minimumAge` para no competir con
despachos aún en vuelo:

```go
resubmitter := outbox.NewResubmitter(registry, 30*time.Second, time.Minute)
stop := resubmitter.Start() // una pasada inmediata, luego una por intervalo
defer stop()                // cancela el bucle y espera a que termine la pasada en curso
```

Para la entrega que no debe bloquear un *request*,
`publisher.WithAsynchronousDispatch()` devuelve un `Publisher` que entrega las
publicaciones confirmadas en una goroutine de fondo; la garantía at-least-once no
cambia.

### 3.7 Migraciones del esquema sin acoplar el motor

El outbox necesita dos tablas. Por defecto las crea con sólo la biblioteca estándar:
`Store.Initialize` corre un `NativeMigrator` interno, así que el núcleo no arrastra
ningún motor de migraciones. La costura es la interfaz `Migrator`:

```go
type Migrator interface {
	Migrate(ctx context.Context, db *sql.DB, dialect Dialect, schema fs.FS) error
}
```

Quien prefiera migraciones versionadas al estilo Flyway puede optar por un
`Migrator` respaldado por [goway](https://github.com/cgardev/goway), que vive en un
**módulo separado** —así goway sólo entra en las builds que lo pidan:

```go
import (
	"github.com/cgardev/gokeel/outbox"
	"github.com/cgardev/gokeel/outbox/gowaymigrator"
)

store := outbox.NewPostgresStore(database, outbox.CompletionModeUpdate,
	outbox.WithMigrator(gowaymigrator.New()))
```

Los scripts del esquema se exponen con `outbox.Schema()`, de modo que cualquier
`Migrator` alternativo reutiliza el SQL exacto sin duplicarlo.

### 3.8 Limitaciones honestas que conviene asumir

- **Una base de datos, una transacción a la vez.** El backend principal es SQLite
  (un escritor único); también se prueba contra PostgreSQL. Las propagaciones que
  suspenden o abren una segunda transacción concurrente se dejan fuera: harían
  deadlock.
- **Contexto, no thread-locals.** La unidad de trabajo vive en el `context.Context`,
  no en un thread-local como Spring. Un `*sql.Tx` no se puede compartir entre
  goroutines, así que el trato es: `work` y sus callbacks corren en la goroutine que
  llamó a `Run`.
- **Sólo `database/sql`.** El paquete no sabe nada del query builder que usen los
  stores; `Querier` es sólo la superficie mínima `QueryContext` / `ExecContext`.
- **Métodos genéricos en Go.** Como los métodos no pueden tener parámetros de tipo,
  el `Run` que devuelve un valor (`RunResult[T]`) y el registro de tipos de evento
  (`RegisterEventType[T]`) son **funciones libres** genéricas, no métodos.
- **Divergencias deliberadas frente a Spring.** Un panic en un callback after-commit
  se traga y se registra (no se propaga), entre otras. Cada una está catalogada,
  con su porqué, en
  [`SPRING_PARITY_AUDIT.md`](https://github.com/cgardev/gokeel/blob/master/transaction/SPRING_PARITY_AUDIT.md).

---

## 4. Hoja de ruta sugerida

Lo implementado hoy (y cubierto por tests): `transaction` con `Run` declarativo,
las cinco propagaciones, savepoints/nesting, aislamiento y sólo-lectura, timeouts,
reglas de rollback y las sincronizaciones before-commit / after-commit /
after-completion; `eventbus` con registro tipado, entrega síncrona ordenada y
aislamiento de panics; `outbox` con store sobre PostgreSQL y SQLite, publicación
after-commit y un resubmitter para las entradas incompletas. Lo que queda:

1. **Releases etiquetados.** Hoy los módulos se consumen por pseudo-versión de
   commit; el objetivo es etiquetar la familia entera en lockstep, de modo que un
   solo número identifique `transaction`, `eventbus`, `outbox` y
   `outbox/gowaymigrator` a la vez.
2. **Sitio de documentación y módulos `example/` ejecutables** por biblioteca.
3. **Más backends del store del outbox** más allá de PostgreSQL y SQLite, validando
   la separación entre las sentencias y el `dialect`.
4. **Cobertura ampliada de `ExecutionListener`** para métricas y trazas de extremo a
   extremo a través de los tres módulos.
5. **Más estrategias de entrega del outbox**: backoff configurable en el
   resubmitter y particionado del reparto asíncrono.

El punto en el que esta familia supera al patrón dominante de Go desde el primer día
es la **demarcación declarativa ligada al contexto + propagación al estilo Spring +
outbox transaccional**, todo con un **núcleo de sólo biblioteca estándar**
(`transaction` y `eventbus` no tienen *ninguna* directiva `require`, propiedad
forzada en CI): ninguna combinación equivalente existe hoy en Go, y es exactamente la
razón por la que un monolito modular en gokeel se siente como Spring tipado en lugar
de como `*sql.Tx` enhebrado a mano.
