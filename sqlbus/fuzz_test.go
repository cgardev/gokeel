package sqlbus

import (
	"strings"
	"testing"
	"time"

	"github.com/cgardev/gokeel/eventbus"

	"github.com/google/uuid"
)

// maximumOrderedUnixSeconds bounds the fuzzed instants to the four-digit-year
// era the fixed-width time layout is defined for; wall clocks never leave it.
const maximumOrderedUnixSeconds = 253402300799 // 9999-12-31T23:59:59Z

func clampInstant(seconds int64, nanos int64) time.Time {
	if seconds < 0 {
		seconds = -seconds
	}
	seconds %= maximumOrderedUnixSeconds
	if nanos < 0 {
		nanos = -nanos
	}
	nanos %= int64(time.Second)
	return time.Unix(seconds, nanos).UTC()
}

// FuzzTimeLayoutOrderMatchesChronology fuzzes the property every SQL
// comparison of the schema rests on: the lexicographic order of two formatted
// timestamps equals their chronological order, and formatting round-trips to
// the same instant.
func FuzzTimeLayoutOrderMatchesChronology(f *testing.F) {
	f.Add(int64(0), int64(0), int64(1), int64(0))
	f.Add(int64(1750000000), int64(500000000), int64(1750000000), int64(0))
	f.Add(int64(1750000000), int64(999999999), int64(1750000001), int64(0))

	f.Fuzz(func(t *testing.T, firstSeconds, firstNanos, secondSeconds, secondNanos int64) {
		first := clampInstant(firstSeconds, firstNanos)
		second := clampInstant(secondSeconds, secondNanos)

		formattedFirst := formatTime(first)
		formattedSecond := formatTime(second)

		if first.Before(second) != (formattedFirst < formattedSecond) ||
			first.After(second) != (formattedFirst > formattedSecond) {
			t.Fatalf("lexicographic order of %q and %q diverges from the chronological order of %v and %v",
				formattedFirst, formattedSecond, first, second)
		}

		parsed, err := parseTime(formattedFirst)
		if err != nil {
			t.Fatalf("parse %q: %v", formattedFirst, err)
		}
		if !parsed.Equal(first) {
			t.Fatalf("round trip of %v = %v", first, parsed)
		}
	})
}

// FuzzDeadLetterReferenceRoundTrips fuzzes the opaque dead-letter reference
// codec of the Broker adapter: any delivery key — including instances and
// listener identifiers containing the separator — must survive the round
// trip, and decoding arbitrary garbage must fail cleanly instead of
// panicking.
func FuzzDeadLetterReferenceRoundTrips(f *testing.F) {
	f.Add("", "billing")
	f.Add("node-1", "billing")
	f.Add("a/b", "with/slash")
	f.Add("%2F", "ledger%")

	f.Fuzz(func(t *testing.T, instance, listener string) {
		key := DeliveryKey{
			MessageID:  uuid.New(),
			Instance:   instance,
			ListenerID: eventbus.ListenerID(listener),
		}
		decoded, ok := decodeDeadLetterReference(encodeDeadLetterReference(key))
		if !ok {
			t.Fatalf("reference of %+v did not decode", key)
		}
		if decoded != key {
			t.Fatalf("reference round trip = %+v, want %+v", decoded, key)
		}

		// Arbitrary input must never panic; validity is not required.
		_, _ = decodeDeadLetterReference(instance)
		_, _ = decodeDeadLetterReference(listener + "/" + instance)
	})
}

type fuzzSerialized struct {
	Name  string
	Count int64
}

// FuzzJSONSerializerRoundTrips fuzzes the persistence seam of every event:
// any field content must survive serialization, and deserializing arbitrary
// payloads must fail cleanly instead of panicking.
func FuzzJSONSerializerRoundTrips(f *testing.F) {
	f.Add("plain", int64(1), "junk")
	f.Add("with\"quotes\\and\nnewlines", int64(-9), "{")
	f.Add("\x00\xff☺", int64(0), `{"Name":"tail"`)

	f.Fuzz(func(t *testing.T, name string, count int64, junk string) {
		serializer := NewJSONSerializer()
		if err := RegisterEventType[fuzzSerialized](serializer, "fuzz.serialized"); err != nil {
			t.Fatalf("register event type: %v", err)
		}

		event := fuzzSerialized{Name: name, Count: count}
		eventType, payload, err := serializer.Serialize(event)
		if err != nil {
			// Some strings are not serializable as JSON (invalid UTF-8 is
			// replaced, not rejected, so an error here is unexpected).
			t.Fatalf("serialize %+v: %v", event, err)
		}
		if eventType != "fuzz.serialized" {
			t.Fatalf("event type = %q, want fuzz.serialized", eventType)
		}
		restored, err := serializer.Deserialize(eventType, payload)
		if err != nil {
			t.Fatalf("deserialize %q: %v", payload, err)
		}
		typed, ok := restored.(fuzzSerialized)
		if !ok {
			t.Fatalf("restored type = %T, want fuzzSerialized", restored)
		}
		if typed.Count != event.Count {
			t.Fatalf("count round trip = %d, want %d", typed.Count, event.Count)
		}
		if !strings.ContainsRune(name, '�') && typed.Name != event.Name {
			// Invalid UTF-8 is coerced to the replacement rune by JSON
			// encoding; every valid string must round-trip verbatim.
			if strings.ToValidUTF8(name, "�") != typed.Name {
				t.Fatalf("name round trip = %q, want %q", typed.Name, event.Name)
			}
		}

		// Arbitrary payloads and names must fail cleanly, never panic.
		if _, err := serializer.Deserialize(eventType, junk); err == nil {
			// Junk that happens to be valid JSON for the type is fine.
			_ = err
		}
		if _, err := serializer.Deserialize(junk, payload); err == nil && junk != eventType {
			t.Fatalf("deserializing under unregistered name %q succeeded", junk)
		}
	})
}
