package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

type sessionErrorQueue struct {
	db       *DB
	jobs     chan SessionErrorEvent
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	mu       sync.RWMutex
	closed   bool
	pending  atomic.Int64
	written  atomic.Uint64
	dropped  atomic.Uint64
	failed   atomic.Uint64
	lastWarn atomic.Int64
}

func newSessionErrorQueue(db *DB) *sessionErrorQueue {
	ctx, cancel := context.WithCancel(context.Background())
	return &sessionErrorQueue{db: db, jobs: make(chan SessionErrorEvent, 512), ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

func (db *DB) EnqueueSessionError(event SessionErrorEvent) bool {
	if db == nil || db.sessionErrors == nil || !ValidSessionOperationKey(event.Identity.Key) || event.Identity.UserID == "" || event.CreatedAt.IsZero() {
		return false
	}
	event.Identity.UserLabel = serviceErrorString(event.Identity.UserLabel, 160)
	event.Identity.UserID = serviceErrorString(event.Identity.UserID, 255)
	event.Identity.Platform = serviceErrorString(event.Identity.Platform, 100)
	event.Identity.SessionID = serviceErrorString(event.Identity.SessionID, 256)
	event.Message = serviceErrorString(event.Message, 2048)
	event.Code, event.Model = serviceErrorString(event.Code, 128), serviceErrorString(event.Model, 128)
	event.RequestID, event.NewAPIRequestID = serviceErrorString(event.RequestID, 160), serviceErrorString(event.NewAPIRequestID, 160)
	event.Endpoint, event.Transport = serviceErrorString(event.Endpoint, 256), serviceErrorString(event.Transport, 24)
	queue := db.sessionErrors
	queue.mu.RLock()
	defer queue.mu.RUnlock()
	if queue.closed {
		queue.dropped.Add(1)
		return false
	}
	queue.pending.Add(1)
	select {
	case queue.jobs <- event:
		return true
	default:
		queue.pending.Add(-1)
		queue.dropped.Add(1)
		queue.warn("queue_full")
		return false
	}
}

func (queue *sessionErrorQueue) warn(reason string) {
	now, previous := time.Now().Unix(), queue.lastWarn.Load()
	if now-previous >= 60 && queue.lastWarn.CompareAndSwap(previous, now) {
		log.Printf("session_error_collector reason=%s dropped=%d write_failures=%d", reason, queue.dropped.Load(), queue.failed.Load())
	}
}

func (queue *sessionErrorQueue) run() {
	defer close(queue.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	batch := make([]SessionErrorEvent, 0, 64)
	lastCleanup := time.Time{}
	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(queue.ctx, 2*time.Second)
		err := queue.db.insertSessionErrors(ctx, batch)
		cancel()
		if err != nil {
			queue.failed.Add(uint64(len(batch)))
			queue.warn("write_failed")
		} else {
			queue.written.Add(uint64(len(batch)))
		}
		queue.pending.Add(-int64(len(batch)))
		clear(batch)
		batch = batch[:0]
	}
	for {
		select {
		case event, open := <-queue.jobs:
			if !open {
				flush()
				return
			}
			batch = append(batch, event)
			if len(batch) == 64 {
				flush()
			}
		case <-ticker.C:
			flush()
			if time.Since(lastCleanup) >= time.Minute {
				ctx, cancel := context.WithTimeout(queue.ctx, 2*time.Second)
				err := queue.db.pruneSessionErrors(ctx, time.Now())
				cancel()
				if err != nil {
					queue.warn("retention_failed")
				}
				lastCleanup = time.Now()
			}
		case <-queue.ctx.Done():
			lost := len(batch)
			for range queue.jobs {
				lost++
			}
			queue.pending.Add(-int64(lost))
			queue.dropped.Add(uint64(lost))
			return
		}
	}
}

func (queue *sessionErrorQueue) close() {
	queue.mu.Lock()
	if !queue.closed {
		queue.closed = true
		close(queue.jobs)
	}
	queue.mu.Unlock()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-queue.done:
	case <-timer.C:
		queue.cancel()
		<-queue.done
	}
	queue.cancel()
}

func (db *DB) insertSessionErrors(ctx context.Context, events []SessionErrorEvent) error {
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		statement, err := tx.PrepareContext(ctx, `INSERT INTO session_error_stats(session_key,user_id,session_id,first_at,last_at,error_count,identity_data,latest_data) VALUES($1,$2,$3,$4,$4,1,$5,$6) ON CONFLICT(session_key) DO UPDATE SET error_count=session_error_stats.error_count+1, first_at=CASE WHEN excluded.first_at<session_error_stats.first_at THEN excluded.first_at ELSE session_error_stats.first_at END, last_at=CASE WHEN excluded.last_at>session_error_stats.last_at THEN excluded.last_at ELSE session_error_stats.last_at END, session_id=CASE WHEN excluded.session_id<>'' THEN excluded.session_id ELSE session_error_stats.session_id END, identity_data=CASE WHEN excluded.session_id<>'' THEN excluded.identity_data ELSE session_error_stats.identity_data END, latest_data=CASE WHEN excluded.last_at>=session_error_stats.last_at THEN excluded.latest_data ELSE session_error_stats.latest_data END`)
		if err != nil {
			return err
		}
		defer statement.Close()
		for _, event := range events {
			identity, err := json.Marshal(event.Identity)
			if err != nil {
				return err
			}
			payload, err := json.Marshal(event)
			if err != nil {
				return err
			}
			if _, err = statement.ExecContext(ctx, event.Identity.Key, event.Identity.UserID, event.Identity.SessionID, event.CreatedAt.UnixMilli(), string(identity), string(payload)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (db *DB) pruneSessionErrors(ctx context.Context, now time.Time) error {
	return db.withSQLiteWriteLock(ctx, func() error {
		if _, err := db.conn.ExecContext(ctx, `DELETE FROM session_error_stats WHERE last_at<$1`, now.Add(-7*24*time.Hour).UnixMilli()); err != nil {
			return err
		}
		_, err := db.conn.ExecContext(ctx, `DELETE FROM session_error_stats WHERE session_key IN (SELECT session_key FROM session_error_stats ORDER BY last_at DESC,session_key DESC LIMIT 2147483647 OFFSET 100000)`)
		return err
	})
}

func (db *DB) SessionErrorCollectorStats() ServiceErrorCollectorStats {
	stats := ServiceErrorCollectorStats{Capacity: 512, RetentionDays: 7, MaxRows: 100000}
	if db != nil && db.sessionErrors != nil {
		queue := db.sessionErrors
		stats.Pending, stats.Written, stats.Dropped, stats.WriteFailures = queue.pending.Load(), queue.written.Load(), queue.dropped.Load(), queue.failed.Load()
	}
	return stats
}
