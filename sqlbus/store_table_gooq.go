package sqlbus

import (
	"github.com/cgardev/gooq"
)

// The typed gooq table definitions mirror the migration scripts. They are
// written by hand rather than generated, because the schema is owned by the
// embedded migrations, not by a live database. Nullable columns are declared
// as StringField for comparison ergonomics; the row scanning goes through
// sql.Null values in the fetch structs, so a NULL never reaches a comparison
// these queries perform.

type messageTable struct {
	gooq.TableImpl
	ID              gooq.StringField
	EventType       gooq.StringField
	SerializedEvent gooq.StringField
	PublisherNode   gooq.StringField
	PublicationDate gooq.StringField
}

func newMessageTable(alias string) *messageTable {
	base := gooq.NewTable(messageTableName).WithAlias(alias)
	return &messageTable{
		TableImpl:       base,
		ID:              gooq.NewStringField(base, "id"),
		EventType:       gooq.NewStringField(base, "event_type"),
		SerializedEvent: gooq.NewStringField(base, "serialized_event"),
		PublisherNode:   gooq.NewStringField(base, "publisher_node"),
		PublicationDate: gooq.NewStringField(base, "publication_date"),
	}
}

type listenerTable struct {
	gooq.TableImpl
	ListenerID   gooq.StringField
	DeliveryMode gooq.StringField
	Ordering     gooq.StringField
}

func newListenerTable(alias string) *listenerTable {
	base := gooq.NewTable(listenerTableName).WithAlias(alias)
	return &listenerTable{
		TableImpl:    base,
		ListenerID:   gooq.NewStringField(base, "listener_id"),
		DeliveryMode: gooq.NewStringField(base, "delivery_mode"),
		Ordering:     gooq.NewStringField(base, "ordering"),
	}
}

type consumerTable struct {
	gooq.TableImpl
	ListenerID       gooq.StringField
	Instance         gooq.StringField
	EventType        gooq.StringField
	DeliveryMode     gooq.StringField
	StartBoundary    gooq.StringField
	Frontier         gooq.StringField
	RegistrationDate gooq.StringField
	HeartbeatDate    gooq.StringField
}

func newConsumerTable(alias string) *consumerTable {
	base := gooq.NewTable(consumerTableName).WithAlias(alias)
	return &consumerTable{
		TableImpl:        base,
		ListenerID:       gooq.NewStringField(base, "listener_id"),
		Instance:         gooq.NewStringField(base, "instance"),
		EventType:        gooq.NewStringField(base, "event_type"),
		DeliveryMode:     gooq.NewStringField(base, "delivery_mode"),
		StartBoundary:    gooq.NewStringField(base, "start_boundary"),
		Frontier:         gooq.NewStringField(base, "frontier"),
		RegistrationDate: gooq.NewStringField(base, "registration_date"),
		HeartbeatDate:    gooq.NewStringField(base, "heartbeat_date"),
	}
}

type deliveryTable struct {
	gooq.TableImpl
	MessageID       gooq.StringField
	ListenerID      gooq.StringField
	Instance        gooq.StringField
	Status          gooq.StringField
	Attempts        gooq.NumericField[int64]
	ClaimToken      gooq.StringField
	ClaimDate       gooq.StringField
	NextAttemptDate gooq.StringField
	CompletionDate  gooq.StringField
	LastError       gooq.StringField
}

func newDeliveryTable(alias string) *deliveryTable {
	base := gooq.NewTable(deliveryTableName).WithAlias(alias)
	return &deliveryTable{
		TableImpl:       base,
		MessageID:       gooq.NewStringField(base, "message_id"),
		ListenerID:      gooq.NewStringField(base, "listener_id"),
		Instance:        gooq.NewStringField(base, "instance"),
		Status:          gooq.NewStringField(base, "status"),
		Attempts:        gooq.NewNumericField[int64](base, "attempts"),
		ClaimToken:      gooq.NewStringField(base, "claim_token"),
		ClaimDate:       gooq.NewStringField(base, "claim_date"),
		NextAttemptDate: gooq.NewStringField(base, "next_attempt_date"),
		CompletionDate:  gooq.NewStringField(base, "completion_date"),
		LastError:       gooq.NewStringField(base, "last_error"),
	}
}

// The unaliased instances address the tables in statements that involve one
// occurrence of each; queries that correlate a table with itself construct
// their own aliased instances.
var (
	gooqMessage  = newMessageTable("")
	gooqListener = newListenerTable("")
	gooqConsumer = newConsumerTable("")
	gooqDelivery = newDeliveryTable("")
)

// incompleteStatuses are the delivery statuses that hold a FIFO queue back: a
// dead letter deliberately does not, so an exhausted head parks and its
// successors continue.
func incompleteStatuses() []string {
	return []string{string(StatusPending), string(StatusProcessing), string(StatusFailed)}
}
