---
title: El bus SQL
description: Qué tipo de bus es sqlbus, el orden que garantiza y el que no, a cuántos listeners da soporte a través de los nodos y qué sucede cuando falla una entrega.
---

`sqlbus` extiende el bus de eventos en proceso a través de los nodos de la aplicación, utilizando una base de datos SQL compartida — PostgreSQL o SQLite — como único transporte. Un evento publicado en un nodo llega a los listeners conectados en cualquier nodo del clúster, sin ningún broker ni cola de mensajes en la infraestructura: la base de datos en la que la aplicación ya escribe es el bus.

Es el hermano distribuido del [outbox transaccional](/gokeel/es/guides/transactional-outbox/), y hace la misma promesa central en un ámbito más amplio: allí donde el outbox garantiza que los listeners de un proceso finalmente manejan un evento, sqlbus garantiza que un evento llega a sus listeners en cualquier lugar del clúster donde se ejecuten.

## Qué tipo de bus es

sqlbus es un **bus basado en claims (reclamaciones), de entrega at-least-once (al menos una vez) y store-and-forward (almacenamiento y reenvío)**. La publicación escribe el evento como una fila de mensaje persistente — dentro de la transacción de negocio del llamador cuando una unidad de trabajo está activa — y cada nodo ejecuta un `Dispatcher` que materializa una fila de entrega por mensaje y listener que aloja, realiza un claim de cada entrega con una actualización protegida que la base de datos arbitra, entrega el evento al bus local en proceso y resuelve el resultado.

Dos consecuencias se derivan de ese diseño:

- **La entrega es at-least-once (al menos una vez), nunca exactly-once (exactamente una vez).** Una caída entre la invocación de un listener y el registro de su éxito conduce a una nueva entrega una vez que expira el lease del claim. Los listeners deben ser idempotentes; la [guía de outbox](/gokeel/es/guides/transactional-outbox/) hace la misma exigencia por la misma razón.
- **La latencia varía según la localidad.** Los listeners conectados en el nodo de publicación se ejecutan sincrónicamente justo después del commit, exactamente igual que los listeners del outbox. A los listeners de otros nodos se llega asincrónicamente mediante el próximo polling de su dispatcher, por lo que la entrega remota llega aproximadamente dentro de un intervalo de polling (un segundo por defecto), o antes si se configura una señal de wake.

## ¿Es FIFO?

**Por listener, la entrega se ordena dando prioridad a los más antiguos como un esfuerzo máximo (best effort); no es una cola FIFO estricta.** Un dispatcher siempre realiza claims de las entregas vencidas de un listener en orden de fecha de publicación, por lo que bajo un único nodo consumidor, sin fallos y con relojes sincronizados, un listener observa los eventos en el orden en que fueron publicados. Cuatro mecanismos — cada uno de ellos deliberado — pueden alterar ese orden:

- **Los reintentos no bloquean la cola.** Una entrega fallida espera a que pase su backoff mientras los nuevos eventos siguen fluyendo. No hay bloqueo de cabeza de línea (head-of-line blocking): el precio es que el evento reintentado llega más tarde que los eventos publicados después de él.
- **Varios nodos comparten el trabajo.** Cuando un listener de tipo competing (competitivo) está conectado en más de un nodo, diferentes eventos se reclaman y procesan de forma concurrente, y nada serializa su finalización.
- **La ruta rápida local se ejecuta primero.** El nodo de publicación entrega a sus propios listeners inmediatamente después del commit, por lo que un evento publicado localmente puede adelantar a uno remoto más antiguo que todavía está esperando al siguiente polling.
- **Las fechas de publicación provienen de los relojes de los publicadores.** El orden a través de los nodos es tan bueno como la sincronización de los relojes del clúster.

Diseñe los listeners de modo que la corrección no dependa de un orden estricto — at-least-once ya los obliga a ser idempotentes, y la independencia del orden es su compañera natural. Cuando una carga de trabajo realmente necesite un orden estricto por clave, enrútela a través de un único nodo de tipo competing (competitivo) y trate un fallo de entrega como una señal de parada de línea en lugar de dejar pasar los eventos más nuevos.

## ¿Cuántos listeners se pueden suscribir?

**Cualquier número — dos y mucho más.** Al igual que el bus en proceso, sqlbus realiza multicast: cada listener conectado a un tipo de evento recibe su propia entrega de cada mensaje, rastreada como su propia fila con su propia máquina de estados. Dos listeners nunca comparten un resultado; que uno de ellos falle no afecta a la entrega del otro.

Lo que hace que el bus distribuido sea más rico es que "dos suscriptores" puede significar dos cosas diferentes, y cada una se declara explíciamente:

**Dos listeners diferentes** reciben ambos cada evento, dondequiera que se ejecuten:

```go
// On any node: both listeners get every OrderPlaced, independently.
err := sqlbus.AttachCompetingListener(ctx, bridge, "billing",
	func(ctx context.Context, event OrderPlaced) error {
		return billing.Invoice(ctx, event.OrderID)
	})
err = sqlbus.AttachCompetingListener(ctx, bridge, "analytics",
	func(ctx context.Context, event OrderPlaced) error {
		return analytics.Record(ctx, event.OrderID)
	})
```

**El mismo listener en dos nodos** comparte o distribuye (fan out) el trabajo, dependiendo de su modo de entrega:

- `AttachCompetingListener` registra al listener como un único consumidor en todo el clúster: cada evento se maneja **exactamente una vez en algún lugar**, por cualquier nodo de alojamiento que realice el claim primero. Este es el valor por defecto seguro para réplicas homogéneas: escalar de un nodo a tres no debe enviar tres facturas.
- `AttachBroadcastListener` registra un consumidor por nodo: cada evento se maneja **una vez en cada nodo de alojamiento**, lo que se adapta a preocupaciones locales del nodo, como invalidar una caché en memoria.

El modo de entrega se arbitra en la base de datos con una semántica de tipo "el primero que se conecta gana" (first-attach-wins). Dos nodos no pueden ejecutar silenciosamente el mismo `ListenerID` bajo diferentes modos: la conexión perdedora falla con un error que envuelve a `sqlbus.ErrConflictingDeliveryMode`, por lo que un despliegue mal configurado se anuncia al iniciar en lugar de procesar dos veces cada evento.

## ¿Qué sucede cuando falla un listener?

Un listener que falla afecta exactamente a una entrega: la suya propia. Los demás listeners del mismo evento, y los demás eventos del mismo listener, proceden sin alteración. La entrega fallida recorre entonces una máquina de estados explícita:

1. **Se registra el fallo.** La entrega pasa a `FAILED`, llevando el texto del error del listener en su columna `last_error`, de modo que una entrega atascada es diagnosticable con un solo `SELECT` en lugar de arqueología de logs. Un listener que entra en pánico (panic) se recupera y se trata como un fallo ordinario: un manejador que explota no puede derribar al dispatcher ni al llamador de la publicación.
2. **Se reintenta con backoff, por cualquier nodo de alojamiento.** La entrega vuelve a estar vencida después de un retraso que se duplica por intento (de 5 segundos hasta 5 minutos por defecto, ajustable con `WithRetryDelay`), y cualquier nodo que aloje al listener y que realice el claim primero ejecuta el reintento: un listener roto por las condiciones locales de un nodo puede tener éxito en otro.
3. **Después del presupuesto de intentos, se convierte en un dead letter.** Una vez agotados los intentos configurados (5 por defecto, `WithMaximumAttempts`), la entrega pasa al estado terminal `EXHAUSTED` y deja de consumir recursos. Los dead letters fijan su mensaje en el almacén — el payload sigue estando disponible — y se listan para el operador:

   ```go
   deadLetters, err := bridge.FindExhausted(ctx, 100)
   for _, dead := range deadLetters {
   	// dead.Delivery.LastError holds the final failure cause.
   	revived, err := bridge.Resubmit(ctx, dead.Delivery.Key)
   	// revived == true: the delivery re-enters the queue with a fresh budget.
   }
   ```

4. **Un nodo caído no puede dejar varada una entrega.** Una entrega reclamada por un nodo que muere a mitad del procesamiento permanece protegida solo durante el lease del claim (5 minutos por defecto, `WithLeaseDuration`); pasado este tiempo, cualquier otro nodo de alojamiento roba el claim y vuelve a entregar. Este camino de recuperación es también la razón por la que la entrega es at-least-once (al menos una vez): la caída puede haber ocurrido después de que el listener se ejecutara pero antes de que se registrara su éxito.

Los fallos nunca hacen fallar al publicador. `Publisher.Publish` devuelve un error solo cuando el evento no se puede almacenar; un listener que rechaza el evento posteriormente se reporta a través de los logs y de la tabla de entregas, y es manejado por el mecanismo de reintentos en lugar de por la ruta de código que produjo el evento.

## Dónde ir a continuación

La mitad en proceso de la historia — el bus a través del cual sqlbus realiza las entregas en cada nodo — se describe en [El bus de eventos](/gokeel/es/guides/event-bus/). Para el antecesor de proceso único del mismo patrón almacenar-entregar-resolver, consulte [El outbox transaccional](/gokeel/es/guides/transactional-outbox/); enrute cada tipo de evento a través de uno de los dos, nunca de ambos, o sus listeners lo procesarán dos veces. El esquema que ambos módulos gestionan a través de la misma costura de migración se trata en [Migraciones de esquemas](/gokeel/es/guides/schema-migrations/).
