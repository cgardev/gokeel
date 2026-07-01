---
title: Event Bus
description: Construcción de un Bus, suscripción de listeners por identificador o por tipo de evento, y publicación de eventos de forma síncrona en memoria.
---

El paquete `eventbus` proporciona un bus de eventos (event bus) en memoria, síncrono y genérico. No conlleva ninguna preocupación de persistencia: las bibliotecas que necesitan garantías de entrega (delivery), como el paquete [outbox](/gokeel/es/reference/synchronizations/), se construyen sobre él. Un `Bus` es seguro para uso concurrente, y no se retiene ningún lock mientras se ejecuta un handler, por lo que un handler puede suscribirse o publicar de manera reentrante sin bloqueos mutuos (deadlocking).

```go
import "github.com/cgardev/gokeel/eventbus"
```

## NewBus

`NewBus` construye un `Bus` vacío. No se requieren opciones, y el valor cero detrás del puntero devuelto está inmediatamente listo para suscripciones.

```go
func NewBus() *Bus
```

```go
bus := eventbus.NewBus()
```

Se comparte un único `Bus` en toda la aplicación; entregue eventos a través de sus métodos en lugar de construir uno por cada sitio de llamada.

## ListenerID

Un `ListenerID` nombra a un listener suscrito. Los identificadores son únicos dentro de un `Bus`, por lo que un llamador puede dirigirse a un listener individualmente o realizar un multicast de un evento a cada listener coincidente.

```go
type ListenerID string
```

```go
const billing eventbus.ListenerID = "billing"
```

## SubscribeTo

`SubscribeTo` es el punto de entrada genérico: registra un listener que recibe cada evento cuyo tipo dinámico sea `T`. Se llama al handler con el evento ya tipado, por lo que no se necesita ninguna aserción dentro de él.

```go
func SubscribeTo[T any](bus *Bus, id ListenerID, handle func(ctx context.Context, event T) error) error
```

```go
type OrderPlaced struct {
    OrderID string
    Total   int64
}

err := eventbus.SubscribeTo(bus, "billing", func(ctx context.Context, event OrderPlaced) error {
    return charge(ctx, event.OrderID, event.Total)
})
```

Suscribirse dos veces bajo el mismo `ListenerID` devuelve `ErrDuplicateListener`. Esta es la función de registro a la que se debe recurrir en casi todos los casos; `Subscribe` existe para listeners que coinciden en algo que no sea el tipo de evento.

## Subscribe

`Subscribe` es la función de registro de nivel inferior. El predicado `matches` decide qué eventos recibe el listener, y `handle` procesa cada uno de ellos. Un `Handler` es `func(ctx context.Context, event any) error`.

```go
func (b *Bus) Subscribe(id ListenerID, matches func(event any) bool, handle Handler) error
```

```go
err := bus.Subscribe("audit",
    func(event any) bool { return true }, // every event
    func(ctx context.Context, event any) error {
        return record(ctx, event)
    })
```

El identificador no debe estar vacío, y tanto `matches` como `handle` deben ser no nulos; de lo contrario, `Subscribe` devuelve un error. `SubscribeTo` está implementado en términos de `Subscribe`, suministrando un predicado de aserción de tipo y un handler que desenvuelve el evento a `T`.

## Publish

`Publish` realiza un multicast del evento a cada listener coincidente, en orden de suscripción. El error devuelto une las fallas de cada listener que rechazó el evento, y los listeners restantes aún se invocan, por lo que una falla no suprime las entregas posteriores.

```go
func (b *Bus) Publish(ctx context.Context, event any) error
```

```go
err := bus.Publish(ctx, OrderPlaced{OrderID: "A-1", Total: 4200})
```

Debido a que la entrega es síncrona, `Publish` devuelve solo después de que se haya ejecutado cada handler coincidente. Inspeccione el error unido con `errors.Is` para detectar un modo de falla específico, por ejemplo, un handler que entra en pánico (panicking).

## ListenersFor

`ListenersFor` devuelve los identificadores de cada listener suscrito al evento, en orden de suscripción, sin entregar nada. Responde a qué listeners alcanzaría un `Publish`.

```go
func (b *Bus) ListenersFor(event any) []ListenerID
```

```go
ids := bus.ListenersFor(OrderPlaced{}) // e.g. []eventbus.ListenerID{"billing", "audit"}
```

## Deliver

`Deliver` entrega el evento a un único listener nombrado en lugar de realizar un multicast. Un handler que entra en pánico se recupera y se reporta como un error que envuelve a `ErrListenerPanic`, por lo que un listener con mal comportamiento no puede derribar al llamador que publica.

```go
func (b *Bus) Deliver(ctx context.Context, id ListenerID, event any) error
```

```go
err := bus.Deliver(ctx, "billing", OrderPlaced{OrderID: "A-1", Total: 4200})
```

Entregar a un identificador sin ninguna suscripción detrás devuelve `ErrUnknownListener`. `Publish` se construye sobre `Deliver`: resuelve los identificadores coincidentes con `ListenersFor` y entrega a cada uno a su vez.

## Errors

El paquete exporta tres errores centinela, cada uno de ellos emparejable con `errors.Is`:

- `ErrDuplicateListener` reporta un `Subscribe` bajo un identificador que ya está tomado.
- `ErrUnknownListener` reporta un `Deliver` hacia un identificador sin ninguna suscripción detrás de él.
- `ErrListenerPanic` reporta un handler que entró en pánico mientras procesaba un evento; el pánico se recupera y sale a la superficie a través de este error, con la traza de la pila capturada adjunta.

```go
if err := bus.Publish(ctx, event); errors.Is(err, eventbus.ErrListenerPanic) {
    // a handler panicked; the others still ran
}
```
