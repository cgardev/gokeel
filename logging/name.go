package logging

import (
	"log/slog"
	"reflect"
	"strings"
)

// separators are the characters that delimit the segments of a hierarchical
// logger name: "/" between the elements of a package import path and "."
// both inside a domain segment and before a type name.
const separators = "/."

// canonicalName normalizes a logger name: surrounding whitespace and
// separators are trimmed to a fixed point, so canonicalization is idempotent
// even for names that interleave both, and the pseudo-name of the root
// logger becomes the empty string, the same aliasing Spring applies to
// "root" and "ROOT".
func canonicalName(name string) string {
	for {
		trimmed := strings.Trim(strings.TrimSpace(name), separators)
		if trimmed == name {
			break
		}
		name = trimmed
	}
	if strings.EqualFold(name, "root") {
		return ""
	}
	return name
}

// displayName renders a canonical name for output, giving the root logger
// the pseudo-name Spring uses to address it.
func displayName(canonical string) string {
	if canonical == "" {
		return "root"
	}
	return canonical
}

// ancestry returns the canonical form of the name followed by every ancestor
// obtained by cutting whole segments from the right, so "a/b.c" yields
// "a/b.c", "a/b", "a". Cutting whole segments keeps siblings with a common
// prefix apart: "a/eventbus" is not a descendant of "a/event". The root
// logger is not part of the chain; it is the fallback the effective-level
// walk ends on.
func ancestry(name string) []string {
	name = canonicalName(name)
	if name == "" {
		return nil
	}
	chain := []string{name}
	for {
		cut := strings.LastIndexAny(name, separators)
		if cut < 0 {
			return chain
		}
		name = strings.Trim(name[:cut], separators)
		if name == "" {
			return chain
		}
		chain = append(chain, name)
	}
}

// NameFor returns the hierarchical logger name of the type T: its package
// import path, a dot, and the type name, the Go analog of Spring's
// LoggerFactory.getLogger(MyClass.class). A pointer type yields the name of
// the type it points to; a type without a package, such as a predeclared
// type, yields its own notation.
func NameFor[T any]() string {
	reflected := reflect.TypeFor[T]()
	for reflected.Kind() == reflect.Pointer {
		reflected = reflected.Elem()
	}
	if reflected.PkgPath() == "" {
		return reflected.String()
	}
	return reflected.PkgPath() + "." + reflected.Name()
}

// LoggerFor returns a logger of the manager named after the type T, so a
// store or service logs under the exact name a configuration document or a
// runtime SetLevel targets, down to the single type.
func LoggerFor[T any](manager *Manager) *slog.Logger {
	return manager.Logger(NameFor[T]())
}
