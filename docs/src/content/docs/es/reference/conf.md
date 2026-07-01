---
title: Configuración
description: Construcción de un Loader sobre fuentes JSON superpuestas, resolución de marcadores de posición del entorno, vinculación en structs y generación de un JSON Schema con GenerateSchema.
---

El paquete `conf` proporciona configuración externa para aplicaciones en Go: el equivalente de la historia de `application.properties` y `application.yml` de Spring Boot, portada por documentos JSON. Un `Loader` fusiona fuentes de documentos ordenadas, resuelve marcadores de posición `${...}` contra el entorno y el propio documento, y vincula el resultado en un struct de Go simple con conversiones relajadas. Un `Loader` es inmutable después de la construcción y seguro para su uso concurrente.

```go
import "github.com/cgardev/gokeel/conf"
```

## NewLoader

`NewLoader` construye un `Loader` sobre las fuentes dadas. Las fuentes se superponen en el orden de las opciones; un documento posterior anula a uno anterior clave por clave, fusionando los objetos de forma profunda, reflejando cómo las fuentes de propiedades de Spring posteriores anulan a las anteriores. Las fuentes se leen en cada `Load`, por lo que una fuente de archivo observa el archivo tal como está en ese momento.

```go
func NewLoader(options ...Option) *Loader
```

```go
loader := conf.NewLoader(
    conf.WithFilesystemFile(defaults, "application.json"),
    conf.WithOptionalFile("/etc/shop/application.json"),
)
```

A lo largo de esta página, `loader` es el `*Loader` construido aquí y `settings` es el struct de destino que se está vinculando.

## Las opciones

Una `Option` personaliza un `Loader` en el momento de la construcción.

### WithFile

```go
func WithFile(path string) Option
```

`WithFile` añade un documento leído desde el archivo en la ruta dada. La falta de un archivo hace que el `Load` falle con un error que coincide con `fs.ErrNotExist`, el equivalente de la `ConfigDataLocationNotFoundException` que aborta el inicio de Spring Boot.

```go
conf.WithFile("/etc/shop/application.json")
```

### WithOptionalFile

```go
func WithOptionalFile(path string) Option
```

`WithOptionalFile` añade un documento leído desde el archivo en la ruta dada, que se omite silenciosamente cuando el archivo no existe: el equivalente del prefijo `optional:` de `spring.config.import`.

```go
conf.WithOptionalFile("./application.local.json")
```

### WithFilesystemFile

```go
func WithFilesystemFile(filesystem fs.FS, path string) Option
```

`WithFilesystemFile` añade un documento leído desde un archivo dentro del sistema de archivos dado. Un `fs.FS` construido con `go:embed` lleva los valores predeterminados dentro del binario de la misma manera que `application.properties` viaja dentro de un jar de Spring Boot, con archivos externos superpuestos sobre él mediante opciones posteriores. Un sistema de archivos nil se ignora.

```go
//go:embed application.json
var defaults embed.FS

conf.WithFilesystemFile(defaults, "application.json")
```

### WithDocument

```go
func WithDocument(document []byte) Option
```

`WithDocument` añade un documento JSON literal. Los bytes se copian, por lo que la mutación posterior de la porción del llamador no puede afectar al `Loader`.

```go
conf.WithDocument([]byte(`{"server": {"port": 9090}}`))
```

### WithLookup

```go
func WithLookup(lookup func(name string) (string, bool)) Option
```

`WithLookup` reemplaza el entorno contra el cual se resuelven los marcadores de posición. El valor predeterminado es `os.LookupEnv`; las pruebas y los procesos que extraen secretos de otro almacén inyectan su propia función aquí. Una búsqueda nil se ignora.

```go
conf.WithLookup(func(name string) (string, bool) { return vault.Read(name) })
```

## Loader.Load

`Load` lee cada fuente, fusiona los documentos en orden, resuelve los marcadores de posición y vincula el resultado en el destino, que debe ser un puntero no nil a un struct. Los nombres que el documento fusionado no menciona conservan los valores ya presentes en el destino, por lo que un llamador establece valores predeterminados a nivel de código rellenando el struct antes de la llamada, el equivalente de los inicializadores de campo en una clase `@ConfigurationProperties`.

```go
func (loader *Loader) Load(target any) error
```

```go
settings := shopConfiguration{Server: serverConfiguration{Port: 8080}}
if err := loader.Load(&settings); err != nil {
    return err
}
```

Una clave que no coincide con ningún campo falla con `ErrUnknownKey` nombrando la ruta punteada completa. Esto difiere de Spring, cuya vinculación ignora las propiedades desconocidas de forma predeterminada: un documento JSON está destinado a ser editado contra el esquema generado, por lo que una clave perdida es un error por el cual vale la pena fallar ruidosamente. Una clave `$schema` a nivel de raíz es la excepción documentada; asocia el esquema en los editores y se descarta antes de la vinculación.

La vinculación convierte con la tolerancia de la vinculación relajada de Spring: las cadenas se convierten a números y a booleanos con los conjuntos de tokens de Spring (`true`, `on`, `yes`, `1` y `false`, `off`, `no`, `0`, en cualquier caso), un `time.Duration` lee la notación de Go `"1m30s"` o un número bruto de nanosegundos (Spring en su lugar lee un número simple como milisegundos), y un campo cuyo tipo implementa `encoding.TextUnmarshaler`, como `time.Time` o `slog.Level`, se vincula a partir de una cadena. Los números conservan su precisión `int64` completa, un `null` de JSON deja el valor actual en su lugar y los structs incrustados se promocionan de la manera en que `encoding/json` los promociona.

## Marcadores de posición

Cada cadena en el documento fusionado puede delegar en el entorno con la gramática de marcadores de posición (placeholders) de Spring: `${NAME}` requiere un valor, `${NAME:default}` recurre al valor por defecto después del primer dos puntos no escapado (se permite un valor por defecto vacío), los marcadores de posición se componen dentro de cadenas más grandes y se anidan en claves y valores por defecto, un valor obtenido para una clave se resuelve recursivamente a sí mismo, y una barra invertida escapa un `${` literal — escrito como `\\${` dentro de una cadena JSON.

```go
document := []byte(`{
    "name": "${SHOP_NAME:shop}",
    "database": "postgres://${DATABASE_HOST}:${DATABASE_PORT:5432}/shop",
    "health": "http://${server.host}:${server.port}/health"
}`)
```

La resolución consulta primero al entorno — bajo el nombre exacto, luego bajo la forma relajada que lee la vinculación de Spring, en la cual se eliminan los guiones, otros separadores se convierten en guiones bajos y el resultado se escribe en mayúsculas, por lo que `${demo.item-price}` también encuentra `DEMO_ITEMPRICE` — y por último al propio documento fusionado, por ruta punteada, de modo que un valor hace referencia a uno definido previamente. El entorno gana sobre el documento, el orden que Spring da a las variables del sistema operativo sobre los archivos de configuración.

Un marcador de posición sin valor y sin valor por defecto falla con `ErrUnresolvedPlaceholder` nombrando la clave, el equivalente de la `PlaceholderResolutionException` que aborta el inicio de Spring Boot; una cadena de referencias que regresa a una clave anterior falla con `ErrCircularPlaceholder`. Un `${` no terminado es texto literal, tal como lo es para el analizador de Spring.

## GenerateSchema

`GenerateSchema` deriva un documento JSON Schema draft 2020-12 a partir del struct sobre el cual se vincula la configuración: el equivalente de los metadatos de configuración que Spring Boot genera a partir de clases `@ConfigurationProperties` para el autocompletado del editor. Los nombres de los campos siguen las etiquetas `json`, un campo es requerido a menos que su etiqueta lleve `omitempty` u `omitzero`, y los objetos rechazan propiedades desconocidas, coincidiendo con la vinculación estricta de `Load`; la raíz además permite la propia clave `$schema`, tipada como una `uri-reference`, de modo que la asociación que lleva un documento se valide limpiamente.

```go
func GenerateSchema(prototype any) ([]byte, error)
```

```go
type serverConfiguration struct {
    Host string `json:"host" jsonschema:"description=Interface to bind,default=localhost"`
    Port int    `json:"port" jsonschema:"minimum=1,maximum=65535"`
    Mode string `json:"mode,omitempty" jsonschema:"enum=development,enum=production"`
}

schema, err := conf.GenerateSchema(shopConfiguration{})
```

La etiqueta de campo `jsonschema` aporta restricciones como elementos clave=valor separados por comas, la gramática establecida por el ecosistema de generación de esquemas de Go: `title`, `description`, `default`, `enum`, `example`, `pattern`, `format`, `minimum`, `maximum`, `minLength` y `maxLength`, con `enum` y `example` repetibles y una barra invertida escapando una coma literal. La etiqueta `jsonschema_description` establece una descripción como un valor completo, el canal seguro para comas para texto libre. Valores de `default`, `enum` y `example` son coaccionados al tipo JSON del campo, y en un campo de matriz `enum` y `example` restringen los elementos. Un campo `time.Duration` se declara como una cadena que coincide con la notación de duración de Go o un recuento de nanosegundos entero. Una clave de etiqueta no admitida, un tipo recursivo y un prototipo que no describe un objeto JSON se reportan como errores.

## Errores

El paquete exporta cuatro errores centinela, cada uno de ellos equiparable con `errors.Is`:

- `ErrUnresolvedPlaceholder` reporta un `${name}` sin valor en el entorno o el documento y sin valor por defecto después de dos puntos. El nombre del marcador de posición se adjunta al error devuelto.
- `ErrCircularPlaceholder` reporta un marcador de posición cuya resolución conduce de regreso a sí mismo, directamente o a través de otros marcadores de posición.
- `ErrUnknownKey` reporta una clave de documento que no coincide con ningún campo del struct de destino, con la ruta punteada completa adjunta.
- `ErrTypeMismatch` reporta un valor de documento que no se puede convertir al tipo del campo al que se vincula — el equivalente de la `BindException` que genera un fallo de vinculación de Spring Boot — con la ruta y ambos tipos adjuntos.

```go
if err := loader.Load(&settings); errors.Is(err, conf.ErrUnknownKey) {
    // the document names a key the struct does not declare
}
```

La falta de un archivo requerido surge como un error que coincide con `fs.ErrNotExist`.

Para el recorrido guiado sobre superposición, marcadores de posición y el flujo de trabajo del editor, consulte [Configuración externa](/gokeel/es/guides/externalized-configuration/); para obtener fragmentos listos para copiar, incluyendo el reajuste de los niveles de registro desde el documento de configuración, consulte el [libro de recetas](/gokeel/es/cookbook/conf/).
