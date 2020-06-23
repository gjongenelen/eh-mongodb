package mongodb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"

	eh "github.com/looplab/eventhorizon"
)

// ErrCouldNotDialDB is when the database could not be dialed.
var ErrCouldNotDialDB = errors.New("could not dial database")

// ErrNoDBClient is when no database client is set.
var ErrNoDBClient = errors.New("no database client")

// ErrCouldNotClearDB is when the database could not be cleared.
var ErrCouldNotClearDB = errors.New("could not clear database")

// ErrCouldNotMarshalEvent is when an event could not be marshaled into BSON.
var ErrCouldNotMarshalEvent = errors.New("could not marshal event")

// ErrCouldNotUnmarshalEvent is when an event could not be unmarshaled into a concrete type.
var ErrCouldNotUnmarshalEvent = errors.New("could not unmarshal event")

// ErrCouldNotLoadAggregate is when an aggregate could not be loaded.
var ErrCouldNotLoadAggregate = errors.New("could not load aggregate")

// ErrCouldNotSaveAggregate is when an aggregate could not be saved.
var ErrCouldNotSaveAggregate = errors.New("could not save aggregate")

// EventStore implements an EventStore for MongoDB.
type EventStore struct {
	client   *mongo.Client
	dbPrefix string
}

// NewEventStore creates a new EventStore with a MongoDB URI: `mongodb://hostname`.
func NewEventStore(uri, dbPrefix string) (*EventStore, error) {
	opts := options.Client().ApplyURI(uri)
	opts.SetWriteConcern(writeconcern.New(writeconcern.WMajority()))
	opts.SetReadConcern(readconcern.Majority())
	opts.SetReadPreference(readpref.Primary())
	client, err := mongo.Connect(context.TODO(), opts)
	if err != nil {
		return nil, ErrCouldNotDialDB
	}

	return NewEventStoreWithClient(client, dbPrefix)
}

// NewEventStoreWithClient creates a new EventStore with a client.
func NewEventStoreWithClient(client *mongo.Client, dbPrefix string) (*EventStore, error) {
	if client == nil {
		return nil, ErrNoDBClient
	}

	s := &EventStore{
		client:   client,
		dbPrefix: dbPrefix,
	}

	return s, nil
}

// Save implements the Save method of the eventhorizon.EventStore interface.
func (s *EventStore) Save(ctx context.Context, events []eh.Event, originalVersion int) error {
	if len(events) == 0 {
		return eh.EventStoreError{
			Err:       eh.ErrNoEventsToAppend,
			Namespace: eh.NamespaceFromContext(ctx),
		}
	}

	// Build all event records, with incrementing versions starting from the
	// original aggregate version.
	dbEvents := make([]interface{}, len(events))
	aggregateID := events[0].AggregateID()
	version := originalVersion
	for i, event := range events {
		// Only accept events belonging to the same aggregate.
		if event.AggregateID() != aggregateID {
			return eh.EventStoreError{
				Err:       eh.ErrInvalidEvent,
				Namespace: eh.NamespaceFromContext(ctx),
			}
		}

		// Only accept events that apply to the correct aggregate version.
		if event.Version() != version+1 {
			return eh.EventStoreError{
				Err:       eh.ErrIncorrectEventVersion,
				Namespace: eh.NamespaceFromContext(ctx),
			}
		}

		// Create the event record for the DB.
		e, err := newDBEvent(ctx, event)
		if err != nil {
			return err
		}
		dbEvents[i] = *e
		version++
	}

	eventsCollection := s.client.Database(s.dbName(ctx)).Collection("events")

	_, err := eventsCollection.InsertMany(ctx, dbEvents)
	if err != nil {
		return eh.EventStoreError{
			Err:       ErrCouldNotSaveAggregate,
			BaseErr:   err,
			Namespace: eh.NamespaceFromContext(ctx),
		}
	}

	return nil
}

// Load implements the Load method of the eventhorizon.EventStore interface.
func (s *EventStore) Load(ctx context.Context, id uuid.UUID) ([]eh.Event, error) {
	c := s.client.Database(s.dbName(ctx)).Collection("events")

	cursor, err := c.Find(ctx, bson.M{"aggregate_id": id})
	if err == mongo.ErrNoDocuments {
		return []eh.Event{}, nil
	} else if err != nil {
		return nil, eh.EventStoreError{
			Err:       err,
			Namespace: eh.NamespaceFromContext(ctx),
		}
	}

	events := make([]eh.Event, 0)
	for cursor.Next(ctx) {
		rawEvent := &dbEvent{}
		if err := cursor.Decode(rawEvent); err != nil {
			return nil, eh.RepoError{
				Err:       err,
				Namespace: eh.NamespaceFromContext(ctx),
			}
		}
		if len(rawEvent.RawData) > 0 {
			var err error
			if rawEvent.data, err = eh.CreateEventData(rawEvent.EventType); err != nil {
				return nil, eh.EventStoreError{
					Err:       ErrCouldNotUnmarshalEvent,
					BaseErr:   err,
					Namespace: eh.NamespaceFromContext(ctx),
				}
			}
			if err := bson.Unmarshal(rawEvent.RawData, rawEvent.data); err != nil {
				return nil, eh.EventStoreError{
					Err:       ErrCouldNotUnmarshalEvent,
					BaseErr:   err,
					Namespace: eh.NamespaceFromContext(ctx),
				}
			}
			rawEvent.RawData = nil
		}

		events = append(events, event{dbEvent: *rawEvent})

	}
	return events, nil
}

// Replace implements the Replace method of the eventhorizon.EventStore interface.
func (s *EventStore) Replace(ctx context.Context, event eh.Event) error {
	c := s.client.Database(s.dbName(ctx)).Collection("events")

	// First check if the aggregate exists, the not found error in the update
	// query can mean both that the aggregate or the event is not found.
	if n, err := c.CountDocuments(ctx, bson.M{"aggregate_id": event.AggregateID()}); n == 0 {
		return eh.ErrAggregateNotFound
	} else if err != nil {
		return eh.EventStoreError{
			Err:       err,
			Namespace: eh.NamespaceFromContext(ctx),
		}
	}

	// Create the event record for the Database.
	e, err := newDBEvent(ctx, event)
	if err != nil {
		return err
	}

	// Find and replace the event.
	if r, err := c.UpdateOne(ctx,
		bson.M{
			"aggregate_id": event.AggregateID(),
			"version":      event.Version(),
		},
		bson.M{
			"$set": *e,
		},
	); err != nil {
		return eh.EventStoreError{
			Err:       ErrCouldNotSaveAggregate,
			BaseErr:   err,
			Namespace: eh.NamespaceFromContext(ctx),
		}
	} else if r.MatchedCount == 0 {
		return eh.ErrInvalidEvent
	}

	return nil
}

// RenameEvent implements the RenameEvent method of the eventhorizon.EventStore interface.
func (s *EventStore) RenameEvent(ctx context.Context, from, to eh.EventType) error {
	c := s.client.Database(s.dbName(ctx)).Collection("events")

	// Find and rename all events.
	// TODO: Maybe use change info.
	if _, err := c.UpdateMany(ctx,
		bson.M{
			"event_type": string(from),
		},
		bson.M{
			"$set": bson.M{"event_type": string(to)},
		},
	); err != nil {
		return eh.EventStoreError{
			Err:       ErrCouldNotSaveAggregate,
			BaseErr:   err,
			Namespace: eh.NamespaceFromContext(ctx),
		}
	}

	return nil
}

// Clear clears the event storage.
func (s *EventStore) Clear(ctx context.Context) error {
	c := s.client.Database(s.dbName(ctx)).Collection("events")

	if err := c.Drop(ctx); err != nil {
		return eh.EventStoreError{
			Err:       ErrCouldNotClearDB,
			BaseErr:   err,
			Namespace: eh.NamespaceFromContext(ctx),
		}
	}
	return nil
}

// Close closes the database client.
func (s *EventStore) Close(ctx context.Context) {
	s.client.Disconnect(ctx)
}

// dbName appends the namespace, if one is set, to the Database prefix to
// get the name of the Database to use.
func (s *EventStore) dbName(ctx context.Context) string {
	ns := eh.NamespaceFromContext(ctx)
	return s.dbPrefix + "_" + ns
}

// aggregateRecord is the Database representation of an aggregate.
type aggregateRecord struct {
	AggregateID uuid.UUID `bson:"_id"`
	Version     int       `bson:"version"`
	// Type        string        `bson:"type"`
	// Snapshot    bson.Raw      `bson:"snapshot"`
}

// dbEvent is the internal event record for the MongoDB event store used
// to save and load events from the DB.
type dbEvent struct {
	EventType     eh.EventType     `bson:"event_type"`
	RawData       bson.Raw         `bson:"data,omitempty"`
	data          eh.EventData     `bson:"-"`
	Timestamp     time.Time        `bson:"timestamp"`
	AggregateType eh.AggregateType `bson:"aggregate_type"`
	AggregateID   uuid.UUID        `bson:"aggregate_id"`
	Version       int              `bson:"version"`
}

// newDBEvent returns a new dbEvent for an event.
func newDBEvent(ctx context.Context, event eh.Event) (*dbEvent, error) {
	e := &dbEvent{
		EventType:     event.EventType(),
		Timestamp:     event.Timestamp(),
		AggregateType: event.AggregateType(),
		AggregateID:   event.AggregateID(),
		Version:       event.Version(),
	}

	// Marshal event data if there is any.
	if event.Data() != nil {
		var err error
		e.RawData, err = bson.Marshal(event.Data())
		if err != nil {
			return nil, eh.EventStoreError{
				Err:       ErrCouldNotMarshalEvent,
				BaseErr:   err,
				Namespace: eh.NamespaceFromContext(ctx),
			}
		}
	}

	return e, nil
}

// event is the private implementation of the eventhorizon.Event interface
// for a MongoDB event store.
type event struct {
	dbEvent
}

// AggrgateID implements the AggrgateID method of the eventhorizon.Event interface.
func (e event) AggregateID() uuid.UUID {
	return e.dbEvent.AggregateID
}

// AggregateType implements the AggregateType method of the eventhorizon.Event interface.
func (e event) AggregateType() eh.AggregateType {
	return e.dbEvent.AggregateType
}

// EventType implements the EventType method of the eventhorizon.Event interface.
func (e event) EventType() eh.EventType {
	return e.dbEvent.EventType
}

// Data implements the Data method of the eventhorizon.Event interface.
func (e event) Data() eh.EventData {
	return e.dbEvent.data
}

// Version implements the Version method of the eventhorizon.Event interface.
func (e event) Version() int {
	return e.dbEvent.Version
}

// Timestamp implements the Timestamp method of the eventhorizon.Event interface.
func (e event) Timestamp() time.Time {
	return e.dbEvent.Timestamp
}

// String implements the String method of the eventhorizon.Event interface.
func (e event) String() string {
	return fmt.Sprintf("%s@%d", e.dbEvent.EventType, e.dbEvent.Version)
}
