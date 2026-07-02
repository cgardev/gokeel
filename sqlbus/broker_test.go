package sqlbus

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/cgardev/gokeel/eventbus"
	"github.com/cgardev/gokeel/eventbus/bustest"
)

// brokerHarness adapts the sqlbus engine to the shared Broker conformance
// suite over SQLite: one node, a fast-polling dispatcher, and a small
// materialization grace so the FIFO watermark adds little latency.
type brokerHarness struct{}

func (brokerHarness) NewBroker(t *testing.T) eventbus.Broker {
	t.Helper()
	path := filepath.Join(t.TempDir(), "broker.db")
	node := newSQLiteNode(t, path,
		WithMaterializationGrace(50*time.Millisecond),
		WithLeaseDuration(30*time.Second),
	)
	serializer, ok := node.bridge.serializer.(*JSONSerializer)
	if !ok {
		t.Fatal("fixture serializer is not the JSON serializer")
	}
	if err := RegisterEventType[bustest.Event](serializer, bustest.EventTypeName); err != nil {
		t.Fatalf("register conformance event type: %v", err)
	}

	dispatcher := NewDispatcher(node.bridge, WithPollInterval(20*time.Millisecond))
	stop := dispatcher.Start()
	t.Cleanup(stop)

	return NewBroker(node.bridge, node.publisher)
}

// SettleWithin allows for the polling cadence and the FIFO ordering
// watermark, which delays ordered deliveries by the materialization grace.
func (brokerHarness) SettleWithin() time.Duration { return 45 * time.Second }

func TestSQLBrokerConformance(t *testing.T) {
	bustest.Run(t, brokerHarness{})
}
