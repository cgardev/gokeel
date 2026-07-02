---
title: El Broker
description: Un contrato de consumidor sobre dos motores — colas FIFO por consumidor, reintentos independientes con dead letters, en memoria o duradero sobre SQL — y cómo elegir y combinarlos.
---

Un `Broker` de gokeel es un bus de mensajes definido por un contrato, no por un motor. Cada evento publicado llega **exactamente una vez por consumidor a cada consumidor coincidente**; cada consumidor posee **su propia cola** y **sus propios reintentos**; la cola es **FIFO de manera predeterminada** y concurrente bajo solicitud; un evento que agota su presupuesto de reintentos se aparca como un **dead letter** inspectable en lugar de desaparecer.

Dos motores implementan el contrato detrás de la misma interfaz:

| | `eventbus.NewMemoryBroker()` | `sqlbus.NewBroker(...)` |
| --- | --- | --- |
| Reside en | la memoria del proceso | PostgreSQL o SQLite |
| Sobrevive a un reinicio | no | sí |
| Llega a otros nodos | no | sí — la base de datos es el transporte |
| La publicación se une a la transacción | no | sí |
| Exactamente una entrega por consumidor | durante la vida útil del proceso | se liquida exactamente una vez; una caída puede reejecutar un manejador¹ |

¹ La durabilidad tiene un precio honesto: después de una caída o un claim expirado, el motor SQL ejecuta el manejador de nuevo antes de liquidar, por lo que sus manejadores deben ser idempotentes. La interfaz, el orden, los reintentos y los dead letters son idénticos de cualquier manera — el código escrito contra el contrato no cambia cuando el motor cambia.

## El contrato en un ejemplo

`Consume[T]` registra un consumidor tipado; `Publish` entrega el evento a la cola de cada consumidor cuyo tipo coincida. `Publish` solo reporta fallos de validación y persistencia — los resultados del manejador se resuelven de forma asíncrona a través de los reintentos de cada consumidor y nunca se exponen a través de `Publish`:

```go
import "github.com/cgardev/gokeel/eventbus"

type OrderPlaced struct {
	OrderID string
}

broker := eventbus.NewMemoryBroker()
defer broker.Stop()

err := eventbus.Consume(ctx, broker, "send-confirmation",
	func(ctx context.Context, event OrderPlaced) error {
		return mailer.SendConfirmation(ctx, event.OrderID)
	})

err = broker.Publish(ctx, OrderPlaced{OrderID: "A-100"})
// Returns immediately; "send-confirmation" processes A-100 exactly once.
```

Un error del manejador no es responsabilidad del publicador: es el comienzo de la historia de reintentos de ese consumidor, y de nadie más.

## FIFO, y lo que sostiene bajo fallo

De manera predeterminada, un consumidor procesa sus eventos **estrictamente en orden de publicación, uno a la vez**. Esa promesa es fácil de mantener en el camino feliz; el broker la mantiene bajo fallo también — un evento que falla *bloquea a sus sucesores* mientras se reintenta, porque entregarlos primero rompería el orden:

```go
var seen []string
_ = eventbus.Consume(ctx, broker, "ledger",
	func(ctx context.Context, event OrderPlaced) error {
		if event.OrderID == "A-2" && !ledger.Ready() {
			return errors.New("ledger not ready") // A-2 will retry...
		}
		seen = append(seen, event.OrderID)
		return nil
	})

for _, id := range []string{"A-1", "A-2", "A-3"} {
	_ = broker.Publish(ctx, OrderPlaced{OrderID: id})
}
// seen is always [A-1 A-2 A-3] — A-3 waited for A-2's retries.
```

El bloqueo de cabeza de línea es la definición de FIFO, no un defecto: el orden y "los fallos nunca retrasan a nadie" no pueden ambos sostenerse. Cuando un evento agota su presupuesto, la cola no permanece cautiva — el evento se aparca como un dead letter y los sucesores continúan.

## No ordenado, cuando el rendimiento gana

Los consumidores cuyos eventos son independientes optan por no ordenarse y procesan concurrentemente. Un evento que falla se reintenta según su propio plan de reintentos mientras los otros siguen fluyendo:

```go
err := eventbus.Consume(ctx, broker, "thumbnails",
	func(ctx context.Context, event ImageUploaded) error {
		return thumbnails.Render(ctx, event.Path)
	},
	eventbus.WithUnorderedDelivery(),
	eventbus.WithWorkers(16))
```

Ambos tipos coexisten en un broker: el consumidor ledger anterior permanece FIFO mientras que las miniaturas se expanden — la ordenación es una propiedad de cada consumidor, no del bus.

## Reintentos y dead letters

Cada consumidor lleva su propio presupuesto de intentos (5 por defecto) y plan de reintentos (5 segundos duplicándose a 5 minutos), ajustable por consumidor:

```go
err := eventbus.Consume(ctx, broker, "sync-crm",
	func(ctx context.Context, event OrderPlaced) error {
		return crm.Upsert(ctx, event.OrderID)
	},
	eventbus.WithMaximumAttempts(8),
	eventbus.WithRetryDelay(func(attempt int) time.Duration {
		return time.Duration(attempt) * time.Second
	}))
```

Un evento que consume su presupuesto completo se convierte en un dead letter: aparcado, inspectable y revivible. La cola continúa sin él.

```go
letters, err := broker.FindExhausted(ctx, 100)
for _, letter := range letters {
	slog.Warn("delivery exhausted",
		"consumer", letter.ListenerID, "cause", letter.LastError)
	revived, err := broker.Resubmit(ctx, letter.Reference)
	// revived == true: the event re-enters its queue with a fresh budget.
}
```

## El mismo código, duradero y multi-nodo

Nada anterior nombró un motor. Para que los eventos sobrevivan a reinicios y lleguen a otros nodos, construya el broker sobre sqlbus en su lugar — los consumidores y las publicaciones permanecen exactamente como estaban escritos:

```go
import "github.com/cgardev/gokeel/sqlbus"

broker := sqlbus.NewBroker(bridge, publisher)

err := eventbus.Consume(ctx, broker, "send-confirmation",
	func(ctx context.Context, event OrderPlaced) error {
		return mailer.SendConfirmation(ctx, event.OrderID)
	})
```

El motor SQL añade lo que solo la durabilidad puede ofrecer:

- **La publicación se une a la transacción de negocio.** Un rollback se lleva el evento con él; un commit garantiza entrega eventual. Consulte [El bus SQL](/gokeel/es/guides/sql-bus/) para la maquinaria debajo.
- **Los consumidores compiten entre nodos.** Un consumidor FIFO permanece estrictamente serial en todo el clúster: un evento a la vez, en orden de publicación, sin importar cuántos nodos lo alojen. Un consumidor no ordenado reparte su trabajo por todo el clúster.
- **Entrega broadcast (difusión).** `eventbus.WithBroadcastDelivery()` solicita una entrega por nodo en lugar de una por consumidor — la forma natural para preocupaciones locales del nodo como invalidar una caché en memoria. El motor en memoria lo trata como consumo regular, porque el proceso y el nodo coinciden.

Las entregas ordenadas en el motor SQL esperan por debajo de un watermark (marca de agua) — la gracia de materialización, 10 minutos por defecto y ajustable con `sqlbus.WithMaterializationGrace` — antes de ejecutarse, por lo que una publicación todavía dentro de una transacción abierta nunca puede colocarse delante de un sucesor ya entregado. El orden cuesta esa latencia; los consumidores no ordenados no la pagan.

## Elegir un motor

- **Memoria** — eventos que solo coordinan el proceso actual: actualizar cachés, notificar sesiones WebSocket, desacoplar módulos en pruebas. Cero dependencias, cero infraestructura, exactly-once (exactamente una vez) durante la vida útil del proceso, y todo se pierde en el apagado por diseño.
- **SQL** — eventos que representan hechos de negocio: deben sobrevivir a un fallo, unirse a la transacción que los produjo y llegar a cada nodo. El coste es manejadores idempotentes y latencia de polling.

Enrute cada tipo de evento a través de un broker. El contrato es el mismo en ambos, por lo que promover un tipo de evento de memoria a duradero es un cambio de constructor, no una reescritura.

## Dónde ir a continuación

El broker se monta sobre dos piezas de nivel inferior que permanecen disponibles por su cuenta: [El bus de eventos](/gokeel/es/guides/event-bus/) es la primitiva síncrona en proceso a través de la cual entregan los motores, y [El bus SQL](/gokeel/es/guides/sql-bus/) documenta la maquinaria duradera — claims, leases, la marca de agua, retención y operaciones multi-nodo. La superficie completa de la API está en [La referencia de Broker](/gokeel/es/reference/broker/).
