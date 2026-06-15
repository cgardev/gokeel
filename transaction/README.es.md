# transaction

Conoces el baile. Un caso de uso necesita tocar dos tablas que deben cambiar
juntas — crear un pedido *y* reservar su stock — así que tiras de una transacción.
Con `database/sql` a pelo eso significa abrir un `*sql.Tx`, pasarlo a cada método
de store, y acordarte de hacer rollback en cada `return` temprano. Te olvidas de
uno y no hay error de compilación: hay una transacción que se queda abierta,
reteniendo la base de datos, hasta que el driver recoge la conexión.

Este paquete te borra ese trabajo de contabilidad. Es una **unidad de trabajo
ligada al contexto** — la versión en Go del `@Transactional` de Spring, para una
sola base de datos. Abres la transacción una vez, tus stores la recogen del
`context.Context` ellos solitos, y las llamadas anidadas se unen a la misma
transacción. Un caso de uso que abarca varios stores hace commit como uno solo, y
no vuelves a pasar un `*sql.Tx` a mano.

Vamos a construir un único ejemplo de principio a fin — crear un pedido en una
tiendita online — y a añadir una idea cada vez, hasta que hayas visto todo lo que
el paquete hace y *por qué* existe cada pieza.

---

## Dónde encaja (un poco de C4)

Antes del código, una imagen para ubicar el paquete. Un **caso de uso** abre una
transacción con `Run`; cada **store**, en lugar de recibir un `*sql.Tx`, le pide
su ejecutor al **Manager** con `Querier(ctx)`; el Manager es el dueño del `*sql.Tx`
real sobre la única base de datos. Como `Run` liga la transacción al contexto, el
`Run` propio de un store *se une* a la transacción del caso de uso en vez de abrir
una segunda.

```
 caso de uso                           stores
 ┌──────────────────────┐              ┌───────────────────────────┐
 │ manager.Run(ctx, fn) │──── ctx ────▶│ store.Run(ctx, fn)         │
 │   abre UNA tx y la   │  lleva la    │   ve la unidad en ctx,     │
 │   liga al ctx        │  unidad de   │   se UNE (sin tx nueva)    │
 └──────────┬───────────┘  trabajo     │ store.Querier(ctx)         │
            │                          │   → el *sql.Tx vivo        │
            ▼                          └─────────────┬─────────────┘
 ┌────────────────────────────────────────────────  ▼ ───────────┐
 │ transaction.Manager                                  │
 │   Run     : begin / join / commit / rollback                   │
 │   Querier : el *sql.Tx mientras hay unidad, si no el *sql.DB   │
 └────────────────────────────────┬──────────────────────────────┘
                                  ▼
                           ┌───────────────┐
                           │ base de datos │  una SQLite (o PostgreSQL)
                           └───────────────┘
```

Todo lo de abajo es esa misma imagen, un detalle cada vez.

---

## El problema, a mano

Seamos honestos sobre el código que ya escribes, porque la razón de ser de este
paquete es borrarlo. Crear un pedido inserta la fila de `orders` y reserva stock
por cada línea, y esas dos escrituras **deben** caer juntas — no puedes acabar con
un pedido cuyo stock nunca se reservó. Así que haces `BeginTx`, y al instante la
transacción se filtra en tu diseño: tiene que llegar a los stores, así que se
vuelve un parámetro.

```go
// Insert recibe un *sql.Tx porque el caso de uso es el único que sabe que hay una
// transacción en marcha. La misma historia con cada otro método de escritura.
func (s OrderStore) Insert(ctx context.Context, tx *sql.Tx, order Order) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO orders (id, customer_email, total_cents, placed_at) VALUES (?, ?, ?, ?)`,
		order.ID, order.CustomerEmail, order.TotalCents, order.PlacedAt.Format(time.RFC3339))
	return err
}

// PlaceOrder es dueño de todo el ciclo de vida de la transacción, a mano.
func PlaceOrder(ctx context.Context, db *sql.DB, orders OrderStore, inventory InventoryStore, order Order) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := orders.Insert(ctx, tx, order); err != nil {
		_ = tx.Rollback() // aquí te acuerdas...
		return err
	}
	for _, line := range order.Lines {
		if err := inventory.Reserve(ctx, tx, line.SKU, line.Quantity); err != nil {
			return err // ...y aquí te olvidas: la transacción se queda ABIERTA.
		}
	}
	return tx.Commit()
}
```

¿Ves el bug? El segundo `return` temprano — el de dentro del bucle — se olvidó del
`tx.Rollback()`. Se marcha dejando la transacción abierta, y en una base de datos
de un solo escritor ese escritor abierto bloquea el *siguiente* pedido. Cada
método de store carga un `*sql.Tx`, cada rama de error tiene que acordarse de
limpiar, y el compilador no pillará la que no lo hace. Esa línea olvidada es el
dolor. El resto de este README es la cura.

> **Recuerda** — a mano, la transacción se vuelve un parámetro en cada firma y
> *tú* eres su dueño; un rollback olvidado la deja colgada.

---

## La solución en dos movimientos: Run y Querier

Aquí está toda la idea: deja de cargar la transacción como parámetro y que pase a
ser un dato del `context.Context` que ya estás pasando por todas partes. Hay
exactamente dos jugadores.

1. El que llama escribe `manager.Run(ctx, func(ctx) error { … })` para **abrir**
   una unidad de trabajo.
2. El store escribe `manager.Querier(ctx)` para **preguntar** contra qué ejecutar
   — el `*sql.Tx` vivo cuando hay una unidad de trabajo en marcha, o el `*sql.DB`
   normal para una sentencia en autocommit cuando no la hay.

Aquí está el mismo `OrderStore`, reescrito. Fíjate en lo que su `Insert` *no*
lleva en la firma (un `*sql.Tx`) y en lo que nunca llamas (`Commit` o `Rollback`):

```go
type OrderStore struct {
	transactions transaction.Transactor // solo Run + Querier
}

func NewOrderStore(transactions transaction.Transactor) *OrderStore {
	return &OrderStore{transactions: transactions}
}

func (s *OrderStore) Insert(ctx context.Context, order Order) error {
	return s.transactions.Run(ctx, func(ctx context.Context) error {
		querier := s.transactions.Querier(ctx) // el *sql.Tx vivo, resuelto del ctx
		_, err := querier.ExecContext(ctx,
			`INSERT INTO orders (id, customer_email, total_cents, placed_at) VALUES (?, ?, ?, ?)`,
			order.ID, order.CustomerEmail, order.TotalCents, order.PlacedAt.UTC().Format(time.RFC3339))
		return err // nil hace commit de esta transacción; un error la revierte
	})
}
```

Y lo cableas una vez, inyectando el Manager como el `Transactor` del store:

```go
manager := transaction.NewManager(database)
orders := NewOrderStore(manager)
```

La forma en que tu función *sale* es la decisión — hay exactamente tres salidas:

```
work devuelve nil    → COMMIT
work devuelve error  → ROLLBACK   (el error igual te lo devuelve)
work hace panic      → ROLLBACK, y el panic se relanza cuando es seguro
```

Esa última importa: un panic hace rollback y se relanza solo *después* de que la
transacción se ha cerrado, así una transacción a medio abrir nunca puede escapar.

> **Recuerda** — la transacción viaja en el contexto, no en tus firmas. `Run` abre
> la unidad de trabajo, `Querier(ctx)` encuentra el ejecutor, y el valor que
> devuelves es la decisión de commit/rollback. Nunca llamas a `Commit` ni a
> `Rollback`.

---

## Cómo consigue un store su conexión

Aquí está la pregunta que descoloca a todo el mundo la primera vez: cuando tu
store hace un `UPDATE`, ¿contra qué lo está ejecutando — la base de datos o la
transacción abierta? En muchos código la respuesta es «contra el `*sql.Tx` que el
llamador se acordó de pasarte», y ese pase a mano es justo la miseria que acabamos
de borrar. Aquí es más tranquilo: el store le pide a `Querier(ctx)`, y esa única
llamada devuelve el `*sql.Tx` vivo cuando hay una unidad de trabajo en marcha, o
el `*sql.DB` normal (autocommit) cuando no la hay.

Así que este `Reserve` no tiene ni rastro de `*sql.Tx`, y aun así hace lo correcto
tanto si lo llamas por su cuenta *como* desde dentro de otro `Run`:

```go
func (s *InventoryStore) Reserve(ctx context.Context, sku string, quantity int64) error {
	return s.transactions.Run(ctx, func(ctx context.Context) error {
		// En un Run esto es el *sql.Tx vivo; llamado a pelo es el *sql.DB.
		result, err := s.transactions.Querier(ctx).ExecContext(ctx,
			`UPDATE inventory SET available = available - ? WHERE sku = ? AND available >= ?`,
			quantity, sku, quantity)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return ErrOutOfStock // el WHERE no casó ninguna fila: nos negamos a sobrevender
		}
		return nil
	})
}
```

Una *lectura* es la misma idea sin los ruedines — ni siquiera abre un `Run`, solo
resuelve el querier y lee contra lo que lleve el contexto:

```go
// Available lee en la transacción abierta cuando se llama dentro de una, si no en la base de datos.
func (s *InventoryStore) Available(ctx context.Context, sku string) (int64, error) {
	rows, err := s.transactions.Querier(ctx).QueryContext(ctx,
		`SELECT available FROM inventory WHERE sku = ?`, sku)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	if !rows.Next() {
		return 0, rows.Err() // sin fila → stock cero
	}
	var available int64
	return available, rows.Scan(&available)
}
```

Piensa en `Querier(ctx)` como el `DataSourceUtils.getConnection` de Spring: no
*decides* si estás en una transacción, lo *preguntas*, y el framework ya lo sabe.

> **Recuerda** — los stores resuelven su ejecutor, nunca lo pasan a mano. La
> interfaz `Querier` es solo `QueryContext` / `ExecContext`, así que es lo que un
> constructor de consultas acepta encantado.

---

## El premio: varios stores, una transacción

Este es el momento en que el paquete sale rentable. Tanto `OrderStore.Insert` como
`InventoryStore.Reserve` están escritos con honestidad — cada uno abre su propio
`Run`, así que por su cuenta cada uno hace commit. Genial por separado, pero
`PlaceOrder` necesita que la fila del pedido *y* el descuento de stock caigan
juntas. Si vienes de Spring, aquí te muerde la duda: «si cada store empieza su
propia transacción, ¿no tendré dos commits?».

No — y aquí está el meollo. La transacción vive en el contexto, así que cuando el
caso de uso abre **un** `Run` externo y llama a los dos stores dentro, sus `Run`s
internos ven la unidad de trabajo que ya está ahí y simplemente **se unen** a ella.
(Esa es la propagación por defecto, `Required`.) Nadie pasa un `*sql.Tx`.

```go
func (uc *PlaceOrder) Do(ctx context.Context, order Order) error {
	return uc.manager.Run(ctx, func(ctx context.Context) error {
		if err := uc.orders.Insert(ctx, order); err != nil {
			return err // rollback: no se tocó el inventario
		}
		for _, line := range order.Lines {
			if err := uc.inventory.Reserve(ctx, line.SKU, line.Quantity); err != nil {
				return err // rollback de TODA la transacción, incluido el pedido
			}
		}
		return nil // commit del pedido + todas las reservas juntas
	})
}
```

```
PlaceOrder.Do
└─ manager.Run            BEGIN  (una transacción, ligada al ctx)
   ├─ orders.Insert
   │   └─ Run  ──────────▶ SE UNE  (sin BEGIN, sin COMMIT)
   ├─ inventory.Reserve
   │   └─ Run  ──────────▶ SE UNE  (sin BEGIN, sin COMMIT)
   └─ return nil          COMMIT  (pedido + reservas juntas)
        return error      ROLLBACK (ambas deshechas)
```

> **Recuerda** — un `Run` externo convierte varias llamadas a stores
> autotransaccionales en una única transacción atómica. Los `Run`s internos se
> unen a ella en vez de abrir la suya.

---

## Cuando algo sale mal

Aquí está el bug que toda transacción hecha a mano acaba criando: en lo profundo
de `PlaceOrder`, un paso falla, lo logueas, *te tragas el error* para que el flujo
se lea bonito, y sigues — y ahora has hecho commit de un pedido cuyo stock nunca se
reservó. Con este paquete no puedes hacer eso sin querer, y ese es todo el sentido
de esta sección.

Cuando `Reserve` corre dentro del `Run` externo, es una llamada *que se une*. En
cuanto su trabajo devuelve un error que provoca rollback, el paquete marca la
unidad de trabajo compartida como **rollback-only** antes de devolverte el error.
Así que aunque te lo tragues, el `Run` más externo se niega a hacer commit: hace
rollback y devuelve `ErrRollbackOnly` arriba del todo.

```go
return manager.Run(ctx, func(ctx context.Context) error {
	if err := orders.Insert(ctx, order); err != nil {
		return err
	}
	for _, line := range order.Lines {
		err := inventory.Reserve(ctx, line.SKU, line.Quantity)
		if errors.Is(err, ErrOutOfStock) {
			// Tentador: tragárselo y «dejar que pase el resto».
			// NO va a pasar — la unidad ya está condenada.
			slog.Warn("línea sin stock, se omite", "sku", line.SKU)
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil // pide un commit...
})
// ...pero si algún Reserve dio ErrOutOfStock, Run hizo rollback y devuelve
// ErrRollbackOnly en su lugar. La fila del pedido nunca llega al disco.
```

En el lado del llamador puedes distinguir la red de seguridad de un error de
verdad:

```go
switch err := placeOrder(...); {
case errors.Is(err, transaction.ErrRollbackOnly):
	slog.Info("pedido revertido: una línea no se pudo reservar")
case err != nil:
	slog.Error("el pedido falló", "error", err)
default:
	slog.Info("pedido creado")
}
```

Es el `globalRollbackOnly` de Spring: en cuanto cualquier participante condena la
unidad, ningún código posterior puede salvarla.

> **Recuerda** — devolver un error (o hacer panic) *es* la decisión de rollback.
> El error fatal de una llamada que se une condena toda la unidad, así que
> tragártelo igual da un rollback y `ErrRollbackOnly`, nunca un pedido a medio
> confirmar.

---

## Propagación

Ya has *usado* la propagación por defecto sin nombrarla: «únete a una transacción
activa, o abre una». Eso es `Required`. La propagación es simplemente tu respuesta
a la pregunta **«¿qué hago si ya hay una transacción en marcha?»**.

```go
manager.Run(ctx, work, transaction.WithPropagation(transaction.Nested))
```

| Propagación              | Ya hay una transacción en marcha     | No hay ninguna                     |
| ------------------------ | ------------------------------------ | ---------------------------------- |
| `Required` *(por defecto)* | se une a ella                      | abre una nueva                     |
| `Supports`               | se une a ella                        | corre sin transacción              |
| `Mandatory`              | se une a ella                        | falla con `ErrTransactionRequired` |
| `Never`                  | falla con `ErrTransactionNotAllowed` | corre sin transacción              |
| `Nested`                 | toma un **savepoint** de ella        | abre una nueva                     |

`Mandatory` viene bien para un helper de bajo nivel que nunca debe correr fuera de
un caso de uso (falla rápido en vez de hacer autocommit en silencio). `Supports` /
`Never` encajan en una lectura que debe funcionar con o sin transacción ambiente.
`Nested` tiene su propia sección a continuación.

Y una ausencia honesta: no vas a encontrar aquí `REQUIRES_NEW` ni `NOT_SUPPORTED`.
Necesitarían una *segunda* transacción corriendo a la vez, y en una base de datos
de un solo escritor esa segunda haría deadlock contra el lock de escritura que la
primera ya tiene. Dejarlas fuera es un encaje deliberado con la fuente de datos, no
un olvido — más en [Alcance](#alcance-y-notas-de-diseño).

> **Recuerda** — `Required` (unirse-o-abrir) es el valor por defecto y lo que
> quieres casi siempre. El resto son para los casos en que «¿ya estoy en una
> transacción?» pide otra respuesta.

---

## El paso opcional (Nested)

Cada llamada a store hasta ahora ha sido todo-o-nada: bajo `Required`, cualquier
error revierte toda la unidad de trabajo. Eso es justo lo correcto para el pedido y
el descuento de stock. Pero ahora el cliente marca «envuélvemelo para regalo», y
eso es otro tipo de trabajo — un «estaría bien». Si la mesa de envoltura se queda
sin lazo, igual quieres que el pedido salga adelante, solo que sin el moño.

Con `Required` a secas, un error del paso de envoltura se llevaría el pedido por
delante. Este es el único momento en que un fallo debe *contenerse*, y para eso
está `Nested`: toma un **savepoint** antes del sub-paso, así que un error que
provoque rollback dentro de él solo deshace hasta ese savepoint — la transacción
externa sigue viva.

```go
func (s *GiftWrapStore) Wrap(ctx context.Context, orderID string, wrap GiftWrap) error {
	return s.transactions.Run(ctx, func(ctx context.Context) error {
		if wrap.Style == "" { // sin lazo, mesa cerrada, ...
			return ErrWrapUnavailable
		}
		_, err := s.transactions.Querier(ctx).ExecContext(ctx,
			`INSERT INTO gift_wraps (order_id, style, price_cents) VALUES (?, ?, ?)`,
			orderID, wrap.Style, wrap.PriceCents)
		return err
	}, transaction.WithPropagation(transaction.Nested))
}
```

Aquí está la parte que descoloca a todo el mundo: **anidar cambia qué se revierte,
no qué se devuelve.** El `Run` interno igual te entrega `ErrWrapUnavailable`; el
savepoint solo significa que la transacción externa sigue sana. Así que atrapas ese
error y dejas que `PlaceOrder` siga — si lo propagaras, abortarías justo el pedido
que intentabas salvar:

```go
if wrap != nil {
	if err := giftWraps.Wrap(ctx, order.ID, *wrap); err != nil &&
		!errors.Is(err, ErrWrapUnavailable) {
		return err // un fallo de envoltura *inesperado* sí aborta el pedido
	}
}
return nil // pedido + inventario hacen commit; la envoltura se omitió en silencio
```

```
Run (Required) ── BEGIN ─────────────────────────── COMMIT  (pedido + inventario sobreviven)
  ├─ orders.Insert
  ├─ inventory.Reserve
  └─ giftWraps.Wrap (Nested) ─ SAVEPOINT ─ err ─ ROLLBACK TO SAVEPOINT
                                                 └─ devuelve ErrWrapUnavailable (te lo tragas)
```

> **Recuerda** — `Nested` es para un sub-paso opcional: su fallo solo hace rollback
> hasta un savepoint y la transacción externa igual hace commit — pero el `Run`
> interno sigue devolviendo el error, así que atrápalo.

---

## Afinar una transacción

Con la propagación como opción ancla, el resto de atributos de `@Transactional`
salen como un pequeño menú de opciones:

| Opción                 | Qué hace                                                              |
| ---------------------- | -------------------------------------------------------------------- |
| `WithIsolation(level)` | nivel de aislamiento para una transacción nueva del todo             |
| `ReadOnly()`           | abre la transacción en solo lectura                                  |
| `WithTimeout(d)`       | cancela el contexto de la transacción tras `d`; al expirar, el error se envuelve con `ErrTransactionTimedOut`. Una `d` negativa falla con `ErrInvalidTimeout` |
| `WithName(name)`       | le pone una etiqueta a la unidad de trabajo, útil para logging       |

```go
// Que un pedido lento no retenga al único escritor para siempre.
return manager.Run(ctx, placeOrderWork,
	transaction.WithName("place-order"),
	transaction.WithTimeout(2*time.Second))
```

Un detalle que conviene interiorizar: `WithIsolation`, `ReadOnly` y `WithTimeout`
solo cuentan para una transacción **nueva del todo** — el `Run` más externo que de
verdad abre una. No hacen nada cuando la llamada *se une* a una existente (no
puedes cambiar el aislamiento de una transacción que ya está en marcha). La
excepción es la comprobación de `WithTimeout` negativo, que es un error de
programación y falla rápido pase lo que pase. Y si te unes con una petición de
aislamiento o solo-lectura que la transacción en marcha no puede cumplir, te llevas
un `ErrIncompatibleJoin`.

> **Recuerda** — las opciones configuran la transacción que *se abre*; un `Run` que
> se une hereda lo que decidió el más externo.

---

## Reglas de rollback: hacer commit a pesar de un error

Conoces el comportamiento por defecto: devuelve un error y `Run` hace rollback. Eso
es justo lo correcto para el pedido y el stock. Pero los casos de uso reales
también tocan cosas que sencillamente no merecen abortar un pedido ya pagado.
Pongamos otorgar puntos de fidelidad: si el servicio de fidelidad está caído, el
pedido ya es válido y el stock ya está reservado, así que tirar toda la transacción
sería la cura equivocada.

Esto es el `@Transactional(noRollbackFor = …)` de Spring. Le dices a `Run` «este
error no es fatal, haz commit igual» — y el error *igual* te vuelve, así que el
llamador puede loguearlo o reintentar más tarde:

```go
return manager.Run(ctx, func(ctx context.Context) error {
	// ... escribir el pedido, reservar stock (que estos fallen SÍ es fatal) ...
	return loyalty.Award(ctx, order.CustomerEmail, order.TotalCents) // mejor esfuerzo
},
	// Haz commit del pedido aunque no se pudieran otorgar los puntos...
	transaction.NoRollbackForError(ErrLoyaltyServiceDown),
	// ...pero una marca de fraude gana y fuerza el rollback, aunque también
	// case con ErrLoyaltyServiceDown de arriba.
	transaction.RollbackForError(ErrLoyaltyFraud),
)
```

```
error devuelto por work
   ├─ ¿casa con una regla RollbackForError?   ── sí ─▶ ROLLBACK  (gana)
   ├─ ¿casa con una regla NoRollbackForError? ── sí ─▶ COMMIT  (el error igual se devuelve)
   └─ si no ────────────────────────────────────────▶ ROLLBACK  (por defecto)
```

También hay formas con predicado (`NoRollbackForFunc`, `RollbackForFunc`) para
cuando un sentinel no basta.

> **Recuerda** — `NoRollbackForError` hace commit a pesar de un error devuelto
> (igual te vuelve el error); `RollbackForError` fuerza el rollback y gana cuando
> los dos casan.

---

## Engancharte al ciclo de vida

A veces quieres hacer algo *justo* cuando la transacción se cierra. Imagina la vida
de un `Run` como un andén por el que el tren pasa, en orden:

```
work() → [before-commit: puede VETAR] → [before-completion]
                                             │
                                 ┌───────────┴───────────┐
                             COMMIT                   ROLLBACK
                                 │                       │
                          [after-commit]                 │   (after-commit se salta)
                                 └───────────┬───────────┘
                                     [after-completion]  ← siempre, recibe el Status
```

El caballo de batalla es **after-commit**. Aquí un bug que previene: tu
`PlaceOrder` hace commit, y luego envía el email «Tu pedido está confirmado».
Envíalo *dentro* del `Run` y un commit fallido significa que le has escrito a un
cliente por un pedido que nunca existió. Envíalo justo *después* de que `Run`
vuelva y una caída en ese hueco finísimo pierde el email sin rastro de que se
debía. El email (y el job de preparación en el almacén) deben pasar exactamente una
vez, y solo cuando las filas están de verdad en disco — para eso está
`RegisterAfterCommit`:

```go
return s.transactions.Run(ctx, func(ctx context.Context) error {
	if err := s.orders.Insert(ctx, order); err != nil {
		return err
	}
	// ... reservar stock ...

	scheduled := transaction.RegisterAfterCommit(ctx, func(ctx context.Context) {
		// La tx está DESLIGADA de este ctx, así que el trabajo de BD aquí hace
		// autocommit contra el *sql.DB. Un panic aquí se recupera y se loguea: un
		// pedido ya confirmado nunca debe parecerle fallido al llamador.
		_ = s.notifier.SendConfirmation(ctx, order.CustomerEmail, order.ID)
		_ = s.notifier.EnqueuePick(ctx, order.ID)
	})
	if !scheduled {
		// No había unidad de trabajo activa (la ruta de autocommit): no hay commit
		// que esperar, así que hazlo ya. Register* devuelve false aquí — no te
		// olvides de esta rama.
		_ = s.notifier.SendConfirmation(ctx, order.CustomerEmail, order.ID)
		_ = s.notifier.EnqueuePick(ctx, order.ID)
	}
	return nil
}, transaction.WithName("place-order"))
```

Las otras fases resuelven lo que after-commit no puede:

- **`RegisterBeforeCommit`** corre dentro de la transacción aún abierta, tu última
  oportunidad de inspeccionar las escrituras pendientes y *vetar* — devuelve un
  error y todo hace rollback, igual que si tu `work` hubiera fallado.
- **`RegisterAfterCompletion`** corre en commit *y* en rollback, con el `Status`
  final — el sitio para registrar «cerró como Committed / RolledBack», nunca el
  sitio para enviar el email (también se dispara al fallar).

```go
transaction.RegisterBeforeCommit(ctx, func(ctx context.Context) error {
	return orderTotalsBalance(ctx) // un no-nil veta el commit
}, transaction.WithOrder(-1)) // el orden menor corre primero dentro de una fase

transaction.RegisterAfterCompletion(ctx,
	func(ctx context.Context, status transaction.Status) {
		slog.Info("transacción de pedido cerrada", "outcome", status.String())
	})
```

La propagación `Nested` tiene su propia pareja, `RegisterSavepoint` /
`RegisterSavepointRollback`, que se disparan alrededor de un savepoint.

> **Recuerda** — `before-commit` es tu último veto, `after-commit` solo se dispara
> al tener éxito (y tras una frontera desligada y a prueba de panics),
> `after-completion` corre siempre con el `Status` final, y `WithOrder` (no el
> orden de registro) ordena una fase.

---

## Recuperar un valor

El `work` de `Run` solo devuelve un `error`, lo cual se queda corto cuando quieres
el `Order` que construiste — con su ID y timestamp generados — de vuelta en la capa
HTTP. El movimiento tentador es declarar un `Order` fuera del closure y asignarlo
dentro. Resístete: si la transacción hace rollback, esa variable guarda un pedido a
medio construir de una fila que nunca cayó.

Usa la herramienta honesta, `RunResult[T]` — una función libre (los métodos en Go
no pueden ser genéricos) que corre igual que `Run` pero deja que tu `work` devuelva
`(T, error)`:

```go
return transaction.RunResult(ctx, transactions,
	func(ctx context.Context) (Order, error) {
		order := draft
		order.ID = uuid.NewString()
		order.PlacedAt = time.Now().UTC()
		if err := orders.Insert(ctx, order); err != nil {
			return Order{}, err // rollback; el Order devuelto es el valor cero
		}
		for _, line := range order.Lines {
			if err := inventory.Reserve(ctx, line.SKU, line.Quantity); err != nil {
				return Order{}, fmt.Errorf("reserve %s: %w", line.SKU, err)
			}
		}
		return order, nil
	},
	transaction.WithName("place-order"))
```

> **Recuerda** — ¿necesitas un valor de una unidad de trabajo? Tira de
> `RunResult[T]` en vez de `Run`, y solo te fíes del valor cuando el error sea
> `nil`.

---

## Espiar y dirigir

Tus llamadas internas a stores ahora se unen a la transacción externa — pero ¿cómo
*sabe* un código en lo hondo de ese árbol de llamadas en qué situación está?
`StatusFromContext` es el espejo de solo lectura del `TransactionStatus` de Spring.
Le pasas un contexto y te dice si hay una unidad de trabajo ligada a él y, si la
hay, su nombre y si *esta* llamada abrió la transacción o solo se unió a una:

```go
func logOrderStep(ctx context.Context, step string) {
	status, active := transaction.StatusFromContext(ctx)
	if !active {
		slog.Info("paso de pedido (sin transacción)", "step", step) // ruta de autocommit
		return
	}
	verb := "uniéndose"
	if status.IsNewTransaction() {
		verb = "abriendo"
	}
	slog.Info("paso de pedido", "step", step, "transaction", status.Name(), "phase", verb)
}
```

El booleano es la clave: `false` significa «aquí no hay transacción», así que
compruébalo antes de fiarte de nada del status. Otros mandos: `status.IsReadOnly()`,
`status.IsCompleted()`, `status.HasSavepoint()`, y las funciones libres
`CurrentTransactionName(ctx)` (para código de logging que no tiene un status a mano)
y `MarkRollbackOnly(ctx)` — que deja a un control de fraude en lo hondo del árbol
*condenar* la transacción sin desenrollar un error por cada capa. El `Run` más
externo devuelve entonces `ErrRollbackOnly`.

> **Recuerda** — `StatusFromContext` lee la unidad de trabajo viva; comprueba su
> booleano primero. `MarkRollbackOnly` veta el commit desde cualquier sitio, sin
> ir pasando un error a mano.

---

## Vigilar cada transacción

Cuando lleves un rato creando pedidos, vas a querer *verlos* — cuánto retuvo cada
uno al escritor, cuántos hicieron commit frente a rollback, un span de traza por
cada uno. Tu instinto, recién salido de la sección anterior, es
`RegisterAfterCommit`. Herramienta equivocada, y aquí va la distinción que merece
tatuarse en la muñeca:

- Las **sincronizaciones** (la familia `Register*`) **participan**: se registran
  desde dentro del `work`, pertenecen a una unidad de trabajo, y pueden hacer
  efectos secundarios de negocio.
- Un **`ExecutionListener`** **observa**: se lo das a `NewManager` una vez al
  cablear, es sin estado, y se dispara alrededor del begin/commit/rollback
  *físico* de cada transacción que conduce el Manager — sin saber nada del pedido
  que se está creando.

```go
observer := transaction.ExecutionListener{
	BeforeBegin: func(ctx context.Context, status transaction.TransactionStatus) {
		slog.InfoContext(ctx, "transaction begin", "name", status.Name())
	},
	AfterCommit: func(ctx context.Context, status transaction.TransactionStatus, err error) {
		metrics.RecordOutcome(status.Name(), "committed", err)
	},
	AfterRollback: func(ctx context.Context, status transaction.TransactionStatus, err error) {
		metrics.RecordOutcome(status.Name(), "rolled_back", err)
	},
}
manager := transaction.NewManager(database, observer)
```

Ese «una vez, al construir» es la clave: no salpicas logging por cada caso de uso;
enganchas un listener y cada transacción se mide gratis. Dos consecuencias con las
que tropiezan los novatos: un listener solo se dispara con una transacción *abierta
físicamente*, así que un `Run` interno que SE UNE (o un paso con savepoint de
`Nested`) no produce callback — un evento por transacción real, no por `Run`. Y un
panic en un listener se recupera, nunca se propaga — un pedido ya confirmado no
debe parecer fallido porque tu contador de métricas reventó (esto diverge de
Spring, donde una excepción del listener escapa).

```
Manager (1 listener, enganchado en NewManager)
└─ Run "place-order"  ── BEGIN físico ──▶ se dispara BeforeBegin
     ├─ orders.Insert    (Run interno SE UNE) ── el listener no se dispara
     ├─ inventory.Reserve(Run interno SE UNE) ── el listener no se dispara
     └─ COMMIT / ROLLBACK ──────────────────▶ se dispara AfterCommit / AfterRollback
```

> **Recuerda** — vigila con listeners (una vez, en el Manager); participa con
> sincronizaciones (por unidad de trabajo). Los listeners nunca se disparan para
> una unión ni para un savepoint.

---

## Errores, con nombre

La parte tranquilizadora del manejo de errores aquí: nunca tienes que mirar con
lupa un error del driver ni comparar cadenas de mensaje. Cada forma en que un `Run`
puede fallar tiene un nombre, y los distingues con `errors.Is`. Los que envuelven
mantienen alcanzable debajo el error original del driver (vía `errors.As`).

| Error                      | Te lo llevas cuando                                                          |
| -------------------------- | --------------------------------------------------------------------------- |
| `ErrRollbackOnly`          | una llamada que se unió condenó la unidad, aunque su error fuera tragado    |
| `ErrTransactionRequired`   | propagación `Mandatory` y no hay ninguna transacción en marcha              |
| `ErrTransactionNotAllowed` | propagación `Never` mientras hay una transacción en marcha                  |
| `ErrIncompatibleJoin`      | te uniste con una petición de aislamiento/solo-lectura que la tx no cumple  |
| `ErrInvalidTimeout`        | `WithTimeout` recibió una duración negativa (cazado antes de correr nada)   |
| `ErrTransactionTimedOut`   | la transacción reventó su propio plazo e hizo rollback (envuelve el error de work) |
| `ErrBeginFailed`           | no se pudo abrir la transacción (envuelve el error del driver)              |
| `ErrTransactionSystem`     | falló un paso de commit, rollback o savepoint (envuelve el error del driver)|

Las dos del reloj merecen una mirada de cerca. `ErrTransactionTimedOut` es un
objetivo *estable*: casa tanto si el driver reportó «context canceled», «context
deadline exceeded» o cualquier otra cosa, así que un `errors.Is` funciona en SQLite
y en PostgreSQL. Y una cancelación de *tu propio* contexto de arriba a propósito
**no** se reporta como timeout de transacción, para que la comprobación siga siendo
significativa. Un ejemplo trabajado:

```go
err := manager.Run(ctx, placeOrderWork,
	transaction.WithName("place-order"),
	transaction.WithTimeout(2*time.Second))

switch {
case err == nil:
	return nil
case errors.Is(err, transaction.ErrInvalidTimeout):
	return fmt.Errorf("place-order está mal configurado: %w", err) // un bug de cableado
case errors.Is(err, transaction.ErrTransactionTimedOut):
	return fmt.Errorf("place-order excedió el tiempo y se revirtió: %w", err)
case errors.Is(err, transaction.ErrTransactionSystem):
	return fmt.Errorf("place-order tuvo un fallo de sistema de base de datos: %w", err)
case errors.Is(err, transaction.ErrRollbackOnly):
	return fmt.Errorf("place-order fue vetado y revertido: %w", err)
default:
	return err // un error de negocio normal, p. ej. ErrOutOfStock — manéjalo según toque
}
```

> **Recuerda** — cada fallo de `Run` es un sentinel con nombre; cázalo con
> `errors.Is`. `ErrInvalidTimeout` es un bug de configuración cazado pronto;
> `ErrTransactionTimedOut` es el plazo en tiempo de ejecución, estable entre
> drivers.

---

## Alcance y notas de diseño

Un par de cosas que conviene saber sobre los límites:

- **Una base de datos, una transacción a la vez.** El backend principal es SQLite
  (un solo escritor); también se prueba contra PostgreSQL. Las propagaciones que
  suspenden una transacción o abren una segunda concurrente (`REQUIRES_NEW`,
  `NOT_SUPPORTED`) se dejan fuera a propósito — con un solo escritor harían
  deadlock.
- **Contexto, no thread-locals.** La unidad de trabajo vive en el
  `context.Context`, no en un thread-local como en Spring. Un `*sql.Tx` no se puede
  compartir entre goroutines, así que el trato es: `work` y sus callbacks corren en
  la goroutine que llamó a `Run`.
- **Un panic no te va a filtrar una transacción.** Si algo hace panic con la
  transacción abierta, primero hace rollback y el panic se relanza solo cuando ya
  está todo cerrado — sin conexiones colgando.
- **`database/sql` y nada más.** Al paquete le da igual el constructor de consultas
  que usen tus stores; `Querier` es solo la superficie mínima `QueryContext` /
  `ExecContext` con la que un constructor se conforma.

### Si vienes de Spring

Te vas a sentir como en casa: `Manager.Run` ↔ `@Transactional`, los mismos
comportamientos de propagación, las mismas reglas de rollback, las cuatro fases de
sincronización, `ExecutionListener` ↔ `TransactionExecutionListener`, y errores
tipados. En qué se diferencia — y exactamente por qué — está catalogado en
[`SPRING_PARITY_AUDIT.md`](./SPRING_PARITY_AUDIT.md).
