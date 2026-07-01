---
title: El Outbox Transaccional
description: Escriba eventos de dominio en la misma transacción que el cambio de negocio, luego entréguelos de manera confiable después del commit.
---

El outbox transaccional resuelve un solo problema: una escritura de negocio y los eventos que produce deben tener éxito o fallar juntos. El paquete `outbox` almacena cada evento como una fila en la transacción productora, luego lo entrega a sus listeners solo después de que esa transacción hace commit. La entrega es at-least-once (al menos una vez), por lo que los listeners deben ser idempotentes.

El paquete superpone algunos colaboradores sobre el [bus de eventos](/gokeel/es/guides/event-bus/) en memoria y el gestor de [transacciones](/gokeel/es/guides/transactions/): un `Store` que persiste publicaciones, un `Registry` que las almacena y entrega, un `Publisher` que vincula la entrega al commit, y un `Resubmitter` que recupera los rezagados.

## El Store

Un `Store` persiste publicaciones y resuelve sus resultados. Dos implementaciones se envían con el paquete, una por dialecto:

```go
import "github.com/cgardev/gokeel/outbox"

store := outbox.NewSQLiteStore(database, outbox.CompletionModeUpdate)
// or
store := outbox.NewPostgresStore(database, outbox.CompletionModeUpdate)
```

Ambos toman un `*sql.DB` abierto y un `CompletionMode`. Llame a `Initialize` una vez al inicio para actualizar el esquema; es idempotente y seguro de ejecutar en cada arranque:

```go
if err := store.Initialize(ctx); err != nil {
	return err
}
```

`Initialize` aplica los scripts de migración integrados a través de un `Migrator`. El valor predeterminado es un migrador nativo de `database/sql`, por lo que el núcleo no lleva ninguna dependencia de motor de migración. Para ejecutar el esquema a través de goway en su lugar, proporcione el adaptador del módulo opcional con `WithMigrator`:

```go
import "github.com/cgardev/gokeel/outbox/gowaymigrator"

store := outbox.NewSQLiteStore(
	database,
	outbox.CompletionModeUpdate,
	outbox.WithMigrator(gowaymigrator.New()),
)
```

## Modos de completado

`CompletionMode` selecciona cómo se resuelve una publicación una vez que su listener tiene éxito:

- `CompletionModeUpdate` conserva la fila y registra su estado y fecha de completado. Este es el valor predeterminado auditable.
- `CompletionModeDelete` elimina la fila, manteniendo la tabla pequeña.
- `CompletionModeArchive` mueve la fila a la tabla `event_publication_archive`.

```go
store := outbox.NewSQLiteStore(database, outbox.CompletionModeArchive)
```

## El Registry

Un `Registry` coordina el patrón. Almacena una publicación por listener suscrito a través del querier del llamador, entrega cada una a través del bus y resuelve el resultado en el store. Constrúyalo sobre un store, un bus de eventos y un serializador:

```go
serializer := outbox.NewJSONSerializer()
if err := outbox.RegisterEventType[OrderPlaced](serializer, "order.placed"); err != nil {
	return err
}

bus := eventbus.NewBus()
registry := outbox.NewRegistry(store, bus, serializer)
```

El `JSONSerializer` desacopla la representación almacenada de los nombres de tipos de Go: cada tipo de evento se registra bajo un nombre de cadena estable con `RegisterEventType`, por lo que una refactorización que cambie el nombre del tipo de Go no deja huérfanas las filas que ya están en el disco. Se permite registrar el mismo par dos veces; vincular un nombre o tipo que ya está vinculado de manera diferente devuelve `outbox.ErrConflictingRegistration`.

Los listeners se suscriben al bus exactamente como lo harían sin el outbox:

```go
err := eventbus.SubscribeTo(bus, "shipping", func(ctx context.Context, event OrderPlaced) error {
	return shipping.Schedule(ctx, event.OrderID)
})
```

## El Publisher

El `Publisher` es el puente entre una escritura de negocio y el registry. Escribe las filas de publicación a través del querier que el gestor de transacciones resuelve a partir del contexto, luego pospone su entrega hasta que esa transacción hace commit:

```go
manager := transaction.NewManager(database)
publisher := outbox.NewPublisher(registry, manager)
```

`NewPublisher` toma el registry y un `QuerierSource`, el cual es satisfecho por `*transaction.Manager`. Dentro de una unidad de trabajo, llame a `Publish`:

```go
err := manager.Run(ctx, func(ctx context.Context) error {
	if err := orders.Insert(ctx, order); err != nil {
		return err
	}
	return publisher.Publish(ctx, OrderPlaced{OrderID: order.ID})
})
```

`Publish` escribe una publicación por listener suscrito a través de la transacción activa, por lo que las filas y la inserción de `orders` hacen commit o rollback como una sola unidad. Luego, registra una devolución de llamada posterior al commit que las entrega. Si la transacción hace rollback, las filas desaparecen con ella y no se entrega nada.

Cuando no hay ninguna unidad de trabajo activa, la escritura de origen ya se ha confirmado automáticamente, por lo que las publicaciones se despachan inmediatamente en su lugar. De cualquier manera, un fallo en la entrega no hace fallar la llamada: las publicaciones afectadas permanecen incompletas y se recuperan más tarde.

### Despacho asíncrono

Por defecto, la entrega posterior al commit se ejecuta de forma síncrona, por lo que el llamador espera a los listeners. Para pasar las publicaciones confirmadas a una goroutine en segundo plano en su lugar, use `WithAsynchronousDispatch`:

```go
publisher := outbox.NewPublisher(registry, manager).WithAsynchronousDispatch()
```

La garantía at-least-once no cambia: las publicaciones se resuelven solo después de que su listener tiene éxito, y las incompletas se recuperan mediante el reenvío.

## Cómo se resuelve una publicación

`Registry.Dispatch` lleva una publicación de almacenada a resuelta. Primero reclama la fila con `ClaimProcessing`, una transición de estado atómica que tiene éxito para exactamente uno de varios despachadores concurrentes y, por lo tanto, deduplica la entrega. En caso de una entrega exitosa, el store registra el resultado a través de `MarkCompleted`, aplicando el modo de completado configurado:

```sql
-- CompletionModeUpdate
UPDATE event_publication SET status = ?, completion_date = ? WHERE id = ?
```

En caso de una entrega fallida, el store llama a `MarkFailed`, lo que deja la fila incompleta para un reenvío posterior en lugar de perder el evento.

## El Resubmitter

Un bloqueo o un colaborador temporalmente no disponible pueden dejar publicaciones entregadas pero no resueltas, o nunca entregadas. El `Resubmitter` las recupera volviendo a entregar cada publicación incompleta según una programación:

```go
resubmitter := outbox.NewResubmitter(registry, 30*time.Second, 1*time.Minute)
stop := resubmitter.Start()
defer stop()
```

`NewResubmitter` toma el registry, el intervalo entre pasadas y una antigüedad mínima. Solo se consideran las publicaciones más antiguas que `minimumAge`, lo que evita entrar en condiciones de carrera con despachos que aún están en curso. `Start` ejecuta una pasada inmediatamente, para recuperar los restos de una ejecución anterior, luego una pasada por intervalo; la función `stop` devuelta cancela el bucle y espera a que termine una pasada en curso.

Cada pasada restaura el evento a partir de su representación serializada, realiza la transición de la fila de vuelta a la entrega a través de `MarkResubmitted` y lo despacha de nuevo. Debido a que la entrega es at-least-once, una publicación que de hecho fue entregada antes del bloqueo se entrega una segunda vez en el reenvío, razón por la cual los listeners deben ser idempotentes.

Para ejecutar una única pasada de recuperación usted mismo en lugar de ejecutar el bucle, llame al registry directamente:

```go
// Re-deliver every incomplete publication older than one minute.
err := registry.ResubmitIncomplete(ctx, 1*time.Minute)
```

Pase una duración no positiva para considerar cada publicación incompleta independientemente de la antigüedad.

## Valores de error esperados

El serializador expone un par de errores centinela que vale la pena manejar:

- `outbox.ErrUnknownEventType`: se serializó o deserializó un evento cuyo tipo nunca se registró con `RegisterEventType`.
- `outbox.ErrConflictingRegistration`: un segundo registro intentó vincular un nombre o tipo ya utilizado a algo diferente.

El ciclo de vida de una publicación está modelado por los valores de `outbox.Status` (`StatusPublished`, `StatusProcessing`, `StatusCompleted`, `StatusFailed` y `StatusResubmitted`), los cuales el store establece a medida que resuelve cada fila.
