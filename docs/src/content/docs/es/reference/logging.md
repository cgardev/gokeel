---
title: Registro de logs
description: Construcción de un Manager de nivel, vinculación de loggers jerárquicos, anulación de niveles a partir de un documento de Configuration y ajuste de un programa en ejecución.
---

El paquete `logging` proporciona niveles de log jerárquicos y dinámicamente ajustables para los paquetes `log/slog` y `log` de la biblioteca estándar: el análogo en Go del árbol de configuración `logging.level` de Spring Boot. Un `Manager` posee un árbol de niveles nombrados en el que cada logger hereda el nivel de su ancestro configurado más cercano, por lo que una asignación en una ruta de paquete gobierna a cada logger debajo de ella. Un `Manager` es seguro para uso concurrente, y la comprobación de nivel en la ruta crítica (hot path) de registro lee una instantánea atómica, por lo que no adquiere ningún bloqueo.

```go
import "github.com/cgardev/gokeel/logging"
```

## NewManager

`NewManager` construye un `Manager` con el análogo de los valores predeterminados con los que arranca una aplicación Spring Boot limpia: un nivel raíz de `slog.LevelInfo` y un delegado de texto de consola, que escribe en el error estándar de la misma manera que lo hace la biblioteca estándar de Go de forma predeterminada.

```go
func NewManager(options ...Option) *Manager
```

```go
levels := logging.NewManager(
    logging.WithLevel("github.com/acme/shop", slog.LevelWarn),
)
```

Las opciones se aplican en el orden dado, de modo que un grupo se declara antes de que se asigne un nivel a su nombre. A lo largo de esta página, `levels` es el `*Manager` construido aquí.

## Las opciones

Una `Option` personaliza un `Manager` en el momento de la construcción.

### WithHandler

```go
func WithHandler(handler slog.Handler) Option
```

`WithHandler` establece el `slog.Handler` a través del cual emite cada logger. El árbol de niveles es la única autoridad de filtrado (el propio nivel del delegado, si tiene uno, nunca se consulta), por lo que debe construir el delegado con el nivel más bajo que debería emitir. Se ignora un handler nil.

```go
logging.WithHandler(slog.NewJSONHandler(os.Stderr,
    &slog.HandlerOptions{Level: logging.LevelTrace}))
```

### WithRootLevel

```go
func WithRootLevel(level slog.Level) Option
```

`WithRootLevel` asigna el nivel raíz, el límite mínimo al que recurre cada nombre cuando no se ha configurado ningún ancestro. El valor predeterminado es `slog.LevelInfo`, el nivel raíz de una aplicación Spring Boot; el valor también se convierte en el nivel que `ResetLevel` restaura en el logger raíz.

```go
logging.WithRootLevel(slog.LevelWarn)
```

### WithLevel

```go
func WithLevel(name string, level slog.Level) Option
```

`WithLevel` asigna un nivel a un nombre en el momento de la construcción, el análogo de un elemento de logger compilado en un archivo Logback. Un `Apply`, `SetLevel` o `ResetLevel` posterior lo anula, exactamente la misma precedencia que Spring otorga a la configuración externalizada sobre los niveles que declara su archivo de registro de logs.

```go
logging.WithLevel("github.com/acme/shop", slog.LevelWarn)
```

### WithGroup

```go
func WithGroup(name string, members ...string) Option
```

`WithGroup` declara un grupo con nombre de loggers, el análogo de las propiedades `logging.group` de Spring Boot; no está relacionado con la agrupación de atributos de `slog.Handler.WithGroup`. Asignar un nivel al nombre del grupo lo distribuye a cada miembro, y un grupo con miembros oculta un logger del mismo nombre, al igual que ocurre en Spring.

```go
logging.WithGroup("persistence",
    "github.com/acme/shop/orders/postgres",
    "github.com/acme/shop/billing/postgres")
```

### WithNameKey

```go
func WithNameKey(key string) Option
```

`WithNameKey` establece la clave de atributo bajo la cual cada logger registra su propio nombre. La clave predeterminada es `logger`; la cadena vacía deshabilita el atributo.

```go
logging.WithNameKey("component")
```

## Manager.Logger y Manager.Handler

`Logger` devuelve un `*slog.Logger` vinculado a un nombre jerárquico, típicamente una ruta de importación de paquete; `Handler` devuelve el `slog.Handler` subyacente para los llamadores que componen handlers por sí mismos. Ambos consultan el árbol de niveles en cada registro, por lo que un `Apply` o `SetLevel` posterior es visible para los loggers que ya se han entregado.

```go
func (manager *Manager) Logger(name string) *slog.Logger
func (manager *Manager) Handler(name string) slog.Handler
```

```go
logger := levels.Logger("github.com/acme/shop/orders")
logger.Info("order placed", "order", "A-100")
```

Los nombres se segmentan en `/` y `.`, de modo que `github.com/acme/shop/orders.Store` se sitúa bajo `github.com/acme/shop/orders`. La herencia corta segmentos completos (un nivel en `github.com/acme/event` no llega a `github.com/acme/eventbus`) y el seudonombre `root` se dirige al logger raíz. Cuando el atributo de nombre está habilitado, el delegado lo lleva preformateado, sin añadir coste por registro.

## Manager.SetLevel y Manager.ResetLevel

`SetLevel` asigna el nivel de un nombre para cada logger ya creado y para cada logger creado posteriormente: la mutación en tiempo de ejecución que realiza Spring Boot Actuator a través de su endpoint de loggers. `ResetLevel` limpia la asignación de modo que el nombre hereda de su ancestro configurado más cercano nuevamente, el efecto de escribir un `configuredLevel` nulo a través del actuator.

```go
func (manager *Manager) SetLevel(name string, level slog.Level)
func (manager *Manager) ResetLevel(name string)
```

```go
levels.SetLevel("github.com/acme/shop/orders", slog.LevelDebug)
levels.ResetLevel("github.com/acme/shop/orders")
```

Cuando el nombre designa un grupo con miembros, `SetLevel` distribuye el nivel a los miembros y el nombre del grupo en sí permanece sin configurar; `ResetLevel` limpia a cada miembro. Restablecer la raíz restaura el nivel con el que se construyó el `Manager` (Logback, en cambio, rechaza limpiar el nivel raíz, y la divergencia mantiene la operación total mientras que la raíz siempre conserva un nivel).

## Manager.EffectiveLevel y Manager.ConfiguredLevel

`EffectiveLevel` resuelve el nivel que produce la jerarquía para un nombre: el nivel de su ancestro configurado más cercano, o el nivel raíz cuando no se ha configurado ningún ancestro. `ConfiguredLevel` devuelve el nivel asignado directamente al nombre e informa si se ha asignado alguno en absoluto; la raíz siempre tiene uno. El par refleja los campos `effectiveLevel` y `configuredLevel` de una entrada de loggers de actuator.

```go
func (manager *Manager) EffectiveLevel(name string) slog.Level
func (manager *Manager) ConfiguredLevel(name string) (slog.Level, bool)
```

```go
effective := levels.EffectiveLevel("github.com/acme/shop/orders/postgres.Store")
configured, ok := levels.ConfiguredLevel("github.com/acme/shop") // ok reports a direct assignment
```

## Configuration

Una `Configuration` es el documento provisto externamente que anula los niveles que compiló un programa: el análogo de las familias de propiedades `logging.level` y `logging.group`. `Levels` asigna un token de nivel a cada nombre de logger, y `Groups` declara grupos con nombre para que una sola entrada pueda reajustar varios paquetes a la vez.

```go
type Configuration struct {
    Levels map[string]string   `json:"levels,omitempty"`
    Groups map[string][]string `json:"groups,omitempty"`
}
```

```go
configuration := logging.Configuration{
    Levels: map[string]string{
        "root":                 "warn",
        "github.com/acme/shop": "debug",
    },
    Groups: map[string][]string{
        "persistence": {"github.com/acme/shop/orders/postgres"},
    },
}
```

`Validate` informa del primer problema que impediría aplicar el documento: un token de nivel que no se puede analizar, un grupo que designa al logger raíz o un miembro de grupo que no nombra a ningún logger.

## ParseConfiguration and ParseLevels

`ParseConfiguration` decodifica un documento JSON `Configuration` y lo valida; se rechazan los campos desconocidos, por lo que una clave mal escrita falla ruidosamente en lugar de ser ignorada silenciosamente. `ParseLevels` lee la lista compacta `name=level` que transporta una variable de entorno o una bandera de línea de comandos; el resultado se conecta directamente en el campo `Levels` de una `Configuration`.

```go
func ParseConfiguration(document []byte) (Configuration, error)
func ParseLevels(list string) (map[string]string, error)
```

```go
overrides, err := logging.ParseLevels("root=warn,github.com/acme/shop=debug")
```

Una lista vacía se analiza como un mapa vacío, por lo que una variable de entorno no establecida no aplica nada. Ambas funciones fallan con `ErrInvalidLevel` cuando un token no nombra ningún nivel conocido.

## Manager.Apply

`Apply` superpone una `Configuration` sobre los niveles que el manager mantiene actualmente: el momento en que Spring Boot aplica las propiedades `logging.level` sobre los niveles que declara su archivo de registro de logs. Cada nombre que configura el documento se sobrescribe; cualquier otro nombre conserva su asignación actual. Los grupos que declara el documento se registran antes de que se aplique cualquier nivel, y permanecen disponibles para llamadas posteriores a `SetLevel` y `ResetLevel`.

```go
func (manager *Manager) Apply(configuration Configuration) error
```

```go
if err := levels.Apply(configuration); err != nil {
    return err
}
```

Dos reglas hacen que el resultado sea determinista allí donde Spring lo deja al orden de iteración de las propiedades: los niveles se aplican en orden lexicográfico de nombre, y un nivel asignado directamente a un nombre gana sobre un nivel que el nombre recibe a través de un grupo. El documento se valida por adelantado, por lo que un fallo de validación deja al manager intacto.

## Manager.Loggers y Manager.Groups

`Loggers` enumera cada logger que conoce el manager (cada nombre vinculado por una llamada a `Logger` o `Handler` más cada nombre al que se le ha asignado un nivel) ordenados por nombre con el logger raíz en primer lugar: el inventario que expone el endpoint de loggers de actuator. `Groups` devuelve los grupos declarados y sus miembros; el resultado es una copia, por lo que mutarlo no afecta al manager.

```go
func (manager *Manager) Loggers() []LoggerLevels
func (manager *Manager) Groups() map[string][]string
```

```go
type LoggerLevels struct {
    Name         string     // canonical name; the root logger reads "root"
    Configured   slog.Level // meaningful only when IsConfigured is true
    IsConfigured bool       // a level is assigned directly, not inherited
    Effective    slog.Level // the level the hierarchy resolves for the name
}
```

```go
entries := levels.Loggers()
// entries[0].Name == "root"; entries[0].IsConfigured == true
```

## LevelTrace y LevelOff

El paquete completa la escala de slog con los dos niveles que Spring acepta más allá de ella, declarados como valores ordinarios de `slog.Level`:

```go
const (
    LevelTrace slog.Level = slog.LevelDebug - 4 // one spacing step below DEBUG
    LevelOff   slog.Level = math.MaxInt32       // no record carries a level this high
)
```

`LevelTrace` ocupa la posición que TRACE tiene por debajo de DEBUG en Logback, y `LevelOff` refleja el OFF de Logback, que es `Integer.MAX_VALUE`: asignarlo silencia un logger por completo, incluso para errores.

## ParseLevel y LevelName

`ParseLevel` convierte un token de nivel al estilo de Spring en un `slog.Level`. Acepta los tokens de Spring `trace`, `debug`, `info`, `warn`, `error`, `fatal` y `off` en cualquier caso de mayúsculas/minúsculas, el alias de conveniencia adicional `warning`, más la notación de desplazamiento que el propio slog analiza, como `DEBUG-2`. `LevelName` representa un nivel con los tokens al estilo de Spring `TRACE` y `OFF` para los dos niveles que añade este paquete, y con la propia notación de slog para cualquier otro valor.

```go
func ParseLevel(token string) (slog.Level, error)
func LevelName(level slog.Level) string
```

```go
level, err := logging.ParseLevel("debug")     // slog.LevelDebug
name := logging.LevelName(logging.LevelTrace) // "TRACE"
```

Dos asignaciones reflejan exactamente a Spring Boot: `fatal` se analiza como `slog.LevelError`, de la misma manera que Logback asigna FATAL a ERROR, y `false` se analiza como `LevelOff`, el alias que Spring conserva porque YAML lee un `off` sin formato como el booleano false. Un token desconocido falla con `ErrInvalidLevel`.

## NameFor y LoggerFor

`NameFor` devuelve el nombre de logger jerárquico del tipo `T`: su ruta de importación de paquete, un punto y el nombre del tipo, el análogo en Go de `LoggerFactory.getLogger(MyClass.class)` de Spring. `LoggerFor` devuelve un logger del manager nombrado según `T`. Son funciones libres en lugar de métodos porque los métodos en Go no pueden ser genéricos.

```go
func NameFor[T any]() string
func LoggerFor[T any](manager *Manager) *slog.Logger
```

```go
logger := logging.LoggerFor[OrderStore](levels)

levels.SetLevel(logging.NameFor[OrderStore](), slog.LevelDebug)
```

Un tipo de puntero produce el nombre del tipo al que apunta, y un tipo sin un paquete, como un tipo predeclarado, produce su propia notación.

## Manager.StandardLogger

`StandardLogger` devuelve un `*log.Logger` de la biblioteca estándar que reenvía cada mensaje al logger nombrado en el nivel dado: el puente para el código que todavía habla la API clásica de `log`. El puente respeta el árbol de niveles: cuando el nivel efectivo del nombre silencia el nivel dado, los mensajes se descartan antes de formatear.

```go
func (manager *Manager) StandardLogger(name string, level slog.Level) *log.Logger
```

```go
bridge := levels.StandardLogger("github.com/acme/legacy", slog.LevelInfo)
bridge.Print("delivered through the tree")

log.SetOutput(bridge.Writer()) // route the process-global logger the same way
log.SetFlags(0)                // the delegate stamps records itself
```

## Errores

El paquete exporta un error centinela, comparable con `errors.Is`:

- `ErrInvalidLevel` informa de un token de nivel que no nombra ningún nivel conocido. El token ofensivo se adjunta al error devuelto, y `ParseLevel`, `ParseLevels`, `ParseConfiguration`, `Configuration.Validate` y `Manager.Apply` lo exponen.

```go
if _, err := logging.ParseLevel("loud"); errors.Is(err, logging.ErrInvalidLevel) {
    // the token names no known level
}
```

Para un recorrido guiado por la jerarquía y la historia de anulación, consulte [Niveles de log](/gokeel/es/guides/log-levels/); para fragmentos de código listos para copiar, consulte el [libro de recetas](/gokeel/es/cookbook/logging/).
