package conf

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type conversionsConfiguration struct {
	Port     int           `json:"port"`
	Ratio    float64       `json:"ratio"`
	Verbose  bool          `json:"verbose"`
	Timeout  time.Duration `json:"timeout"`
	Started  time.Time     `json:"started"`
	Level    slog.Level    `json:"level"`
	Anything any           `json:"anything"`
}

func loadDocument(t *testing.T, document string, target any) error {
	t.Helper()
	return NewLoader(WithDocument([]byte(document)), WithLookup(emptyLookup)).Load(target)
}

func TestStringsConvertToNumbersAndBooleans(t *testing.T) {
	var target conversionsConfiguration
	err := loadDocument(t, `{
		"port": "8080",
		"ratio": "0.75",
		"verbose": "yes",
		"timeout": "1s",
		"started": "2026-07-01T10:00:00Z",
		"level": "info",
		"anything": null
	}`, &target)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if target.Port != 8080 || target.Ratio != 0.75 {
		t.Errorf("numbers = %d, %g, want the strings converted", target.Port, target.Ratio)
	}
	if !target.Verbose {
		t.Error("verbose = false: the Spring token \"yes\" must convert to true")
	}
}

func TestSpringBooleanTokensConvert(t *testing.T) {
	tokens := []struct {
		token string
		value bool
	}{
		{"true", true}, {"on", true}, {"yes", true}, {"1", true},
		{"false", false}, {"off", false}, {"no", false}, {"0", false},
		{"ON", true}, {" Off ", false},
	}
	for _, entry := range tokens {
		var target struct {
			Flag bool `json:"flag"`
		}
		if err := loadDocument(t, `{"flag": "`+entry.token+`"}`, &target); err != nil {
			t.Fatalf("token %q: %v", entry.token, err)
		}
		if target.Flag != entry.value {
			t.Errorf("token %q bound as %t, want %t", entry.token, target.Flag, entry.value)
		}
	}
}

func TestDurationsParseFromGoNotationAndNanoseconds(t *testing.T) {
	var fromText conversionsConfiguration
	if err := loadDocument(t, `{"port":1,"ratio":1,"verbose":true,"timeout":"1m30s","started":"2026-07-01T10:00:00Z","level":"warn","anything":1}`, &fromText); err != nil {
		t.Fatalf("load: %v", err)
	}
	if fromText.Timeout != 90*time.Second {
		t.Errorf("timeout = %v, want the compound Go notation parsed", fromText.Timeout)
	}

	var fromNumber struct {
		Timeout time.Duration `json:"timeout"`
	}
	if err := loadDocument(t, `{"timeout": 1500000000}`, &fromNumber); err != nil {
		t.Fatalf("load: %v", err)
	}
	if fromNumber.Timeout != 1500*time.Millisecond {
		t.Errorf("timeout = %v, want a raw number read as nanoseconds", fromNumber.Timeout)
	}
}

func TestTextUnmarshalerFieldsBindFromStrings(t *testing.T) {
	var target conversionsConfiguration
	err := loadDocument(t, `{"port":1,"ratio":1,"verbose":true,"timeout":"1s","started":"2026-07-01T10:00:00Z","level":"warn","anything":1}`, &target)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if target.Level != slog.LevelWarn {
		t.Errorf("level = %v, want slog.Level bound through UnmarshalText", target.Level)
	}
	if target.Started.IsZero() || target.Started.Hour() != 10 {
		t.Errorf("started = %v, want the RFC 3339 timestamp bound through UnmarshalText", target.Started)
	}
}

func TestAnUnknownKeyFailsWithItsFullPath(t *testing.T) {
	var target applicationConfiguration
	err := loadDocument(t, `{"name": "shop", "server": {"host": "h", "port": 1, "portt": 2}}`, &target)

	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("load error = %v, want ErrUnknownKey", err)
	}
	if !strings.Contains(err.Error(), "server.portt") {
		t.Errorf("error %q does not name the full path of the unknown key", err)
	}
}

func TestATypeMismatchNamesThePath(t *testing.T) {
	var target applicationConfiguration
	err := loadDocument(t, `{"name": "shop", "server": {"host": "h", "port": "not-a-number"}}`, &target)

	if !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("load error = %v, want ErrTypeMismatch", err)
	}
	if !strings.Contains(err.Error(), "server.port") {
		t.Errorf("error %q does not name the path of the mismatched value", err)
	}
}

func TestMapsPointersAndNestedSlicesBind(t *testing.T) {
	var target struct {
		Limits   map[string]int `json:"limits"`
		Optional *int           `json:"optional"`
		Matrix   [][]string     `json:"matrix"`
	}
	err := loadDocument(t, `{
		"limits": {"orders": 10, "billing": 20},
		"optional": 42,
		"matrix": [["a"], ["b", "c"]]
	}`, &target)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if target.Limits["orders"] != 10 || target.Limits["billing"] != 20 {
		t.Errorf("limits = %v, want both map entries bound", target.Limits)
	}
	if target.Optional == nil || *target.Optional != 42 {
		t.Errorf("optional = %v, want the pointer allocated and set", target.Optional)
	}
	if len(target.Matrix) != 2 || target.Matrix[1][1] != "c" {
		t.Errorf("matrix = %v, want the nested slices bound", target.Matrix)
	}
}

func TestEmbeddedStructFieldsArePromoted(t *testing.T) {
	type common struct {
		Region string `json:"region"`
	}
	var target struct {
		common
		Name string `json:"name"`
	}
	if err := loadDocument(t, `{"region": "eu-west", "name": "shop"}`, &target); err != nil {
		t.Fatalf("load: %v", err)
	}
	if target.Region != "eu-west" {
		t.Errorf("region = %q, want the promoted embedded field bound", target.Region)
	}
}

func TestANamedEmbeddedStructBindsAsANestedObject(t *testing.T) {
	type common struct {
		Region string `json:"region"`
	}
	var target struct {
		Common common `json:"common"`
		Name   string `json:"name"`
	}
	if err := loadDocument(t, `{"common": {"region": "eu-west"}, "name": "shop"}`, &target); err != nil {
		t.Fatalf("load: %v", err)
	}
	if target.Common.Region != "eu-west" {
		t.Errorf("region = %q, want the nested object bound under its name", target.Common.Region)
	}
}

func TestFixedSizeArraysBindAndRejectLengthMismatches(t *testing.T) {
	var target struct {
		Pair [2]string `json:"pair"`
	}
	if err := loadDocument(t, `{"pair": ["a", "b"]}`, &target); err != nil {
		t.Fatalf("load: %v", err)
	}
	if target.Pair[0] != "a" || target.Pair[1] != "b" {
		t.Errorf("pair = %q, want both elements bound", target.Pair)
	}

	if err := loadDocument(t, `{"pair": ["a"]}`, &target); !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("short array error = %v, want ErrTypeMismatch", err)
	}
	if err := loadDocument(t, `{"pair": ["a", "b", "c"]}`, &target); !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("long array error = %v, want ErrTypeMismatch", err)
	}
}

func TestAShallowerFieldWinsAJSONNameConflict(t *testing.T) {
	type inner struct {
		InnerName string `json:"host"`
	}
	var promoted struct {
		OuterName string `json:"host"`
		inner
	}
	if err := loadDocument(t, `{"host": "value"}`, &promoted); err != nil {
		t.Fatalf("load: %v", err)
	}
	if promoted.OuterName != "value" || promoted.InnerName != "" {
		t.Errorf("bound fields = %q, %q: the shallower field must win, as in encoding/json",
			promoted.OuterName, promoted.InnerName)
	}

	var reordered struct {
		inner
		OuterName string `json:"host"`
	}
	if err := loadDocument(t, `{"host": "value"}`, &reordered); err != nil {
		t.Fatalf("load: %v", err)
	}
	if reordered.OuterName != "value" || reordered.InnerName != "" {
		t.Errorf("bound fields = %q, %q: the outcome must not depend on declaration order",
			reordered.OuterName, reordered.InnerName)
	}
}

func TestAnAmbiguousJSONNameIsNotBindable(t *testing.T) {
	type left struct {
		Host string
	}
	type right struct {
		Host string
	}
	var target struct {
		left
		right
	}
	err := loadDocument(t, `{"Host": "value"}`, &target)
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("load error = %v, want ErrUnknownKey: an ambiguous name must not bind into an arbitrary side", err)
	}
}

func TestAnEmbeddedStructTaggedDashIsExcluded(t *testing.T) {
	type hidden struct {
		Secret string `json:"secret"`
	}
	var embedded struct {
		hidden `json:"-"`
		Name   string `json:"name"`
	}
	err := loadDocument(t, `{"secret": "leaked", "name": "n"}`, &embedded)
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("load error = %v, want ErrUnknownKey: the excluded subtree must not bind", err)
	}
	if embedded.Secret != "" {
		t.Errorf("secret = %q, want the excluded field untouched", embedded.Secret)
	}
}

func TestANilEmbeddedPointerToAnUnexportedStructFailsInsteadOfPanicking(t *testing.T) {
	type inner struct {
		Host string `json:"host"`
	}
	var target struct {
		*inner
	}
	err := loadDocument(t, `{"host": "h"}`, &target)
	if err == nil {
		t.Fatal("a nil embedded pointer to an unexported struct type was silently accepted")
	}
	if !strings.Contains(err.Error(), "host") {
		t.Errorf("error %q does not name the field being reached", err)
	}
}

func TestNullLeavesTheCurrentValueInPlace(t *testing.T) {
	target := applicationConfiguration{Name: "preset"}
	if err := loadDocument(t, `{"name": null, "server": {"host": "h", "port": 1}}`, &target); err != nil {
		t.Fatalf("load: %v", err)
	}
	if target.Name != "preset" {
		t.Errorf("name = %q, want the preset value kept for a JSON null", target.Name)
	}
}

func TestLargeIntegersKeepTheirPrecision(t *testing.T) {
	var target struct {
		Identifier int64 `json:"identifier"`
		Anything   any   `json:"anything"`
	}
	if err := loadDocument(t, `{"identifier": 9007199254740993, "anything": 9007199254740993}`, &target); err != nil {
		t.Fatalf("load: %v", err)
	}

	if target.Identifier != 9007199254740993 {
		t.Errorf("identifier = %d, want the integer beyond float64 precision preserved", target.Identifier)
	}
	if got, ok := target.Anything.(int64); !ok || got != 9007199254740993 {
		t.Errorf("anything = %v (%T), want an int64 preserving the value", target.Anything, target.Anything)
	}
}
