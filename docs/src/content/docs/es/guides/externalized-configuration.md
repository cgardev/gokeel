---
title: Configuración externalizada
description: Carga documentos JSON en capas, difiere valores al entorno con marcadores de posición, vincula el resultado en un struct simple y genera un JSON Schema para editores.
---

El módulo `conf` externaliza la configuración de la misma manera que lo hacen `application.properties` and `application.yml` de Spring Boot, transportada por documentos JSON. Un `Loader` fusiona fuentes de documentos ordenadas, resuelve los marcadores de posición `${...}` con respecto al entorno y al propio documento, y vincula el resultado en un struct de Go simple con las conversiones relajadas que realiza la vinculación `@ConfigurationProperties` de Spring. A partir de ese mismo struct, `GenerateSchema` deriva un JSON Schema, de modo que los editores y los asistentes de código conocen cada clave, su tipo y sus valores permitidos mientras se editan los documentos.

El módulo depende únicamente de la biblioteca estándar de Go. Un `Loader` es inmutable después de su construcción y seguro para uso concurrente, y sus fuentes se leen en cada `Load`, por lo que una recarga observa los archivos tal como están en ese momento.

## Declaración de la configuración

La configuración es un struct simple con etiquetas `json`. Los valores que ya están presentes en el destino cuando se ejecuta `Load` son los valores predeterminados a nivel de código —el equivalente de los inicializadores de campo en una clase `@ConfigurationProperties`— porque los nombres que los documentos no mencionan conservan lo que sea que contenga el struct:

```go
import "github.com/cgardev/gokeel/conf"

type serverConfiguration struct {
	Host    string        `json:"host"`
	Port    int           `json:"port"`
	Timeout time.Duration `json:"timeout,omitempty"`
}

type shopConfiguration struct {
	Name   string              `json:"name"`
	Server serverConfiguration `json:"server"`
}
```

## Carga de documentos

`NewLoader` toma las fuentes en el orden en que se superponen entre sí: un documento posterior gana clave por clave, con objetos fusionados en profundidad, reflejando cómo las fuentes de propiedades posteriores de Spring superponen a las anteriores. Los valores predeterminados viajan dentro del binario a través de un `fs.FS` —el equivalente de `application.properties` empaquetado en un jar de Spring Boot— y un archivo externo se superpone sobre ellos:

```go
//go:embed application.json
var defaults embed.FS

loader := conf.NewLoader(
	conf.WithFilesystemFile(defaults, "application.json"),
	conf.WithOptionalFile("/etc/shop/application.json"),
)

settings := shopConfiguration{Server: serverConfiguration{Port: 8080}}
if err := loader.Load(&settings); err != nil {
	return err
}
```

`WithOptionalFile` es el equivalente del prefijo `optional:` de `spring.config.import`: un archivo faltante se omite silenciosamente. Un archivo nombrado con `WithFile` debe existir —el fallo coincide con `fs.ErrNotExist`, el equivalente de `ConfigDataLocationNotFoundException` que aborta el inicio de Spring Boot. `WithDocument` añade un documento literal, copiado a la defensiva.

## Diferir al entorno

Un valor de cadena puede nombrar una variable de entorno en lugar de contener el valor, exactamente como lo hace un valor de propiedad de Spring. `${NAME}` lee la variable y hace fallar el `Load` con `conf.ErrUnresolvedPlaceholder` cuando no está establecida; `${NAME:default}` recurre al valor predeterminado después de los dos puntos; y los marcadores de posición se componen dentro de cadenas más grandes:

```go
document := []byte(`{
	"name": "${SHOP_NAME:shop}",
	"server": {
		"host": "${SHOP_HOST}",
		"port": "${SHOP_PORT:8080}"
	},
	"database": "postgres://${DATABASE_HOST}:${DATABASE_PORT:5432}/shop"
}`)
```

La gramática es la de Spring, incluyendo sus casos particulares: los marcadores de posición se anidan en claves y valores predeterminados, un valor obtenido para una clave se resuelve recursivamente a sí mismo, una barra invertida escapa un `${` literal, y una cadena que vuelve a una clave anterior falla con `conf.ErrCircularPlaceholder` en lugar de realizar una recursividad infinita.

La resolución consulta primero al entorno —bajo el nombre exacto, luego bajo la forma relajada que lee Spring, por lo que `${demo.item-price}` también encuentra `DEMO_ITEMPRICE`— y al propio documento fusionado al final, por lo que un valor puede hacer referencia a uno definido previamente, de la misma manera que Spring filtra los valores de las propiedades a través del Environment existente:

```go
document := []byte(`{
	"server": {"host": "shop.internal", "port": 8080},
	"health":  "http://${server.host}:${server.port}/health"
}`)
```

## Vinculación relajada

Los valores se convierten al tipo de campo con la tolerancia de la vinculación relajada de Spring. Las cadenas se convierten en números y en booleanos —con los conjuntos de tokens de Spring, por lo que `"yes"`, `"on"` y `"1"` son verdaderos—, un campo `time.Duration` lee la notación de Go `"1m30s"` (o un número sin procesar de nanosegundos; Spring en su lugar lee un número simple como milisegundos), y cualquier campo cuyo tipo deserialice texto, como `time.Time` o `slog.Level`, se vincula a partir de una cadena. Los números conservan su precisión `int64` completa, y un `null` de JSON deja el valor actual en su lugar, por lo que nunca borra un valor predeterminado.

Una regla es deliberadamente más estricta que Spring, cuya vinculación ignora las propiedades desconocidas por defecto: una clave que no coincide con ningún campo hace fallar el `Load` con `conf.ErrUnknownKey` indicando la ruta completa separada por puntos. Un documento destinado a ser editado con un esquema trata una clave huérfana como un error por el cual vale la pena fallar ruidosamente. La clave `$schema` de nivel raíz es la excepción documentada: asocia el esquema en los editores y se descarta antes de la vinculación.

## El esquema para editores

`GenerateSchema` deriva un documento JSON Schema draft 2020-12 a partir del mismo struct en el que se vinculan los documentos —el equivalente de los metadatos de configuración que genera Spring Boot a partir de las clases `@ConfigurationProperties` para el autocompletado en el editor. Los campos sin `omitempty` u `omitzero` son obligatorios, los objetos rechazan las propiedades desconocidas tal como lo hace `Load`, y la etiqueta de campo `jsonschema` aporta descripciones, valores predeterminados, enumeraciones y límites:

```go
type serverConfiguration struct {
	Host string `json:"host" jsonschema:"description=Interface to bind,default=localhost"`
	Port int    `json:"port" jsonschema:"minimum=1,maximum=65535"`
	Mode string `json:"mode,omitempty" jsonschema:"enum=development,enum=production"`
}

schema, err := conf.GenerateSchema(shopConfiguration{})
if err != nil {
	return err
}
err = os.WriteFile("application.schema.json", schema, 0o644)
```

Luego, un documento apunta su clave `$schema` al archivo —`"$schema": "./application.schema.json"`— y el editor autocompleta las claves, comprueba los tipos, marca las claves huérfanas y muestra cada descripción en su lugar. La raíz generada declara la propiedad `$schema` en sí misma, por lo que la asociación nunca activa la validación estricta que habilita.

## Dónde ir después

La [referencia de configuración](/gokeel/es/reference/conf/) cubre cada símbolo exportado —las opciones, la gramática del marcador de posición y las etiquetas del esquema— y el [libro de recetas](/gokeel/es/cookbook/conf/) recopila recetas listas para copiar, incluyendo cómo reajustar los [niveles de registro](/gokeel/es/guides/log-levels/) de una aplicación en ejecución desde el documento de configuración.
