---
title: Niveles de log
description: Nombre los loggers jerárquicamente, deje que un paquete herede su nivel del ancestro configurado más cercano y reajuste un programa en ejecución desde el código, un documento JSON o el entorno.
---

El módulo `logging` trae el modelo `logging.level` de Spring Boot al logger de la biblioteca estándar. Cada `slog.Logger` está vinculado a un nombre jerárquico, una asignación de nivel en una ruta de paquete gobierna cada logger debajo de ella, y los niveles con los que se compila un programa pueden ser sobrescritos desde el exterior y cambiados mientras el programa se ejecuta — hasta un solo tipo, de la manera en que `logging.level.com.acme.shop.OrderService=DEBUG` llega a una clase en Spring.

Un `Manager` es propietario del árbol de niveles. Depende únicamente de la biblioteca estándar de Go y es seguro para su uso concurrente: los handlers leen una instantánea inmutable del árbol a través de un puntero atómico, la misma disciplina que `slog.LevelVar`, por lo que la comprobación de nivel en la ruta crítica de log no requiere ningún bloqueo.

## Construcción de un manager

`NewManager` devuelve un manager con el equivalente de los valores por defecto con los que arranca una nueva aplicación de Spring Boot: el nivel raíz es `slog.LevelInfo` y los registros se escriben como texto en la salida de error estándar:

```go
import "github.com/cgardev/gokeel/logging"

levels := logging.NewManager()
logger := levels.Logger("github.com/acme/shop/orders")

logger.Info("order placed", "order", "A-100")
// time=... level=INFO msg="order placed" logger=github.com/acme/shop/orders order=A-100
```

Cada registro lleva el nombre de su logger bajo el atributo `logger`, el equivalente de la columna de nombre de logger en una línea de log de Spring. `WithNameKey` cambia el nombre del atributo, y la clave vacía lo deshabilita.

El formato de salida es un `slog.Handler` delegado, por lo que cualquier handler funciona: `slog.NewJSONHandler` o uno propio. El árbol es la única autoridad de filtrado: nunca se consulta el nivel del propio delegado, así que construya el delegado con el nivel más bajo que deba emitir jamás:

```go
levels := logging.NewManager(
	logging.WithHandler(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: logging.LevelTrace,
	})),
)
```

## La jerarquía

Los nombres de los loggers son jerárquicos: los segmentos se separan mediante `/` y `.`, por lo que una ruta de importación de paquete y un nombre de tipo calificado por el paquete forman el mismo tipo de árbol que Spring construye a partir de nombres de clase separados por puntos. El nivel efectivo de un nombre es el nivel asignado a su ancestro configurado más cercano, recurriendo a la raíz (la regla de nivel efectivo de Logback):

```go
levels := logging.NewManager(
	logging.WithLevel("github.com/acme/shop", slog.LevelWarn),
	logging.WithLevel("github.com/acme/shop/orders", slog.LevelDebug),
)

levels.EffectiveLevel("github.com/acme/shop/orders/postgres.Store") // DEBUG
levels.EffectiveLevel("github.com/acme/shop/billing")               // WARN
levels.EffectiveLevel("github.com/acme/warehouse")                  // INFO, the root
```

La herencia corta segmentos enteros, por lo que un nombre nunca filtra su nivel a un hermano que simplemente comparte un prefijo: un nivel en `github.com/acme/event` no llega a `github.com/acme/eventbus`. El seudonombre `root` se dirige al logger raíz, tal como lo hace en Spring.

## Sobrescrituras externas

Los niveles declarados con `WithLevel` son el equivalente de los niveles que un archivo Logback compila en una aplicación Spring. La configuración externa los sobrescribe: `Apply` superpone un documento `Configuration` sobre el árbol actual, exactamente como las propiedades `logging.level` de Spring se aplican sobre el archivo de log al inicio. Cada nombre que el documento configura se sobrescribe; cualquier otro nombre mantiene su asignación:

```go
document := []byte(`{
	"levels": {
		"root": "warn",
		"github.com/acme/shop/orders": "debug"
	}
}`)

configuration, err := logging.ParseConfiguration(document)
if err != nil {
	return err
}
err = levels.Apply(configuration)
```

`ParseConfiguration` rechaza campos desconocidos y valida cada token de nivel, y `Apply` valida nuevamente antes de tocar el árbol, por lo que un nivel mal escrito falla con `logging.ErrInvalidLevel` y no cambia nada.

Para las sobrescrituras que llegan a través de una bandera o una variable de entorno, `ParseLevels` lee el formato de lista compacto `name=level`:

```go
overrides, err := logging.ParseLevels(os.Getenv("LOG_LEVELS"))
if err != nil {
	return err
}
err = levels.Apply(logging.Configuration{Levels: overrides})
```

Una variable no establecida se analiza como un mapa vacío y no aplica nada, por lo que la ruta del código es la misma independientemente de si el operador proporcionó o no sobrescrituras.

## Cambio de niveles en tiempo de ejecución

`SetLevel` y `ResetLevel` son las mutaciones en tiempo de ejecución que realiza Spring Boot Actuator a través de su endpoint de loggers. Un cambio es visible para cada logger ya creado: los handlers consultan el árbol en cada registro, por lo que un logger de larga duración almacenado en una estructura se reajusta en el lugar:

```go
levels.SetLevel("github.com/acme/shop/orders", slog.LevelDebug)
// ... diagnose ...
levels.ResetLevel("github.com/acme/shop/orders") // inherit again
```

`ResetLevel` borra la asignación para que el nombre vuelva a heredar de su ancestro configurado más cercano (el efecto de escribir un `configuredLevel` nulo a través del actuator). Restablecer la raíz restaura el nivel con el que se construyó el manager; Logback, en cambio, rechaza borrar el nivel raíz, y la divergencia mantiene la operación total mientras la raíz siempre conserva un nivel.

`Loggers` es el inventario que expone el actuator: cada nombre conocido con su nivel configurado directamente, si lo hay, y el nivel efectivo que la jerarquía resuelve para él. La raíz todavía lleva el nivel que el documento asignó anteriormente:

```go
entries := levels.Loggers()
// entries[0] == logging.LoggerLevels{Name: "root", Configured: slog.LevelWarn,
//	IsConfigured: true, Effective: slog.LevelWarn}
```

## Grupos de loggers

Un grupo nombra varios loggers a la vez, el equivalente de las propiedades `logging.group` de Spring Boot. Asignar un nivel al nombre del grupo lo propaga a cada miembro, y un grupo con miembros oculta un logger con el mismo nombre, tal como ocurre en Spring:

```go
levels := logging.NewManager(
	logging.WithGroup("persistence",
		"github.com/acme/shop/orders/postgres",
		"github.com/acme/shop/billing/postgres",
	),
)

levels.SetLevel("persistence", slog.LevelDebug) // both members, one call
```

Los grupos también pueden llegar en un documento `Configuration` bajo `"groups"`, y permanecen registrados para llamadas posteriores a `SetLevel` y `ResetLevel`. Mientras que Spring deja un conflicto entre el nivel de un grupo y el nivel directo de un miembro al orden de las propiedades, el módulo lo resuelve de forma determinista: dentro de un mismo documento, un nivel asignado directamente a un nombre prevalece sobre un nivel que el nombre recibe a través de un grupo.

## Trace, off y los tokens

slog predeclara cuatro niveles; la escala de Spring tiene dos más. `LevelTrace` se sitúa un paso de espaciado por debajo de `slog.LevelDebug`, y `LevelOff` silencia un logger por completo: ningún registro lleva un nivel tan alto:

```go
levels.SetLevel("github.com/noisy/dependency", logging.LevelOff) // silence it
levels.SetLevel("github.com/acme/suspect", logging.LevelTrace)   // everything
```

`ParseLevel` lee los tokens de Spring en cualquier caso (`trace`, `debug`, `info`, `warn`, `error`, `fatal`, `off`), además de la notación de desplazamiento que el propio slog analiza, como `DEBUG-2`. Dos mapeos reflejan exactamente a Spring Boot: `fatal` se analiza como `slog.LevelError`, de la misma manera que Logback mapea FATAL a ERROR, y `false` se analiza como `LevelOff`, el alias que Spring conserva porque YAML lee un `off` simple como el booleano false.

## Un logger por tipo

`NameFor` deriva un nombre jerárquico a partir de un tipo (su ruta de importación de paquete, un punto y el nombre del tipo), el equivalente en Go de `LoggerFactory.getLogger(MyClass.class)` de Spring. `LoggerFor` vincula un logger a ese nombre en una sola llamada:

```go
type OrderStore struct{ /* ... */ }

logger := logging.LoggerFor[OrderStore](levels)
// named github.com/acme/shop/orders.OrderStore

levels.SetLevel(logging.NameFor[OrderStore](), slog.LevelDebug)
```

Dado que el nombre del tipo es un segmento más debajo del paquete, una asignación a nivel de paquete aún lo cubre, y una asignación a nivel de tipo sobrescribe al paquete solo para ese tipo.

## El paquete log clásico

El código que aún utiliza la API `log` estándar se une al árbol a través de `StandardLogger`, que devuelve un `*log.Logger` que reenvía cada mensaje a un nivel fijo. El puente respeta el árbol, por lo que un nombre silenciado descarta los mensajes antes de que sean formateados:

```go
bridge := levels.StandardLogger("github.com/acme/legacy", slog.LevelInfo)
bridge.Print("still on the old API") // filtered like any other record
```

El logger estándar global del proceso se enruta de la misma manera:

```go
log.SetOutput(bridge.Writer())
log.SetFlags(0) // the delegate stamps records; avoid a duplicated timestamp
```

## Pasos siguientes

La [referencia de Logging](/gokeel/es/reference/logging/) cubre cada símbolo exportado (las opciones, el documento `Configuration` y las funciones auxiliares de análisis) y el [recetario](/gokeel/es/cookbook/logging/) recopila recetas listas para copiar para las tareas comunes.
