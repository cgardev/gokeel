package conf

import (
	"errors"
	"fmt"
	"strings"
)

// The placeholder grammar mirrors the Spring PropertyPlaceholderHelper: the
// ${ prefix, the } suffix, the first unescaped colon separating a key from
// its default value, and the backslash escape Spring Framework 6.2
// introduced for rendering a literal ${.
const (
	placeholderPrefix = "${"
	placeholderSuffix = "}"
	valueSeparator    = ':'
	escapeCharacter   = '\\'
)

var (
	// ErrUnresolvedPlaceholder reports a ${name} with no value in the
	// environment or the document and no default after a colon: the
	// analog of the PlaceholderResolutionException that aborts a Spring
	// Boot startup. The placeholder name is attached to the returned
	// error.
	ErrUnresolvedPlaceholder = errors.New("placeholder could not be resolved")

	// ErrCircularPlaceholder reports a placeholder whose resolution leads
	// back to itself, directly or through other placeholders. Spring
	// detects the same condition and reports a circular placeholder
	// reference.
	ErrCircularPlaceholder = errors.New("placeholder references form a cycle")
)

// resolveTree substitutes the placeholders of every string in the tree,
// returning a new tree; maps and slices are rebuilt, keys are left
// untouched, and non-string scalars pass through unchanged.
func resolveTree(value any, lookup func(name string) (string, bool)) (any, error) {
	switch typed := value.(type) {
	case string:
		return resolveText(typed, lookup, map[string]bool{})
	case map[string]any:
		resolved := make(map[string]any, len(typed))
		for key, child := range typed {
			resolvedChild, err := resolveTree(child, lookup)
			if err != nil {
				return nil, err
			}
			resolved[key] = resolvedChild
		}
		return resolved, nil
	case []any:
		resolved := make([]any, len(typed))
		for index, child := range typed {
			resolvedChild, err := resolveTree(child, lookup)
			if err != nil {
				return nil, err
			}
			resolved[index] = resolvedChild
		}
		return resolved, nil
	default:
		return value, nil
	}
}

// resolveText substitutes every placeholder in one string. The visited set
// carries the keys currently being resolved, so a chain that returns to an
// earlier key is reported instead of recursing forever.
func resolveText(text string, lookup func(name string) (string, bool), visited map[string]bool) (string, error) {
	var builder strings.Builder
	for index := 0; index < len(text); {
		if text[index] == escapeCharacter && strings.HasPrefix(text[index+1:], placeholderPrefix) {
			// The escape renders the prefix literally and is itself
			// removed from the output, as in Spring.
			builder.WriteString(placeholderPrefix)
			index += 1 + len(placeholderPrefix)
			continue
		}
		if strings.HasPrefix(text[index:], placeholderPrefix) {
			content, length, terminated := placeholderAt(text[index:])
			if !terminated {
				// An unterminated prefix is literal text, the
				// behavior of the Spring parser.
				builder.WriteString(text[index:])
				break
			}
			resolved, err := resolvePlaceholder(content, lookup, visited)
			if err != nil {
				return "", fmt.Errorf("%w in value %q", err, text)
			}
			builder.WriteString(resolved)
			index += length
			continue
		}
		builder.WriteByte(text[index])
		index++
	}
	return builder.String(), nil
}

// placeholderAt reports the content and total length of the placeholder that
// starts at the beginning of text, tracking brace depth so a nested
// placeholder inside a key or a default pairs its braces correctly.
func placeholderAt(text string) (content string, length int, terminated bool) {
	depth := 0
	for index := len(placeholderPrefix); index < len(text); index++ {
		switch text[index] {
		case '{':
			depth++
		case '}':
			if depth == 0 {
				return text[len(placeholderPrefix):index], index + len(placeholderSuffix), true
			}
			depth--
		}
	}
	return "", 0, false
}

// resolvePlaceholder resolves the content between one ${ and its }. The key
// may itself contain placeholders, the default after the first unescaped
// colon is resolved only when the key has no value, and a value obtained for
// the key is resolved recursively — the three nesting forms Spring supports.
func resolvePlaceholder(content string, lookup func(name string) (string, bool), visited map[string]bool) (string, error) {
	keyText, defaultText, hasDefault := splitPlaceholder(content)

	key, err := resolveText(keyText, lookup, visited)
	if err != nil {
		return "", err
	}

	if value, ok := lookup(key); ok {
		if visited[key] {
			return "", fmt.Errorf("%w: %q", ErrCircularPlaceholder, key)
		}
		visited[key] = true
		resolved, err := resolveText(value, lookup, visited)
		delete(visited, key)
		return resolved, err
	}
	if hasDefault {
		return resolveText(defaultText, lookup, visited)
	}
	return "", fmt.Errorf("%w: %q", ErrUnresolvedPlaceholder, key)
}

// splitPlaceholder cuts the placeholder content at the first unescaped colon
// outside any nested braces. An escaped colon stays part of the key with its
// escape removed, so a key may itself contain a colon, as in Spring; inside
// nested braces the escape is preserved verbatim, because it belongs to the
// nested placeholder and is consumed by its own split.
func splitPlaceholder(content string) (key string, defaultValue string, hasDefault bool) {
	var builder strings.Builder
	depth := 0
	for index := 0; index < len(content); index++ {
		character := content[index]
		switch {
		case character == escapeCharacter && index+1 < len(content) && content[index+1] == valueSeparator:
			if depth > 0 {
				builder.WriteByte(escapeCharacter)
			}
			builder.WriteByte(valueSeparator)
			index++
		case character == '{':
			depth++
			builder.WriteByte(character)
		case character == '}':
			depth--
			builder.WriteByte(character)
		case character == valueSeparator && depth == 0:
			return builder.String(), content[index+1:], true
		default:
			builder.WriteByte(character)
		}
	}
	return builder.String(), "", false
}
