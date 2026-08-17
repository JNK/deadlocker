package engine

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Lock is one row of performance_schema.data_locks, resolved back to the actor
// that holds or requests it.
//
// LockMode is the field this whole tool exists to make visible: "X,GAP" is a
// gap lock, "X,REC_NOT_GAP" a plain record lock, bare "X" a next-key lock
// (record + gap), and "X,INSERT_INTENTION" the intention lock an INSERT takes
// before it can write into a gap.
type Lock struct {
	Actor      string `json:"actor"`
	ThreadID   uint64 `json:"thread_id"`
	TrxID      string `json:"trx_id"`
	Schema     string `json:"schema"`
	Table      string `json:"table"`
	Index      string `json:"index"`
	LockType   string `json:"lock_type"`   // TABLE | RECORD
	LockMode   string `json:"lock_mode"`   // X | X,GAP | X,REC_NOT_GAP | …
	LockStatus string `json:"lock_status"` // GRANTED | WAITING
	LockData   string `json:"lock_data"`   // the key value, or "supremum pseudo-record"
	LockID     string `json:"lock_id"`

	// Explain is a plain-language reading of LockMode, generated for the UI.
	Explain string `json:"explain"`
}

// LockWait is one requesting/blocking pair from data_lock_waits.
type LockWait struct {
	WaitingActor   string `json:"waiting_actor"`
	BlockingActor  string `json:"blocking_actor"`
	WaitingThread  uint64 `json:"waiting_thread"`
	BlockingThread uint64 `json:"blocking_thread"`
	WaitingLockID  string `json:"waiting_lock_id"`
	BlockingLockID string `json:"blocking_lock_id"`
	// Resolved lock details for the pair, when available.
	WaitingLock  *Lock `json:"waiting_lock,omitempty"`
	BlockingLock *Lock `json:"blocking_lock,omitempty"`
}

// Transaction is a row of information_schema.innodb_trx.
type Transaction struct {
	Actor        string `json:"actor"`
	TrxID        string `json:"trx_id"`
	State        string `json:"state"`
	Started      string `json:"started"`
	Query        string `json:"query"`
	IsolationLvl string `json:"isolation_level"`
	RowsLocked   int64  `json:"rows_locked"`
	RowsModified int64  `json:"rows_modified"`
	ThreadID     uint64 `json:"thread_id"`
	WaitStarted  string `json:"wait_started,omitempty"`
}

// LockSnapshot is everything we know about lock state at one instant.
type LockSnapshot struct {
	At           time.Time     `json:"at"`
	Locks        []Lock        `json:"locks"`
	Waits        []LockWait    `json:"waits"`
	Transactions []Transaction `json:"transactions"`
	Err          string        `json:"err,omitempty"`
}

// explainLockMode renders a lock mode as a sentence. The gap-lock cases carry
// the explanation people actually need when a SELECT ... FOR UPDATE on a
// missing row blocks an unrelated insert.
func explainLockMode(lockType, mode, data string) string {
	if strings.EqualFold(lockType, "TABLE") {
		switch strings.ToUpper(mode) {
		case "IX":
			return "Intention exclusive: signals intent to take row-level X locks on this table."
		case "IS":
			return "Intention shared: signals intent to take row-level S locks on this table."
		case "X":
			return "Table-level exclusive lock."
		case "S":
			return "Table-level shared lock."
		}
		return "Table-level " + mode + " lock."
	}

	upper := strings.ToUpper(mode)
	parts := strings.Split(upper, ",")
	base := parts[0]
	flags := map[string]bool{}
	for _, p := range parts[1:] {
		flags[strings.TrimSpace(p)] = true
	}

	kind := "exclusive"
	if base == "S" {
		kind = "shared"
	}

	switch {
	case flags["INSERT_INTENTION"]:
		return "Insert intention lock: this transaction wants to insert into the gap before " + quoteData(data) +
			". It conflicts with any gap lock another transaction holds over that same gap — this is the classic cause of insert-vs-gap-lock waits."
	case flags["GAP"]:
		return "Gap lock (" + kind + "): locks the open interval before " + quoteData(data) +
			" without locking the row itself. It blocks inserts into that range but not updates of existing rows. A gap lock is taken even when no matching row exists."
	case flags["REC_NOT_GAP"]:
		return "Record lock (" + kind + "): locks only the index record " + quoteData(data) + ", leaving the gap before it free for inserts."
	case data == "supremum pseudo-record":
		return "Next-key lock (" + kind + ") on the supremum pseudo-record: locks the gap from the last real key to the end of the index, so any insert above the highest key waits."
	default:
		return "Next-key lock (" + kind + "): the index record " + quoteData(data) +
			" plus the gap before it. This is the default in REPEATABLE READ and locks both the row and the range preceding it."
	}
}

func quoteData(d string) string {
	d = strings.TrimSpace(d)
	if d == "" {
		return "the record"
	}
	if d == "supremum pseudo-record" {
		return "the supremum pseudo-record"
	}
	return "key " + d
}

const locksQuery = `
SELECT
    COALESCE(t.PROCESSLIST_ID, 0)        AS conn_id,
    dl.ENGINE_TRANSACTION_ID             AS trx_id,
    COALESCE(dl.OBJECT_SCHEMA, '')       AS obj_schema,
    COALESCE(dl.OBJECT_NAME, '')         AS obj_name,
    COALESCE(dl.INDEX_NAME, '')          AS index_name,
    dl.LOCK_TYPE                         AS lock_type,
    dl.LOCK_MODE                         AS lock_mode,
    dl.LOCK_STATUS                       AS lock_status,
    COALESCE(dl.LOCK_DATA, '')           AS lock_data,
    dl.ENGINE_LOCK_ID                    AS lock_id
FROM performance_schema.data_locks dl
LEFT JOIN performance_schema.threads t ON t.THREAD_ID = dl.THREAD_ID
WHERE dl.OBJECT_SCHEMA = ?
ORDER BY conn_id, dl.OBJECT_NAME, dl.INDEX_NAME, dl.LOCK_STATUS DESC, dl.LOCK_DATA`

const waitsQuery = `
SELECT
    COALESCE(rt.PROCESSLIST_ID, 0) AS waiting_conn,
    COALESCE(bt.PROCESSLIST_ID, 0) AS blocking_conn,
    w.REQUESTING_ENGINE_LOCK_ID    AS waiting_lock_id,
    w.BLOCKING_ENGINE_LOCK_ID      AS blocking_lock_id
FROM performance_schema.data_lock_waits w
LEFT JOIN performance_schema.threads rt ON rt.THREAD_ID = w.REQUESTING_THREAD_ID
LEFT JOIN performance_schema.threads bt ON bt.THREAD_ID = w.BLOCKING_THREAD_ID`

const trxQuery = `
SELECT
    trx_id,
    trx_state,
    COALESCE(DATE_FORMAT(trx_started, '%H:%i:%s'), '')      AS started,
    COALESCE(trx_query, '')                                 AS query,
    COALESCE(trx_isolation_level, '')                       AS isolation_level,
    COALESCE(trx_rows_locked, 0)                            AS rows_locked,
    COALESCE(trx_rows_modified, 0)                          AS rows_modified,
    COALESCE(trx_mysql_thread_id, 0)                        AS conn_id,
    COALESCE(DATE_FORMAT(trx_wait_started, '%H:%i:%s'), '') AS wait_started
FROM information_schema.innodb_trx`

// Snapshot reads the current lock state for one database, mapping MySQL
// connection ids back to actor names.
func Snapshot(ctx context.Context, db *sql.DB, schema string, actorByConnID map[uint64]string) LockSnapshot {
	snap := LockSnapshot{At: time.Now()}
	byLockID := map[string]*Lock{}

	rows, err := db.QueryContext(ctx, locksQuery, schema)
	if err != nil {
		snap.Err = "data_locks: " + err.Error()
		return snap
	}
	for rows.Next() {
		var l Lock
		var connID uint64
		if err := rows.Scan(&connID, &l.TrxID, &l.Schema, &l.Table, &l.Index,
			&l.LockType, &l.LockMode, &l.LockStatus, &l.LockData, &l.LockID); err != nil {
			snap.Err = "scan data_locks: " + err.Error()
			break
		}
		l.ThreadID = connID
		l.Actor = actorByConnID[connID]
		l.Explain = explainLockMode(l.LockType, l.LockMode, l.LockData)
		snap.Locks = append(snap.Locks, l)
	}
	rows.Close()
	for i := range snap.Locks {
		byLockID[snap.Locks[i].LockID] = &snap.Locks[i]
	}

	wrows, err := db.QueryContext(ctx, waitsQuery)
	if err != nil {
		if snap.Err == "" {
			snap.Err = "data_lock_waits: " + err.Error()
		}
	} else {
		for wrows.Next() {
			var w LockWait
			if err := wrows.Scan(&w.WaitingThread, &w.BlockingThread, &w.WaitingLockID, &w.BlockingLockID); err != nil {
				break
			}
			w.WaitingActor = actorByConnID[w.WaitingThread]
			w.BlockingActor = actorByConnID[w.BlockingThread]
			// Only surface waits involving this run's actors; the shared server
			// may host other runs concurrently.
			if w.WaitingActor == "" && w.BlockingActor == "" {
				continue
			}
			if l, ok := byLockID[w.WaitingLockID]; ok {
				c := *l
				w.WaitingLock = &c
			}
			if l, ok := byLockID[w.BlockingLockID]; ok {
				c := *l
				w.BlockingLock = &c
			}
			snap.Waits = append(snap.Waits, w)
		}
		wrows.Close()
	}

	trows, err := db.QueryContext(ctx, trxQuery)
	if err != nil {
		if snap.Err == "" {
			snap.Err = "innodb_trx: " + err.Error()
		}
		return snap
	}
	defer trows.Close()
	for trows.Next() {
		var t Transaction
		if err := trows.Scan(&t.TrxID, &t.State, &t.Started, &t.Query, &t.IsolationLvl,
			&t.RowsLocked, &t.RowsModified, &t.ThreadID, &t.WaitStarted); err != nil {
			break
		}
		t.Actor = actorByConnID[t.ThreadID]
		if t.Actor == "" {
			continue
		}
		snap.Transactions = append(snap.Transactions, t)
	}
	return snap
}

// BlockedBy returns the actors blocking the given actor, according to the
// snapshot.
func (s LockSnapshot) BlockedBy(actor string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range s.Waits {
		if w.WaitingActor != actor || w.BlockingActor == "" || w.BlockingActor == actor {
			continue
		}
		if !seen[w.BlockingActor] {
			seen[w.BlockingActor] = true
			out = append(out, w.BlockingActor)
		}
	}
	return out
}

// WaitFor returns the wait edge describing why actor is stuck.
func (s LockSnapshot) WaitFor(actor string) *LockWait {
	for i := range s.Waits {
		if s.Waits[i].WaitingActor == actor {
			return &s.Waits[i]
		}
	}
	return nil
}

// LatestDeadlock extracts the LATEST DETECTED DEADLOCK section from
// SHOW ENGINE INNODB STATUS. InnoDB keeps only the most recent one, which is
// exactly what we want right after provoking it.
func LatestDeadlock(ctx context.Context, db *sql.DB) (string, error) {
	rows, err := db.QueryContext(ctx, "SHOW ENGINE INNODB STATUS")
	if err != nil {
		return "", err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	if !rows.Next() {
		return "", fmt.Errorf("SHOW ENGINE INNODB STATUS returned no rows")
	}
	vals := make([]any, len(cols))
	holders := make([]sql.RawBytes, len(cols))
	for i := range vals {
		vals[i] = &holders[i]
	}
	if err := rows.Scan(vals...); err != nil {
		return "", err
	}
	var status string
	for i, c := range cols {
		if strings.EqualFold(c, "Status") {
			status = string(holders[i])
		}
	}
	if status == "" && len(holders) > 0 {
		status = string(holders[len(holders)-1])
	}

	start := strings.Index(status, "LATEST DETECTED DEADLOCK")
	if start < 0 {
		return "", nil
	}
	rest := status[start:]
	// Sections are separated by a line of dashes followed by a heading.
	if end := strings.Index(rest, "\n------------\n"); end > 0 {
		rest = rest[:end]
	}
	return strings.TrimSpace(rest), nil
}
