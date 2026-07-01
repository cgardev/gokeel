package conf

import (
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrUnknownKey reports a document key that matches no field of the
	// target struct. The full dotted path is attached to the returned
	// error.
	ErrUnknownKey = errors.New("configuration key is not recognized")

	// ErrTypeMismatch reports a document value that cannot be converted
	// to the type of the field it binds to, the analog of the
	// BindException a Spring Boot binding failure raises. The path and
	// both types are attached to the returned error.
	ErrTypeMismatch = errors.New("configuration value cannot be converted")
)

var (
	durationType        = reflect.TypeFor[time.Duration]()
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// bind overlays the resolved document tree onto target, which must be a
// non-nil pointer to a struct.
func bind(tree map[string]any, target any) error {
	pointer := reflect.ValueOf(target)
	if pointer.Kind() != reflect.Pointer || pointer.IsNil() {
		return errors.New("configuration target must be a non-nil pointer to a struct")
	}
	value := pointer.Elem()
	if value.Kind() != reflect.Struct {
		return fmt.Errorf("configuration target must point to a struct, not %s", value.Kind())
	}
	return bindValue(value, tree, "")
}

// bindValue binds one document value onto one target value, converting with
// the same leniency the relaxed binding of Spring applies: strings convert
// to numbers, booleans, durations, and any type that unmarshals text. A JSON
// null leaves the current value in place, so defaults survive.
func bindValue(target reflect.Value, value any, path string) error {
	if value == nil {
		return nil
	}

	if target.Kind() == reflect.Pointer {
		if target.IsNil() {
			target.Set(reflect.New(target.Type().Elem()))
		}
		return bindValue(target.Elem(), value, path)
	}

	if target.Type() == durationType {
		return bindDuration(target, value, path)
	}
	if text, ok := value.(string); ok {
		if unmarshaler, ok := textUnmarshaler(target); ok {
			if err := unmarshaler.UnmarshalText([]byte(text)); err != nil {
				return mismatch(path, value, target.Type(), err)
			}
			return nil
		}
	}

	switch target.Kind() {
	case reflect.Interface:
		if target.NumMethod() != 0 {
			return mismatch(path, value, target.Type(), nil)
		}
		target.Set(reflect.ValueOf(normalize(value)))
		return nil
	case reflect.Bool:
		return bindBool(target, value, path)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return bindInteger(target, value, path)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return bindUnsigned(target, value, path)
	case reflect.Float32, reflect.Float64:
		return bindFloat(target, value, path)
	case reflect.String:
		return bindString(target, value, path)
	case reflect.Slice:
		return bindSlice(target, value, path)
	case reflect.Array:
		return bindArray(target, value, path)
	case reflect.Map:
		return bindMap(target, value, path)
	case reflect.Struct:
		tree, ok := value.(map[string]any)
		if !ok {
			return mismatch(path, value, target.Type(), nil)
		}
		return bindStruct(target, tree, path)
	default:
		return mismatch(path, value, target.Type(), nil)
	}
}

// bindStruct binds a document object onto a struct. Field names come from
// the json tags, embedded structs are promoted the way encoding/json
// promotes them, and a key that matches no field fails with ErrUnknownKey.
// Fields the object does not mention keep their current values.
func bindStruct(target reflect.Value, tree map[string]any, path string) error {
	fields := structFields(target.Type())
	for key, child := range tree {
		field, ok := fields[key]
		if !ok {
			return fmt.Errorf("%w: %s", ErrUnknownKey, childPath(path, key))
		}
		fieldValue, err := fieldByIndex(target, field.Index, childPath(path, key))
		if err != nil {
			return err
		}
		if err := bindValue(fieldValue, child, childPath(path, key)); err != nil {
			return err
		}
	}
	return nil
}

// fieldCandidate is one field competing for a document name, together with
// whether its json tag named it explicitly.
type fieldCandidate struct {
	field  reflect.StructField
	tagged bool
}

// structFields indexes the bindable fields of a struct type by their
// document names. Embedded structs without a json name contribute their
// promoted fields, an embedded struct with a json name binds as a nested
// object under that name, and an embedded struct tagged "-" is excluded
// together with everything it would promote. Name conflicts follow the
// encoding/json rules: the field at the shallowest embedding depth wins, at
// equal depth a single explicitly tagged field wins, and an ambiguous name
// is not bindable at all, so a document key naming it fails with
// ErrUnknownKey.
func structFields(structType reflect.Type) map[string]reflect.StructField {
	candidates := map[string][]fieldCandidate{}
	var containers [][]int
	for _, field := range reflect.VisibleFields(structType) {
		if insideContainer(field.Index, containers) {
			continue
		}
		name, tagged := jsonName(field)
		anonymousStruct := field.Anonymous && dereference(field.Type).Kind() == reflect.Struct
		if name == "-" {
			if anonymousStruct {
				// An excluded embedded struct must not leak its
				// promoted fields either.
				containers = append(containers, field.Index)
			}
			continue
		}
		if field.PkgPath != "" {
			// An unexported field is not bindable. An unexported
			// embedded struct still promotes its exported fields,
			// which appear on their own in the walk — unless a
			// json tag names the embedded struct itself, a value
			// reflection cannot bind into, so the whole subtree is
			// excluded, as it is by encoding/json.
			if anonymousStruct && tagged {
				containers = append(containers, field.Index)
			}
			continue
		}
		if anonymousStruct && !tagged {
			// A plain embedded struct is a container: its promoted
			// fields appear on their own in the walk.
			continue
		}
		if field.Anonymous && tagged {
			// A named embedded struct binds as one nested object,
			// so its children must not also bind as promoted
			// fields.
			containers = append(containers, field.Index)
		}
		candidates[name] = append(candidates[name], fieldCandidate{field: field, tagged: tagged})
	}

	fields := map[string]reflect.StructField{}
	for name, competing := range candidates {
		if winner, ok := dominantField(competing); ok {
			fields[name] = winner
		}
	}
	return fields
}

// dominantField applies the name-conflict rules of encoding/json: the
// shallowest embedding depth wins, a single explicitly tagged field breaks a
// tie, and anything else is ambiguous and dropped.
func dominantField(candidates []fieldCandidate) (reflect.StructField, bool) {
	shallowest := len(candidates[0].field.Index)
	for _, candidate := range candidates[1:] {
		if depth := len(candidate.field.Index); depth < shallowest {
			shallowest = depth
		}
	}
	var atDepth, tagged []fieldCandidate
	for _, candidate := range candidates {
		if len(candidate.field.Index) != shallowest {
			continue
		}
		atDepth = append(atDepth, candidate)
		if candidate.tagged {
			tagged = append(tagged, candidate)
		}
	}
	if len(atDepth) == 1 {
		return atDepth[0].field, true
	}
	if len(tagged) == 1 {
		return tagged[0].field, true
	}
	return reflect.StructField{}, false
}

// jsonName reports the document name of a field and whether the json tag
// named it explicitly.
func jsonName(field reflect.StructField) (name string, tagged bool) {
	tag, _, _ := strings.Cut(field.Tag.Get("json"), ",")
	if tag == "" {
		return field.Name, false
	}
	return tag, true
}

// insideContainer reports whether the index path descends into a field
// already claimed as one nested object.
func insideContainer(index []int, containers [][]int) bool {
	for _, container := range containers {
		if len(index) <= len(container) {
			continue
		}
		matches := true
		for position, step := range container {
			if index[position] != step {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

// fieldByIndex walks an index path, allocating any nil embedded pointer on
// the way, so a promoted field behind an embedded pointer is bindable. A nil
// embedded pointer to an unexported struct type cannot be allocated through
// reflection; encoding/json reports the same shape as an error, and so does
// this walk instead of panicking.
func fieldByIndex(target reflect.Value, index []int, path string) (reflect.Value, error) {
	for _, step := range index {
		if target.Kind() == reflect.Pointer {
			if target.IsNil() {
				if !target.CanSet() {
					return reflect.Value{}, fmt.Errorf(
						"cannot allocate the nil embedded pointer %s on the way to %s", target.Type(), path)
				}
				target.Set(reflect.New(target.Type().Elem()))
			}
			target = target.Elem()
		}
		target = target.Field(step)
	}
	return target, nil
}

// bindDuration accepts the Go duration notation, such as "1m30s", and a raw
// JSON number of nanoseconds. Spring instead reads a bare number as
// milliseconds and offers a simple value-and-unit form beside ISO-8601; the
// Go notation is the native convention of time.ParseDuration, adding the
// compound values the Spring simple form rejects while lacking its day unit.
func bindDuration(target reflect.Value, value any, path string) error {
	switch typed := value.(type) {
	case string:
		duration, err := time.ParseDuration(strings.TrimSpace(typed))
		if err != nil {
			return mismatch(path, value, durationType, err)
		}
		target.SetInt(int64(duration))
		return nil
	case json.Number:
		nanoseconds, err := typed.Int64()
		if err != nil {
			return mismatch(path, value, durationType, err)
		}
		target.SetInt(nanoseconds)
		return nil
	default:
		return mismatch(path, value, durationType, nil)
	}
}

// bindBool accepts JSON booleans and the token sets of the Spring
// StringToBooleanConverter: true, on, yes, and 1 are true; false, off, no,
// and 0 are false, in any case.
func bindBool(target reflect.Value, value any, path string) error {
	switch typed := value.(type) {
	case bool:
		target.SetBool(typed)
		return nil
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true", "on", "yes", "1":
			target.SetBool(true)
			return nil
		case "false", "off", "no", "0":
			target.SetBool(false)
			return nil
		}
		return mismatch(path, value, target.Type(), nil)
	default:
		return mismatch(path, value, target.Type(), nil)
	}
}

func bindInteger(target reflect.Value, value any, path string) error {
	text, err := numericText(value, path, target.Type())
	if err != nil {
		return err
	}
	parsed, parseErr := strconv.ParseInt(text, 10, 64)
	if parseErr != nil || target.OverflowInt(parsed) {
		return mismatch(path, value, target.Type(), parseErr)
	}
	target.SetInt(parsed)
	return nil
}

func bindUnsigned(target reflect.Value, value any, path string) error {
	text, err := numericText(value, path, target.Type())
	if err != nil {
		return err
	}
	parsed, parseErr := strconv.ParseUint(text, 10, 64)
	if parseErr != nil || target.OverflowUint(parsed) {
		return mismatch(path, value, target.Type(), parseErr)
	}
	target.SetUint(parsed)
	return nil
}

func bindFloat(target reflect.Value, value any, path string) error {
	text, err := numericText(value, path, target.Type())
	if err != nil {
		return err
	}
	parsed, parseErr := strconv.ParseFloat(text, 64)
	if parseErr != nil || target.OverflowFloat(parsed) {
		return mismatch(path, value, target.Type(), parseErr)
	}
	target.SetFloat(parsed)
	return nil
}

// numericText renders a document number or string as the text a strconv
// parser reads, so "8080" and 8080 bind the same way.
func numericText(value any, path string, targetType reflect.Type) (string, error) {
	switch typed := value.(type) {
	case json.Number:
		return typed.String(), nil
	case string:
		return strings.TrimSpace(typed), nil
	default:
		return "", mismatch(path, value, targetType, nil)
	}
}

// bindString accepts strings and renders scalars as text, the way every
// Spring property value is convertible to a string.
func bindString(target reflect.Value, value any, path string) error {
	switch typed := value.(type) {
	case string:
		target.SetString(typed)
		return nil
	case json.Number:
		target.SetString(typed.String())
		return nil
	case bool:
		target.SetString(strconv.FormatBool(typed))
		return nil
	default:
		return mismatch(path, value, target.Type(), nil)
	}
}

func bindSlice(target reflect.Value, value any, path string) error {
	elements, ok := value.([]any)
	if !ok {
		return mismatch(path, value, target.Type(), nil)
	}
	slice := reflect.MakeSlice(target.Type(), len(elements), len(elements))
	for index, element := range elements {
		if err := bindValue(slice.Index(index), element, elementPath(path, index)); err != nil {
			return err
		}
	}
	target.Set(slice)
	return nil
}

func bindArray(target reflect.Value, value any, path string) error {
	elements, ok := value.([]any)
	if !ok || len(elements) != target.Len() {
		return mismatch(path, value, target.Type(), nil)
	}
	for index, element := range elements {
		if err := bindValue(target.Index(index), element, elementPath(path, index)); err != nil {
			return err
		}
	}
	return nil
}

func bindMap(target reflect.Value, value any, path string) error {
	tree, ok := value.(map[string]any)
	if !ok || target.Type().Key().Kind() != reflect.String {
		return mismatch(path, value, target.Type(), nil)
	}
	result := reflect.MakeMapWithSize(target.Type(), len(tree))
	for key, child := range tree {
		element := reflect.New(target.Type().Elem()).Elem()
		if err := bindValue(element, child, childPath(path, key)); err != nil {
			return err
		}
		result.SetMapIndex(reflect.ValueOf(key).Convert(target.Type().Key()), element)
	}
	target.Set(result)
	return nil
}

// textUnmarshaler reports the encoding.TextUnmarshaler behind an addressable
// target, covering types such as time.Time and slog.Level.
func textUnmarshaler(target reflect.Value) (encoding.TextUnmarshaler, bool) {
	if !target.CanAddr() || !reflect.PointerTo(target.Type()).Implements(textUnmarshalerType) {
		return nil, false
	}
	unmarshaler, ok := target.Addr().Interface().(encoding.TextUnmarshaler)
	return unmarshaler, ok
}

// normalize renders a document value for an untyped target: json.Number
// becomes int64 when integral and float64 otherwise, and containers are
// rebuilt with their children normalized.
func normalize(value any) any {
	switch typed := value.(type) {
	case json.Number:
		if integer, err := typed.Int64(); err == nil {
			return integer
		}
		float, err := typed.Float64()
		if err != nil {
			return typed.String()
		}
		return float
	case map[string]any:
		normalized := make(map[string]any, len(typed))
		for key, child := range typed {
			normalized[key] = normalize(child)
		}
		return normalized
	case []any:
		normalized := make([]any, len(typed))
		for index, child := range typed {
			normalized[index] = normalize(child)
		}
		return normalized
	default:
		return value
	}
}

func dereference(fieldType reflect.Type) reflect.Type {
	for fieldType.Kind() == reflect.Pointer {
		fieldType = fieldType.Elem()
	}
	return fieldType
}

func childPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func elementPath(path string, index int) string {
	return path + "[" + strconv.Itoa(index) + "]"
}

func mismatch(path string, value any, targetType reflect.Type, cause error) error {
	if cause != nil {
		return fmt.Errorf("%w: %v does not fit %s at %s: %w", ErrTypeMismatch, value, targetType, path, cause)
	}
	return fmt.Errorf("%w: %v does not fit %s at %s", ErrTypeMismatch, value, targetType, path)
}
