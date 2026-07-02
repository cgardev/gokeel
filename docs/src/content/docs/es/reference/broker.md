---
title: Broker
description: El contrato Broker independiente del motor — Consume, Publish, opciones de consumidor, dead letters — y los constructores de los motores en memoria y SQL.
---

La interfaz `Broker` es el contrato de consumidor independiente del motor de gokeel: exactamente una entrega (delivery) por consumidor, un presupuesto de reintentos independiente por consumidor, ordenación FIFO por defecto con la posibilidad de optar por la entrega no ordenada, y dead letters para eventos agotados. El paquete `eventbus` define el contrato y proporciona el motor en memoria; el módulo `sqlbus` implementa el mismo contrato con persistencia y consumo entre nodos.

```go
import "github.com/cgardev/gokeel/eventbus"
```

## Broker

```go
type Broker interface {
    Publish(ctx context.Context, event any) error
    Subscribe(ctx context.Context, registration ConsumerRegistration) error
    FindExhausted(ctx context.Context, limit int) ([]DeadLetter, error)
    Resubmit(ctx context.Context, reference string) (bool, error)
}
```

`Publish` entrega el evento a la cola de cada consumidor coincidente e informa solo de fallos de validación y persistencia; los resultados del manejador se resuelven de forma asíncrona a través de los reintentos por consumidor y se exponen como dead letters. Los motores en proceso entregan exactamente una vez por consumidor durante la vida útil del proceso; los motores persistentes resuelven cada entrega exactamente una vez pero pueden ejecutar un manejador de nuevo después de una caída o un claim expirado, por lo que los manejadores deben ser idempotentes cuando la durabilidad está en juego.

## Consume

`Consume` es la puerta de entrada tipada del contrato: registra un consumidor de cada evento de tipo `T`, con ordenación FIFO y el presupuesto de reintentos por defecto a menos que las opciones digan lo contrario.

```go
func Consume[T any](ctx context.Context, broker Broker, id ListenerID,
    handle func(ctx context.Context, event T) error, options ...ConsumerOption) error
```

```go
err := eventbus.Consume(ctx, broker, "send-confirmation",
    func(ctx context.Context, event OrderPlaced) error {
        return mailer.SendConfirmation(ctx, event.OrderID)
    },
    eventbus.WithMaximumAttempts(8))
```

El identificador debe ser único dentro del broker. En el motor SQL, el tipo de evento del consumidor debe estar registrado en el serializador del bridge.

## Opciones de consumidor

| Opción | Efecto | Por defecto |
| --- | --- | --- |
| `WithUnorderedDelivery()` | Procesa eventos de forma concurrente; un evento fallido se reintenta sin retrasar a los demás. | FIFO |
| `WithWorkers(n)` | Concurrencia de un consumidor no ordenado. El motor SQL no lo interpreta: su concurrencia se rige por el tamaño de lote del dispatcher y los nodos de alojamiento. | 8 |
| `WithMaximumAttempts(n)` | Intentos de entrega que un evento puede consumir antes de aparcarse como dead letter. | 5 |
| `WithRetryDelay(f)` | Plan de reintentos; `f` recibe el recuento de intentos gastados. | 5 s duplicándose hasta 5 min |
| `WithBroadcastDelivery()` | Una entrega por nodo de aplicación en lugar de una por consumidor. Los motores en proceso lo tratan como consumo regular. | competing |

Un consumidor FIFO procesa un evento a la vez, en orden de publicación; un evento fallido bloquea a sus sucesores mientras se reintenta, y un evento agotado se aparca como dead letter mientras la cola continúa.

## DeadLetter

Una `DeadLetter` describe un evento cuya entrega consumió su presupuesto de intentos para un consumidor.

```go
type DeadLetter struct {
    Reference       string          // opaque handle for Resubmit
    ListenerID      ListenerID      // the consumer whose delivery exhausted
    Event           any             // the parked event, when restorable
    Attempts        int             // delivery attempts consumed
    LastError       string          // failure of the final attempt
    PublicationDate time.Time
}
```

`FindExhausted` devuelve dead letters del más antiguo al más reciente, hasta el límite — por fecha de publicación en el motor SQL, por el orden en que se aparcaron en el motor en memoria. `Resubmit` otorga al dead letter referenciado un presupuesto de intentos fresco y devuelve false cuando la referencia es desconocida o ya fue reenviada. Un evento revivido vuelve a entrar en su cola en la posición que dicta su orden de publicación original.

## El motor en memoria

```go
func NewMemoryBroker() *MemoryBroker
func (b *MemoryBroker) Stop()
```

`NewMemoryBroker` construye el motor en proceso: una cola por consumidor, nada persistido, exactamente una vez por consumidor durante la vida útil del proceso. `Stop` cancela los workers, espera a que terminen las entregas en curso y descarta eventos encolados y reintentos pendientes. `Publish`, `Subscribe`, y `Resubmit` después de `Stop` devuelven `ErrBrokerStopped`; `FindExhausted` sigue devolviendo los dead letters registrados para inspección.

## El motor SQL

```go
// package sqlbus
func NewBroker(bridge *Bridge, publisher *Publisher) *Broker
```

`sqlbus.NewBroker` adapta la maquinaria durable al mismo contrato: `Publish` pasa a través del `Publisher` transaccional, por lo que el evento se une a la unidad de trabajo del llamador, y los consumidores se adjuntan durablemente a través del `Bridge` — compitiendo en todo el clúster por defecto, broadcast por nodo bajo solicitud. El llamador mantiene un `Dispatcher` en ejecución por nodo, exactamente como con la API de nivel inferior; consulte la [guía del bus SQL](/gokeel/es/guides/sql-bus/).

Los consumidores ordenados en el motor SQL reclaman solo la cabeza de su cola, por debajo del watermark (marca de agua) de materialización, por lo que su latencia de entrega es al menos la gracia de materialización del bridge.

## Errores

| Error | Significado |
| --- | --- |
| `eventbus.ErrDuplicateListener` | El identificador del consumidor ya está registrado. |
| `eventbus.ErrBrokerStopped` | El broker en memoria fue detenido. |
| `sqlbus.ErrConflictingDeliveryMode` | Otro nodo registró el listener bajo un modo de entrega diferente. |
| `sqlbus.ErrConflictingOrdering` | Otro nodo registró el listener bajo una ordenación diferente. |

## Conformidad

El conjunto bajo `eventbus/bustest` ejercita cualquier implementación de `Broker` contra el contrato — fan-out, FIFO bajo reintentos, concurrencia no ordenada, reintentos independientes, dead letters y reenvío. Ambos motores incluidos lo ejecutan; un motor alternativo también debería hacerlo.
