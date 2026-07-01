---
title: Outbox
description: El outbox transaccional en gokeel — Store, los almacenes SQLite y PostgreSQL, Publication, Registry, Publisher, Resubmitter y el JSON Serializer.
---

El paquete `outbox` implementa el patrón outbox transaccional: una publicación
se escribe en la misma transacción de base de datos que el cambio de negocio que produjo
el evento, se entrega a través del bus en el mismo proceso (in-process bus) después de que esa transacción hace commit,
y su resultado se consolida de vuelta en el store. La entrega es at-least-once (al menos una vez), por lo que
los listeners deben ser idempotentes. El paquete decora el
[Event Bus](/gokeel/es/reference/event-bus/) y difiere la entrega para después del commit
a través del [Transaction Manager](/gokeel/es/reference/transaction-manager/).

```go
import "github.com/cgardev/gokeel/outbox"
```

## Store

`Store` es el puerto de salida que persiste las publicaciones. `Create` recibe el
`Querier` de quien llama, por lo que la publicación se une a la transacción del cambio de
negocio; cada uno de los demás métodos se ejecuta en su propia conexión, porque los resultados se
consolidan independientemente de cualquier transacción de negocio.

```go
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
```

`Initialize` actualiza el esquema y es seguro llamarlo en cada inicio.
`ClaimProcessing` informa si quien llama obtuvo la publicación para la
entrega; devuelve `false` cuando otro despachador ya la retiene o la completó, lo
que deduplica los intentos concurrentes de despacho. `MarkResubmitted` devuelve
`false` cuando otro llamador reenvió o consolidó la publicación primero. Las
aplicaciones raramente llaman a `Store` directamente —el `Registry` lo maneja— pero la
interfaz es el punto de articulación para un backend personalizado.

`Querier` es la superficie mínima de ejecución sobre la cual se ejecutan las sentencias. Es
satisfecha por `*sql.DB`, `*sql.Tx` y `*sql.Conn`, y por lo tanto por el querier que el
transaction manager resuelve a partir del contexto.

```go
type Querier interface {
    QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
    ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}
```

## NewSQLiteStore and NewPostgresStore

Los dos almacenes integrados comparten el mismo esquema y consultas; solo el dialecto
de migración y el estilo de los marcadores de posición difieren. Cada constructor es variático en
`Option`.

```go
func NewSQLiteStore(database *sql.DB, completionMode CompletionMode, options ...Option) *SQLiteStore
func NewPostgresStore(database *sql.DB, completionMode CompletionMode, options ...Option) *PostgresStore
```

```go
database, err := sql.Open("sqlite", "app.db")
if err != nil {
    return err
}

store := outbox.NewSQLiteStore(database, outbox.CompletionModeUpdate)
if err := store.Initialize(ctx); err != nil {
    return err
}
```

Por defecto, el almacén migra el esquema con el migrador nativo de `database/sql`, por
lo que el go.mod central no lleva ninguna dependencia de motor de migración.
`WithMigrator` anula ese valor por defecto; pase `gowaymigrator.New()` desde
`github.com/cgardev/gokeel/outbox/gowaymigrator` para aplicar el esquema con goway
en su lugar. Consulte la referencia del [Schema Migrator](/gokeel/es/reference/migrator/).

```go
import "github.com/cgardev/gokeel/outbox/gowaymigrator"

store := outbox.NewPostgresStore(database, outbox.CompletionModeUpdate,
    outbox.WithMigrator(gowaymigrator.New()))
```

## CompletionMode

`CompletionMode` selecciona cómo se consolida una publicación completada mediante
`MarkCompleted`.

```go
const (
    CompletionModeUpdate  CompletionMode = iota // keep the row, recording status and completion date
    CompletionModeDelete                         // remove the row
    CompletionModeArchive                        // move the row to the archive table
)
```

`CompletionModeUpdate` conserva cada publicación para auditoría.
`CompletionModeDelete` descarta las filas completadas para mantener la tabla pequeña.
`CompletionModeArchive` las mueve a `event_publication_archive`, reteniendo el
historial sin sobrecargar la tabla activa.

## Publication

`Publication` es una entrada de outbox: la publicación de un único evento a un único
listener de destino. El `Registry` produce una por listener suscrito.

```go
type Publication struct {
    ID              uuid.UUID
    ListenerID      eventbus.ListenerID
    EventType       string
    SerializedEvent string

    // Event holds the in-memory event instance. It is populated when the
    // publication is created or deserialized for resubmission; it is never
    // persisted as such.
    Event any

    PublicationDate      time.Time
    CompletionDate       *time.Time
    Status               Status
    CompletionAttempts   int
    LastResubmissionDate *time.Time
}
```

`Status` modela el ciclo de vida de una entrada: `StatusPublished`,
`StatusProcessing`, `StatusCompleted`, `StatusFailed` y `StatusResubmitted`.

## Serializer and NewJSONSerializer

`Serializer` convierte eventos a y desde su representación persistida.
`JSONSerializer` es la implementación estándar, respaldada por `encoding/json`.

```go
type Serializer interface {
    Serialize(event any) (eventType string, payload string, err error)
    Deserialize(eventType string, payload string) (event any, err error)
}

func NewJSONSerializer() *JSONSerializer
```

Los tipos de eventos se registran con el genérico `RegisterEventType` bajo un nombre
estable, lo que desacopla la representación persistida de los nombres de tipos de Go a través de
las refactorizaciones.

```go
func RegisterEventType[T any](serializer *JSONSerializer, name string) error
```

```go
serializer := outbox.NewJSONSerializer()
if err := outbox.RegisterEventType[OrderPlaced](serializer, "order.placed"); err != nil {
    return err
}
```

Serializar o deserializar un tipo no registrado devuelve
`outbox.ErrUnknownEventType`. Vincular un nombre o un tipo que ya está vinculado a
algo diferente devuelve `outbox.ErrConflictingRegistration`; se permite registrar el
mismo par nuevamente.

## Registry and NewRegistry

`Registry` coordina el patrón: almacena una publicación por listener suscrito a través
del querier de quien llama y entrega cada publicación a través del bus. No es el propietario
de la transacción —el `Publisher` escribe dentro de una unidad de trabajo (unit of work) y difiere
la entrega para después del commit. Un `Registry` es inmutable después de su construcción y es
seguro para uso concurrente.

```go
func NewRegistry(store Store, bus EventBus, serializer Serializer) *Registry
```

`EventBus` es la sección del bus en el mismo proceso en la que confía el registro, satisfecha
por `*eventbus.Bus`.

```go
serializer := outbox.NewJSONSerializer()
registry := outbox.NewRegistry(store, bus, serializer)
```

`Publish` almacena una publicación del evento para cada listener suscrito,
escribiendo a través del querier proporcionado para que las publicaciones se unan a la
transacción de negocio; las publicaciones devueltas quedan pendientes y deben entregarse a
`Dispatch` después de que la transacción haga commit. `Dispatch` entrega cada publicación
y consolida el resultado —completado en caso de éxito, fallido en caso contrario.
`ResubmitIncomplete` vuelve a entregar cada publicación incompleta, restaurando el
evento a partir de su forma serializada; un `olderThan` positivo considera solo las
publicaciones publicadas antes de ese tiempo de antigüedad, lo que evita entrar en condiciones de
carrera contra despachos que aún están en curso.

```go
func (r *Registry) Publish(ctx context.Context, querier Querier, event any) ([]Publication, error)
func (r *Registry) Dispatch(ctx context.Context, publications ...Publication) error
func (r *Registry) ResubmitIncomplete(ctx context.Context, olderThan time.Duration) error
```

## Publisher and NewPublisher

`Publisher` es el puente entre una escritura de negocio y el registro: almacena
las publicaciones dentro de la unidad de trabajo (unit of work) actual y las entrega solo después de que
esa unidad hace commit. Cuando no hay ninguna unidad de trabajo activa, la escritura de origen
ya ha hecho commit, por lo que las publicaciones se despachan inmediatamente. Un `Publisher`
es inmutable después de su construcción y es seguro para uso concurrente.

```go
func NewPublisher(registry *Registry, querier QuerierSource) *Publisher
```

`QuerierSource` resuelve el querier a través del cual se escriben las filas; es
satisfecho por `*transaction.Manager`, cuyo `Querier` devuelve la transacción activa
cuando una unidad de trabajo está en progreso.

```go
publisher := outbox.NewPublisher(registry, manager)

err := manager.Run(ctx, func(ctx context.Context) error {
    if err := placeOrder(ctx, order); err != nil {
        return err
    }
    // The publication joins this transaction; delivery is deferred to commit.
    return publisher.Publish(ctx, OrderPlaced{ID: order.ID})
})
```

`Publish` no hace fallar la llamada ante un error de entrega: las publicaciones afectadas
permanecen incompletas y se recuperan mediante `ResubmitIncomplete`.
`WithAsynchronousDispatch` devuelve un `Publisher` que entrega las publicaciones confirmadas (committed)
a una goroutine en segundo plano, de modo que quienes llaman en una ruta de solicitud no esperan
por listeners lentos; la garantía at-least-once (al menos una vez) no cambia.

```go
func (p *Publisher) WithAsynchronousDispatch() *Publisher
```

```go
publisher := outbox.NewPublisher(registry, manager).WithAsynchronousDispatch()
```

## Resubmitter and NewResubmitter

`Resubmitter` vuelve a entregar periódicamente las publicaciones incompletas, de modo que una entrega
que falló contra un colaborador temporalmente no disponible se reintenta mientras la
aplicación se ejecuta en lugar de esperar a un reinicio.

```go
func NewResubmitter(registry *Registry, interval, minimumAge time.Duration) *Resubmitter
```

Ejecuta una pasada de reenvío por `interval`, considerando solo las publicaciones
más antiguas que `minimumAge` para no entrar en condiciones de carrera con despachos que aún están en curso.
`Start` inicia el bucle en segundo plano —una pasada de inmediato, que recupera los
remanentes de una ejecución anterior, luego una pasada por intervalo— y devuelve una función de
parada (stop) que cancela el bucle y espera a que termine una pasada en curso.

```go
func (r *Resubmitter) Start() (stop func())
```

```go
resubmitter := outbox.NewResubmitter(registry, 30*time.Second, time.Minute)
stop := resubmitter.Start()
defer stop()
```

Para un tutorial de extremo a extremo que conecta estas piezas, consulte la guía de
[Transactional Outbox](/gokeel/es/guides/transactional-outbox/).
