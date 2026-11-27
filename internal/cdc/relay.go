// Package cdc replaces outbox polling with change data capture and a
// partitioned log:
//
//	PostgreSQL WAL --(logical replication)--> Relay --> Kafka topic "post-events"
//	                                                     ├─ group search-indexer
//	                                                     ├─ group timeline-fanout
//	                                                     └─ group post-stats
//
// Why: with one polling worker doing every job, jobs block each other (a
// celebrity fan-out delays search indexing; a Redis outage pauses indexing).
// A log with independent consumer groups gives each job its own progress,
// failures and scaling, and lets new consumers replay history. This is the
// transactional outbox + log tailing pattern (what Debezium's outbox router
// does), built directly on pgoutput here.
package cdc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"
)

const (
	Publication = "networkai_outbox"
	Topic       = "post-events"
)

// Event is the wire format on the topic. ID is the outbox row id: unique and
// increasing, so consumers can use it for deduplication.
type Event struct {
	ID            int64     `json:"id"`
	AggregateType string    `json:"aggregate_type"`
	AggregateID   string    `json:"aggregate_id"`
	EventType     string    `json:"event_type"`
	CreatedAt     time.Time `json:"created_at"`
}

type Relay struct {
	// ReplicationURL is a PostgreSQL URL; replication=database is added.
	ReplicationURL string
	Slot           string
	Topic          string        // default "post-events"
	Pool           *pgxpool.Pool // ordinary connection, for deleting relayed rows
	Producer       *kgo.Client
	Log            *slog.Logger
	// OnPublished is optional observability (events per commit, WAL->broker delay).
	OnPublished func(n int, lag time.Duration)
}

// Run streams until ctx is cancelled. Delivery is at-least-once: the WAL
// position is confirmed to PostgreSQL only after the broker acknowledged
// every event up to it, so a crash replays from the last confirmed commit.
//
// Cancellation is a clean shutdown wherever it lands, including mid-publish:
// an interrupted batch was not confirmed, so the next run replays it.
func (r *Relay) Run(ctx context.Context) error {
	err := r.run(ctx)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (r *Relay) run(ctx context.Context) error {
	conn, err := pgconn.Connect(ctx, withReplication(r.ReplicationURL))
	if err != nil {
		return fmt.Errorf("replication connect: %w", err)
	}
	defer conn.Close(context.WithoutCancel(ctx))

	if err := createSlot(ctx, conn, r.Slot); err != nil {
		return err
	}
	// Start position 0 = resume from the slot's confirmed position.
	if err := pglogrepl.StartReplication(ctx, conn, r.Slot, 0, pglogrepl.StartReplicationOptions{
		PluginArgs: []string{"proto_version '1'", "publication_names '" + Publication + "'"},
	}); err != nil {
		return fmt.Errorf("start replication: %w", err)
	}
	topic := r.Topic
	if topic == "" {
		topic = Topic
	}
	r.Log.Info("relay streaming", "slot", r.Slot, "publication", Publication)

	relations := map[uint32]*pglogrepl.RelationMessage{}
	var pending []*kgo.Record
	var pendingMaxID int64
	var confirmed, received pglogrepl.LSN
	nextStatus := time.Now()

	for {
		if time.Now().After(nextStatus) {
			// Nothing in flight means every WAL record we have seen is either
			// relayed or irrelevant (other tables), so confirm all of it.
			// Confirming only outbox commits would make PostgreSQL retain WAL
			// forever while other tables are written: the classic "idle slot
			// fills the disk" CDC failure.
			if len(pending) == 0 && received > confirmed {
				confirmed = received
			}
			if err := pglogrepl.SendStandbyStatusUpdate(ctx, conn, pglogrepl.StandbyStatusUpdate{
				WALWritePosition: confirmed, WALFlushPosition: confirmed, WALApplyPosition: confirmed,
			}); err != nil {
				return fmt.Errorf("status update: %w", err)
			}
			nextStatus = time.Now().Add(5 * time.Second)
		}

		rctx, cancel := context.WithDeadline(ctx, nextStatus)
		raw, err := conn.ReceiveMessage(rctx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if pgconn.Timeout(err) {
				continue
			}
			return fmt.Errorf("receive: %w", err)
		}
		if e, ok := raw.(*pgproto3.ErrorResponse); ok {
			return fmt.Errorf("replication error: %s", e.Message)
		}
		cd, ok := raw.(*pgproto3.CopyData)
		if !ok {
			continue
		}
		switch cd.Data[0] {
		case pglogrepl.PrimaryKeepaliveMessageByteID:
			k, err := pglogrepl.ParsePrimaryKeepaliveMessage(cd.Data[1:])
			if err != nil {
				return err
			}
			if k.ServerWALEnd > received {
				received = k.ServerWALEnd
			}
			if k.ReplyRequested {
				nextStatus = time.Time{}
			}
		case pglogrepl.XLogDataByteID:
			x, err := pglogrepl.ParseXLogData(cd.Data[1:])
			if err != nil {
				return err
			}
			msg, err := pglogrepl.Parse(x.WALData)
			if err != nil {
				return fmt.Errorf("decode wal: %w", err)
			}
			switch m := msg.(type) {
			case *pglogrepl.RelationMessage:
				relations[m.RelationID] = m
			case *pglogrepl.InsertMessage:
				ev, err := decodeInsert(relations[m.RelationID], m.Tuple)
				if err != nil {
					return err
				}
				val, _ := json.Marshal(ev)
				pending = append(pending, &kgo.Record{
					Topic: topic,
					// Same key -> same partition -> per-post ordering.
					Key:   []byte(ev.AggregateID),
					Value: val,
				})
				pendingMaxID = max(pendingMaxID, ev.ID)
			case *pglogrepl.CommitMessage:
				if len(pending) > 0 {
					if err := r.Producer.ProduceSync(ctx, pending...).FirstErr(); err != nil {
						return fmt.Errorf("produce: %w", err) // restart replays from `confirmed`
					}
					if r.OnPublished != nil {
						r.OnPublished(len(pending), time.Since(m.CommitTime))
					}
					// The broker has them: drop the rows so the table stays small.
					// (Deletes are not published.) Failure here is harmless.
					if _, err := r.Pool.Exec(ctx, `DELETE FROM outbox WHERE id <= $1`, pendingMaxID); err != nil {
						r.Log.Warn("outbox cleanup failed", "err", err)
					}
					pending = pending[:0]
				}
				confirmed = m.TransactionEndLSN
			}
			if end := x.WALStart + pglogrepl.LSN(len(x.WALData)); end > received {
				received = end
			}
		}
	}
}

// EnsureSlot creates the replication slot if it does not exist. Changes are
// captured only from the moment the slot exists: rows written before that
// are not in the log, which is why CDC systems need an initial snapshot
// (Debezium takes one; here `networkai reindex` backfills the index). Call
// this before relying on capture, e.g. at deploy time.
func (r *Relay) EnsureSlot(ctx context.Context) error {
	conn, err := pgconn.Connect(ctx, withReplication(r.ReplicationURL))
	if err != nil {
		return fmt.Errorf("replication connect: %w", err)
	}
	defer conn.Close(context.WithoutCancel(ctx))
	return createSlot(ctx, conn, r.Slot)
}

func createSlot(ctx context.Context, conn *pgconn.PgConn, slot string) error {
	_, err := pglogrepl.CreateReplicationSlot(ctx, conn, slot, "pgoutput", pglogrepl.CreateReplicationSlotOptions{})
	var pgErr *pgconn.PgError
	if err != nil && (!errors.As(err, &pgErr) || pgErr.Code != "42710") { // 42710: slot already exists
		return fmt.Errorf("create slot: %w", err)
	}
	return nil
}

func decodeInsert(rel *pglogrepl.RelationMessage, tuple *pglogrepl.TupleData) (Event, error) {
	if rel == nil || tuple == nil {
		return Event{}, errors.New("insert for unknown relation")
	}
	var ev Event
	for i, col := range tuple.Columns {
		if col.DataType != 't' { // 'n' null, 'u' unchanged toast
			continue
		}
		v := string(col.Data)
		switch rel.Columns[i].Name {
		case "id":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return Event{}, err
			}
			ev.ID = n
		case "aggregate_type":
			ev.AggregateType = v
		case "aggregate_id":
			ev.AggregateID = v
		case "event_type":
			ev.EventType = v
		case "created_at":
			t, err := time.Parse("2006-01-02 15:04:05.999999-07", v)
			if err == nil {
				ev.CreatedAt = t
			}
		}
	}
	if ev.ID == 0 || ev.AggregateID == "" {
		return Event{}, fmt.Errorf("incomplete outbox row: %+v", ev)
	}
	return ev, nil
}

func withReplication(url string) string {
	sep := "?"
	for _, c := range url {
		if c == '?' {
			sep = "&"
		}
	}
	return url + sep + "replication=database"
}
