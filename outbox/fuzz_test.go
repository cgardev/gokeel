package outbox

import (
	"testing"
	"time"
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

// FuzzTimeLayoutOrderMatchesChronology fuzzes the property the incomplete-
// before filter rests on: the lexicographic order of two formatted timestamps
// equals their chronological order, and formatting round-trips to the same
// instant.
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

type fuzzPayload struct {
	Body string
}

// FuzzJSONSerializerRoundTrips fuzzes the persistence seam of every
// publication: serialization must round-trip, and deserializing arbitrary
// names and payloads must fail cleanly instead of panicking.
func FuzzJSONSerializerRoundTrips(f *testing.F) {
	f.Add("plain", "junk")
	f.Add("with\"quotes\\and\nnewlines", "{")
	f.Add("\x00\xff☺", `{"Body":`)

	f.Fuzz(func(t *testing.T, body string, junk string) {
		serializer := NewJSONSerializer()
		if err := RegisterEventType[fuzzPayload](serializer, "fuzz.payload"); err != nil {
			t.Fatalf("register event type: %v", err)
		}

		eventType, payload, err := serializer.Serialize(fuzzPayload{Body: body})
		if err != nil {
			t.Fatalf("serialize %q: %v", body, err)
		}
		restored, err := serializer.Deserialize(eventType, payload)
		if err != nil {
			t.Fatalf("deserialize %q: %v", payload, err)
		}
		if _, ok := restored.(fuzzPayload); !ok {
			t.Fatalf("restored type = %T, want fuzzPayload", restored)
		}

		// Arbitrary names must be rejected; arbitrary payloads must never
		// panic.
		if _, err := serializer.Deserialize(junk, payload); err == nil && junk != eventType {
			t.Fatalf("deserializing under unregistered name %q succeeded", junk)
		}
		_, _ = serializer.Deserialize(eventType, junk)
	})
}
