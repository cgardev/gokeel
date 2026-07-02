---
title: El bus de eventos
description: Registre listeners tipados, publique eventos y confíe en la entrega ordenada con aislamiento de pánico e identidad por listener en el bus de eventos síncrono en proceso.
---

Un `Bus` de gokeel es un bus de eventos síncrono en proceso. Publicar un evento invoca a cada listener coincidente en la goroutine de llamada, uno tras otro, y retorna una vez que todos se han ejecutado. El bus no tiene responsabilidades de persistencia: nunca escribe en una base de datos y no ofrece ninguna garantía de entrega más allá de la vida útil del proceso. Las bibliotecas que necesitan una entrega duradera, como el outbox, se construyen sobre él.

El bus depende únicamente de la biblioteca estándar de Go, y un `Bus` es seguro para uso concurrente. No se mantiene ningún bloqueo mientras se ejecuta un manejador, por lo que un manejador puede suscribirse o publicar de forma reentrante sin causar interbloqueos.

## Construcción de un bus

`NewBus` devuelve un bus vacío sin listeners:

```go
import "github.com/cgardev/gokeel/eventbus"

bus := eventbus.NewBus()
```

## Registro de un listener tipado

`SubscribeTo[T]` registra un listener que recibe cada evento cuyo tipo dinámico es `T`. El listener se identifica mediante un `ListenerID`, y el manejador recibe el evento ya convertido a `T`:

```go
type OrderPlaced struct {
	OrderID string
	Total   int64
}

err := eventbus.SubscribeTo(bus, "send-confirmation-email",
	func(ctx context.Context, event OrderPlaced) error {
		return mailer.SendConfirmation(ctx, event.OrderID)
	})
```

El identificador debe ser único. Suscribirse dos veces con el mismo `ListenerID` devuelve un error que envuelve a `eventbus.ErrDuplicateListener`. Un identificador vacío, o un manejador nil, también se rechaza.

## Publicación de un evento

`Publish` realiza un multicast del evento a cada listener coincidente. El tipo dinámico del valor decide qué listeners coinciden, por lo que un valor `OrderPlaced` llega a cada listener registrado con `SubscribeTo[OrderPlaced]`:

```go
err := bus.Publish(ctx, OrderPlaced{OrderID: "A-100", Total: 4999})
```

`Publish` es síncrono. Retorna solo después de que se haya ejecutado cada listener coincidente. Los listeners sin un tipo coincidente no son invocados, y un evento que no coincide con ningún listener es una operación sin efecto (no-op) que devuelve un error nil.

## Entrega ordenada

Los listeners son invocados en orden de suscripción: el listener registrado primero se ejecuta primero. `ListenersFor` expone ese orden sin realizar ninguna entrega, devolviendo los identificadores de cada listener que coincidiría con el evento:

```go
_ = eventbus.SubscribeTo(bus, "reserve-stock",  reserveStock)
_ = eventbus.SubscribeTo(bus, "send-receipt",   sendReceipt)

ids := bus.ListenersFor(OrderPlaced{OrderID: "A-100"})
// ids == []eventbus.ListenerID{"reserve-stock", "send-receipt"}
```

`Publish` recorre exactamente esta lista. Si un listener devuelve un error, los listeners restantes se siguen invocando; `Publish` recopila cada fallo y los devuelve combinados con `errors.Join`, por lo que una sola llamada presenta todos los errores a la vez.

## Aislamiento de pánico

Un manejador que entra en pánico no puede derribar al invocador que publica. El bus recupera el pánico, captura la traza de la pila (stack trace) y lo presenta como un error que envuelve a `eventbus.ErrListenerPanic`. Debido a que el pánico se convierte en un error ordinario, los listeners restantes se siguen ejecutando y el invocador observa el fallo a través del error devuelto en lugar de una caída (crash):

```go
err := bus.Publish(ctx, OrderPlaced{OrderID: "A-100"})
if errors.Is(err, eventbus.ErrListenerPanic) {
	// A listener panicked; the recovered value and stack are in err.
}
```

## Identidad de los listeners

Cada suscripción está identificada por un `ListenerID`, un tipo de cadena con nombre (named string):

```go
type ListenerID string
```

El identificador hace más que proteger contra duplicados. Permite a un invocador dirigirse directamente a un listener con `Deliver`, evitando el multicast basado en tipos que realiza `Publish`:

```go
err := bus.Deliver(ctx, "send-receipt", OrderPlaced{OrderID: "A-100"})
```

`Deliver` invoca solo al listener nombrado y lo ejecuta con la misma recuperación de pánico que `Publish`. Realizar una entrega a un identificador que no tiene ninguna suscripción detrás devuelve un error que envuelve a `eventbus.ErrUnknownListener`.

## Suscripción de bajo nivel

`SubscribeTo[T]` es un envoltorio ligero sobre `Subscribe`, que es la primitiva que expone el bus. `Subscribe` recibe un predicado `matches` que decide qué eventos recibe el listener, y un `Handler` que recibe el evento `any` sin procesar:

```go
err := bus.Subscribe("audit-everything",
	func(event any) bool { return true }, // match every event
	func(ctx context.Context, event any) error {
		return audit.Record(ctx, event)
	})
```

`SubscribeTo[T]` simplemente proporciona un predicado que coincide con el tipo dinámico y un manejador que realiza la conversión de tipo antes de llamar a su función tipada. Recurra a `Subscribe` directamente solo cuando un listener deba abarcar varios tipos de eventos o coincidir por algo distinto del tipo por sí solo.

## Dónde ir a continuación

El `Bus` es la primitiva síncrona de la historia de eventos de gokeel: el invocador espera a cada manejador y observa cada fallo. Cuando los consumidores deban tener colas propias — procesadas en orden FIFO, reintentadas de forma independiente, aparcadas como dead letters cuando siguen fallando — suscríbalos a través de [El Broker](/gokeel/es/guides/broker/), cuyo motor en memoria está construido sobre este bus y cuyo motor SQL añade durabilidad detrás de la misma interfaz. Para eventos que deben sobrevivir a un commit y ser entregados al menos una vez, consulte [El outbox transaccional](/gokeel/es/guides/transactional-outbox/), que escribe eventos dentro de la misma transacción y los publica en un bus después de que el commit se realiza con éxito. Para vincular el manejo de eventos a los ganchos del ciclo de vida de la transacción, consulte [Propagación y sincronizaciones](/gokeel/es/guides/propagation-and-synchronizations/).
