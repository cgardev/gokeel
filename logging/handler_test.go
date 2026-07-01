package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
	"testing/slogtest"
)

// decodeLines parses each JSON line a slog.JSONHandler wrote into a map, so
// tests can assert on the exact attributes of the emitted records.
func decodeLines(t *testing.T, buffer *bytes.Buffer) []map[string]any {
	t.Helper()
	var entries []map[string]any
	for _, line := range bytes.Split(buffer.Bytes(), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("decode output line %q: %v", line, err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func TestHandlerConformsToTheSlogHandlerContract(t *testing.T) {
	var buffer bytes.Buffer
	manager := NewManager(
		WithHandler(slog.NewJSONHandler(&buffer, nil)),
		WithRootLevel(LevelTrace),
		WithNameKey(""),
	)
	handler := manager.Handler("github.com/acme/application")

	results := func() []map[string]any {
		return decodeLines(t, &buffer)
	}
	if err := slogtest.TestHandler(handler, results); err != nil {
		t.Fatalf("handler violates the slog contract: %v", err)
	}
}

func TestEveryRecordCarriesTheLoggerNameAttribute(t *testing.T) {
	var buffer bytes.Buffer
	manager := NewManager(WithHandler(slog.NewJSONHandler(&buffer, nil)))

	manager.Logger("github.com/acme/storage").Info("named")
	manager.Logger("root").Info("root named")

	entries := decodeLines(t, &buffer)
	if len(entries) != 2 {
		t.Fatalf("emitted %d records, want 2", len(entries))
	}
	if got := entries[0]["logger"]; got != "github.com/acme/storage" {
		t.Errorf("logger attribute = %v, want the hierarchical name", got)
	}
	if got := entries[1]["logger"]; got != "root" {
		t.Errorf("root logger attribute = %v, want %q", got, "root")
	}
}

func TestWithNameKeyRenamesAndDisablesTheAttribute(t *testing.T) {
	var renamed bytes.Buffer
	NewManager(
		WithHandler(slog.NewJSONHandler(&renamed, nil)),
		WithNameKey("component"),
	).Logger("github.com/acme/storage").Info("renamed key")

	entries := decodeLines(t, &renamed)
	if got := entries[0]["component"]; got != "github.com/acme/storage" {
		t.Errorf("component attribute = %v, want the hierarchical name", got)
	}

	var disabled bytes.Buffer
	NewManager(
		WithHandler(slog.NewJSONHandler(&disabled, nil)),
		WithNameKey(""),
	).Logger("github.com/acme/storage").Info("no name attribute")

	entries = decodeLines(t, &disabled)
	if _, present := entries[0]["logger"]; present {
		t.Error("the logger attribute is present although the empty key disables it")
	}
}

func TestWithAttrsAndWithGroupPreserveLevelFiltering(t *testing.T) {
	recorder := &recordingHandler{}
	manager := NewManager(WithHandler(recorder))
	logger := manager.Logger("github.com/acme").With("request", "r-1").WithGroup("details")

	logger.Debug("dropped while the root level holds")
	manager.SetLevel("github.com/acme", slog.LevelDebug)
	logger.Debug("emitted after the runtime change")

	if got := recorder.count(); got != 1 {
		t.Fatalf("recorded %d records, want exactly the one emitted after SetLevel", got)
	}
}

func TestTheNameAttributeSurvivesAttributeGrouping(t *testing.T) {
	var buffer bytes.Buffer
	manager := NewManager(WithHandler(slog.NewJSONHandler(&buffer, nil)))

	manager.Logger("github.com/acme/storage").WithGroup("details").Info("grouped", "key", "value")

	entries := decodeLines(t, &buffer)
	if got := entries[0]["logger"]; got != "github.com/acme/storage" {
		t.Errorf("top-level logger attribute = %v, want the name outside the attribute group", got)
	}
	details, ok := entries[0]["details"].(map[string]any)
	if !ok || details["key"] != "value" {
		t.Errorf("grouped attributes = %v, want key nested under details", entries[0]["details"])
	}
}

func TestStandardLoggerStampsTheConfiguredLevel(t *testing.T) {
	recorder := &recordingHandler{}
	manager := NewManager(WithHandler(recorder))

	manager.StandardLogger("github.com/acme/legacy", slog.LevelWarn).Print("stamped at warn")

	if got := recorder.count(); got != 1 {
		t.Fatalf("recorded %d records, want 1", got)
	}
	if got := recorder.levels()[0]; got != slog.LevelWarn {
		t.Errorf("bridged record level = %v, want the level the bridge was constructed with, %v", got, slog.LevelWarn)
	}
}

func TestStandardLoggerBridgesTheClassicLogPackage(t *testing.T) {
	recorder := &recordingHandler{}
	manager := NewManager(WithHandler(recorder))
	bridge := manager.StandardLogger("github.com/acme/legacy", slog.LevelInfo)

	bridge.Print("delivered through the bridge")
	manager.SetLevel("github.com/acme/legacy", LevelOff)
	bridge.Print("silenced by the tree")

	if got := recorder.count(); got != 1 {
		t.Fatalf("recorded %d records, want only the message sent before OFF", got)
	}
	if got := recorder.messages()[0]; got != "delivered through the bridge" {
		t.Errorf("bridged message = %q, want the printed text", got)
	}
}
