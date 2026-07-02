package outbox

import (
	"github.com/cgardev/gooq"
)

// The typed gooq table definitions mirror the migration scripts. They are
// written by hand rather than generated, because the schema is owned by the
// embedded migrations, not by a live database. Nullable columns are declared
// as StringField for comparison ergonomics; the row scanning goes through
// sql.Null values, so a NULL never reaches a comparison these queries
// perform.
type publicationTable struct {
	gooq.TableImpl
	ID                   gooq.StringField
	ListenerID           gooq.StringField
	EventType            gooq.StringField
	SerializedEvent      gooq.StringField
	PublicationDate      gooq.StringField
	CompletionDate       gooq.StringField
	Status               gooq.StringField
	CompletionAttempts   gooq.NumericField[int64]
	LastResubmissionDate gooq.StringField
}

func newPublicationTable(name string) *publicationTable {
	base := gooq.NewTable(name)
	return &publicationTable{
		TableImpl:            base,
		ID:                   gooq.NewStringField(base, "id"),
		ListenerID:           gooq.NewStringField(base, "listener_id"),
		EventType:            gooq.NewStringField(base, "event_type"),
		SerializedEvent:      gooq.NewStringField(base, "serialized_event"),
		PublicationDate:      gooq.NewStringField(base, "publication_date"),
		CompletionDate:       gooq.NewStringField(base, "completion_date"),
		Status:               gooq.NewStringField(base, "status"),
		CompletionAttempts:   gooq.NewNumericField[int64](base, "completion_attempts"),
		LastResubmissionDate: gooq.NewStringField(base, "last_resubmission_date"),
	}
}

var (
	gooqPublication = newPublicationTable(tableName)
	gooqArchive     = newPublicationTable(archiveTableName)
)
