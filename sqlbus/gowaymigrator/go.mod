module github.com/cgardev/gokeel/sqlbus/gowaymigrator

go 1.26.3

require (
	github.com/cgardev/gokeel/sqlbus v0.0.0-20260703104738-2c7d202df10e
	github.com/cgardev/goway v0.0.0-20260531141847-626204f558ed
	modernc.org/sqlite v1.52.0
)

require (
	github.com/cgardev/gokeel/eventbus v0.0.0-20260703104738-2c7d202df10e // indirect
	github.com/cgardev/gokeel/transaction v0.0.0-20260703104738-2c7d202df10e // indirect
	github.com/cgardev/gooq v0.0.0-20260531151630-ef8fc4b12d37 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.45.0 // indirect
	modernc.org/libc v1.73.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

// Intra-repository edges. The adapter is a SEPARATE module, and relative replace
// directives are NOT transitive to a consumer: sqlbus/go.mod's own replaces for
// transaction and eventbus do NOT reach this module, so the adapter restates them
// itself while the family has no published tags. Paths are relative to
// sqlbus/gowaymigrator/, so sqlbus is one level up and the leaves are two.
replace github.com/cgardev/gokeel/sqlbus => ../

replace github.com/cgardev/gokeel/transaction => ../../transaction

replace github.com/cgardev/gokeel/eventbus => ../../eventbus
