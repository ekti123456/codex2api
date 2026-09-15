package database

import (
	"container/list"
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

const sessionActivityCapacity = 100000
const sessionActivityBatchSize = 128
const sessionActivityPersistInterval = 30 * time.Second
const SessionActivityRecentWindow = 30 * time.Minute

// Only timestamps are persisted. In-flight counts always belong to this process.
type SessionActivity struct {
	State             string     `json:"state"`
	ActiveRequests    int        `json:"active_requests"`
	AuxiliaryRequests int        `json:"auxiliary_requests"`
	LastActiveAt      *time.Time `json:"last_active_at,omitempty"`
	LastSuccessAt     *time.Time `json:"last_success_at,omitempty"`
	LastAuxiliaryAt   *time.Time `json:"last_auxiliary_at,omitempty"`
	Recovered         bool       `json:"recovered"`
}

type SessionActivityPage struct {
	Items           map[string]SessionActivity `json:"items"`
	ObservedAt      time.Time                  `json:"observed_at"`
	LiveScope       string                     `json:"live_scope"`
	TrackingLimited bool                       `json:"tracking_limited"`
}

type sessionActivityTimes struct{ active, success, auxiliary int64 }
type sessionActivityEntry struct {
	key               string
	times             sessionActivityTimes
	active, auxiliary int
	lru, dirty        *list.Element
	dirtyAt           time.Time
	flushing          bool
}

type sessionActivityTracker struct {
	mu         sync.Mutex
	entries    map[string]*sessionActivityEntry
	lru, dirty list.List
	capacity   int
	limited    bool
	stop, done chan struct{}
	closeOnce  sync.Once
}

func newSessionActivityTracker(capacity int) *sessionActivityTracker {
	return &sessionActivityTracker{entries: make(map[string]*sessionActivityEntry), capacity: capacity, stop: make(chan struct{}), done: make(chan struct{})}
}

func (tracker *sessionActivityTracker) markDirty(entry *sessionActivityEntry, now time.Time) {
	if entry.dirty == nil {
		entry.dirtyAt = now
		entry.dirty = tracker.dirty.PushBack(entry)
	}
	tracker.lru.MoveToFront(entry.lru)
}

type SessionActivityLease struct {
	tracker   *sessionActivityTracker
	entry     *sessionActivityEntry
	auxiliary bool
	once      sync.Once
}

// No SQL, goroutine or account-dependent key on the request path. Clean idle
// entries may be evicted; active and unsaved entries are never silently dropped.
func (db *DB) BeginSessionActivity(key string, auxiliary bool, now time.Time) *SessionActivityLease {
	if db == nil || db.sessionActivity == nil || !ValidSessionOperationKey(key) {
		return nil
	}
	tracker := db.sessionActivity
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	entry := tracker.entries[key]
	if entry == nil {
		if len(tracker.entries) >= tracker.capacity {
			candidate := tracker.lru.Back()
			for checked := 0; candidate != nil && checked < 64; checked++ {
				old := candidate.Value.(*sessionActivityEntry)
				previous := candidate.Prev()
				if old.active == 0 && old.auxiliary == 0 && old.dirty == nil && !old.flushing {
					delete(tracker.entries, old.key)
					tracker.lru.Remove(candidate)
					break
				}
				candidate = previous
			}
			if len(tracker.entries) >= tracker.capacity {
				tracker.limited = true
				return nil
			}
		}
		entry = &sessionActivityEntry{key: key}
		entry.lru = tracker.lru.PushFront(entry)
		tracker.entries[key] = entry
	}
	if auxiliary {
		entry.auxiliary++
		entry.times.auxiliary = max(entry.times.auxiliary, now.UnixMilli())
	} else {
		entry.active++
		entry.times.active = max(entry.times.active, now.UnixMilli())
	}
	tracker.markDirty(entry, now)
	return &SessionActivityLease{tracker: tracker, entry: entry, auxiliary: auxiliary}
}

func (lease *SessionActivityLease) Finish(success bool, now time.Time) {
	if lease == nil {
		return
	}
	lease.once.Do(func() {
		lease.tracker.mu.Lock()
		defer lease.tracker.mu.Unlock()
		entry := lease.entry
		if lease.auxiliary {
			entry.auxiliary--
			entry.times.auxiliary = max(entry.times.auxiliary, now.UnixMilli())
		} else {
			entry.active--
			entry.times.active = max(entry.times.active, now.UnixMilli())
			if success {
				entry.times.success = max(entry.times.success, now.UnixMilli())
			}
		}
		lease.tracker.markDirty(entry, now)
	})
}

type sessionActivityWrite struct {
	entry *sessionActivityEntry
	times sessionActivityTimes
}

func (db *DB) flushSessionActivity(ctx context.Context, now time.Time, force bool) (int, error) {
	tracker := db.sessionActivity
	tracker.mu.Lock()
	batch := make([]sessionActivityWrite, 0, sessionActivityBatchSize)
	for len(batch) < sessionActivityBatchSize {
		first := tracker.dirty.Front()
		if first == nil {
			break
		}
		entry := first.Value.(*sessionActivityEntry)
		if !force && now.Sub(entry.dirtyAt) < sessionActivityPersistInterval {
			break
		}
		tracker.dirty.Remove(first)
		entry.dirty, entry.flushing = nil, true
		batch = append(batch, sessionActivityWrite{entry, entry.times})
	}
	tracker.mu.Unlock()
	if len(batch) == 0 {
		return 0, nil
	}
	values, args := make([]string, 0, len(batch)), make([]any, 0, len(batch)*4)
	for _, item := range batch {
		n := len(args)
		values = append(values, fmt.Sprintf("(CAST($%d AS TEXT),CAST($%d AS BIGINT),CAST($%d AS BIGINT),CAST($%d AS BIGINT))", n+1, n+2, n+3, n+4))
		args = append(args, item.entry.key, item.times.active, item.times.success, item.times.auxiliary)
	}
	// Persist only sessions retained by overload statistics, even though the
	// bounded memory cache can see a request before its first overload is saved.
	query := `WITH incoming(session_key,last_active_at,last_success_at,last_auxiliary_at) AS (VALUES ` + strings.Join(values, ",") + `)
		INSERT INTO session_activity(session_key,last_active_at,last_success_at,last_auxiliary_at)
		SELECT incoming.* FROM incoming JOIN session_error_stats stats ON stats.session_key=incoming.session_key WHERE 1=1
		ON CONFLICT(session_key) DO UPDATE SET
		last_active_at=CASE WHEN excluded.last_active_at>session_activity.last_active_at THEN excluded.last_active_at ELSE session_activity.last_active_at END,
		last_success_at=CASE WHEN excluded.last_success_at>session_activity.last_success_at THEN excluded.last_success_at ELSE session_activity.last_success_at END,
		last_auxiliary_at=CASE WHEN excluded.last_auxiliary_at>session_activity.last_auxiliary_at THEN excluded.last_auxiliary_at ELSE session_activity.last_auxiliary_at END`
	err := db.withSQLiteWriteLock(ctx, func() error { _, err := db.conn.ExecContext(ctx, query, args...); return err })
	tracker.mu.Lock()
	for _, item := range batch {
		item.entry.flushing = false
		if err != nil {
			tracker.markDirty(item.entry, now)
		}
	}
	tracker.mu.Unlock()
	return len(batch), err
}

func (db *DB) runSessionActivity() {
	tracker := db.sessionActivity
	defer close(tracker.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastWarning := time.Time{}
	for {
		select {
		case now := <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			var err error
			for batches := 0; batches < 32 && ctx.Err() == nil; batches++ {
				var count int
				count, err = db.flushSessionActivity(ctx, now, false)
				if err != nil || count == 0 {
					break
				}
			}
			cancel()
			if err != nil && time.Since(lastWarning) >= time.Minute {
				log.Printf("session_activity persistence failed: %v", err)
				lastWarning = now
			}
		case <-tracker.stop:
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			for ctx.Err() == nil {
				n, err := db.flushSessionActivity(ctx, time.Now(), true)
				if err != nil {
					log.Printf("session_activity shutdown flush failed: %v", err)
					return
				}
				if n == 0 {
					return
				}
			}
			return
		}
	}
}

func activityTime(ms int64) *time.Time {
	if ms <= 0 {
		return nil
	}
	stamp := time.UnixMilli(ms).UTC()
	return &stamp
}

func (db *DB) SessionActivities(ctx context.Context, keys []string, now time.Time) (SessionActivityPage, error) {
	page := SessionActivityPage{Items: make(map[string]SessionActivity, len(keys)), ObservedAt: now.UTC(), LiveScope: "instance"}
	if len(keys) == 0 || len(keys) > 100 {
		return page, fmt.Errorf("select between 1 and 100 sessions")
	}
	args, placeholders := make([]any, 0, len(keys)), make([]string, 0, len(keys))
	for _, key := range keys {
		if !ValidSessionOperationKey(key) {
			return page, fmt.Errorf("invalid session key")
		}
		if _, exists := page.Items[key]; exists {
			return page, fmt.Errorf("duplicate session key")
		}
		page.Items[key] = SessionActivity{State: "unknown"}
		args = append(args, key)
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
	}
	times, errors := make(map[string]sessionActivityTimes, len(keys)), make(map[string]int64, len(keys))
	// Exactly one primary-key batch read for this page; never scan usage logs,
	// count groups, hydrate all sessions or issue a query for each row.
	rows, err := db.conn.QueryContext(ctx, `SELECT stats.session_key,stats.last_at,COALESCE(activity.last_active_at,0),COALESCE(activity.last_success_at,0),COALESCE(activity.last_auxiliary_at,0)
		FROM session_error_stats stats LEFT JOIN session_activity activity ON activity.session_key=stats.session_key
		WHERE stats.session_key IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var key string
		var lastError int64
		var value sessionActivityTimes
		if err := rows.Scan(&key, &lastError, &value.active, &value.success, &value.auxiliary); err != nil {
			rows.Close()
			return page, err
		}
		times[key], errors[key] = value, lastError
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, err
	}
	tracker := db.sessionActivity
	if tracker != nil {
		tracker.mu.Lock()
		defer tracker.mu.Unlock()
		page.TrackingLimited = tracker.limited
	}
	for key, value := range times {
		item := page.Items[key]
		if tracker != nil {
			if entry := tracker.entries[key]; entry != nil {
				item.ActiveRequests, item.AuxiliaryRequests = entry.active, entry.auxiliary
				value.active, value.success, value.auxiliary = max(value.active, entry.times.active), max(value.success, entry.times.success), max(value.auxiliary, entry.times.auxiliary)
			}
		}
		item.LastActiveAt, item.LastSuccessAt, item.LastAuxiliaryAt = activityTime(value.active), activityTime(value.success), activityTime(value.auxiliary)
		item.Recovered = value.success > errors[key]
		switch {
		case item.ActiveRequests > 0:
			item.State = "running"
		case item.AuxiliaryRequests > 0:
			item.State = "auxiliary"
		case page.TrackingLimited:
			item.State = "unknown"
		case value.active <= 0:
			item.State = "unknown"
		case now.UnixMilli()-value.active <= SessionActivityRecentWindow.Milliseconds():
			item.State = "recent"
		default:
			item.State = "idle"
		}
		page.Items[key] = item
	}
	return page, nil
}
