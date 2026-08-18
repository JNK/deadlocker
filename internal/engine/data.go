package engine

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Reading the table itself.
//
// The lock tables say a transaction holds X,GAP on PRIMARY before key 7. That
// is the truth, and it is unreadable until you can see that there are rows 5
// and 9 and nothing between them. A scenario's seed data is in the YAML, but by
// step four it has been updated, deleted and inserted into, and the gap the
// locks are talking about is a gap in a table nobody is showing.
//
// So: the table, in index order, with every lock drawn onto the row or the gap
// it actually covers.
//
// The read runs on the run's observer connection — a session of its own, in
// autocommit, taking no locks. That has one consequence worth stating plainly
// rather than hiding: it sees committed data only. A row an actor has inserted
// but not committed is not here, though its lock is, which is why locks that
// match no visible row are reported rather than dropped.

// IndexInfo is one index of a table, as InnoDB orders it.
type IndexInfo struct {
	Name    string   `json:"name"`
	Unique  bool     `json:"unique"`
	Type    string   `json:"type"`
	Columns []string `json:"columns"`
	// KeyColumns is Columns followed by the primary key's columns, for a
	// secondary index. That is the order InnoDB stores the index in and the
	// shape LOCK_DATA takes, so it is what a lock is matched against.
	KeyColumns []string `json:"key_columns"`
	// Orderable is false for an index this view cannot sort by — a functional
	// index, whose key is an expression rather than a column.
	Orderable bool `json:"orderable"`
}

// TableInfo is one table of the scratch database.
type TableInfo struct {
	Name   string `json:"name"`
	Engine string `json:"engine"`
	// Estimate is InnoDB's row estimate, which is what information_schema has
	// without counting. The exact count comes back with the rows.
	Estimate int64       `json:"estimate"`
	Indexes  []IndexInfo `json:"indexes"`
}

// PrimaryKey returns the primary key's columns, or nil when the table has none.
func (t TableInfo) PrimaryKey() []string {
	for _, idx := range t.Indexes {
		if idx.Name == "PRIMARY" {
			return idx.Columns
		}
	}
	return nil
}

// Index looks an index up by name, falling back to the first one.
func (t TableInfo) Index(name string) (IndexInfo, bool) {
	for _, idx := range t.Indexes {
		if strings.EqualFold(idx.Name, name) {
			return idx, true
		}
	}
	if len(t.Indexes) > 0 {
		return t.Indexes[0], name == ""
	}
	return IndexInfo{}, false
}

// RowLock is a lock drawn onto the row or gap it covers.
type RowLock struct {
	Actor  string `json:"actor"`
	Mode   string `json:"mode"`
	Status string `json:"status"`
	Index  string `json:"index"`
	Data   string `json:"data"`
	// Object is what performance_schema called the locked object, which for a
	// partitioned table names the partition: "orders#p#p2024".
	Object string `json:"object,omitempty"`
	// Kind is record, next-key, gap, insert-intention or table.
	Kind    string `json:"kind"`
	Explain string `json:"explain"`
}

// DataRow is one row of the table, with whatever locks cover it.
type DataRow struct {
	Values []string `json:"values"`
	Null   []bool   `json:"null"`
	// Key is the row's index key, component by component, in index order.
	Key []string `json:"key"`
	// Locks cover the record itself; GapLocks cover the gap before it, which is
	// where an insert would land.
	Locks    []RowLock `json:"locks,omitempty"`
	GapLocks []RowLock `json:"gap_locks,omitempty"`
}

// TableView is one table read at one instant, annotated with one lock snapshot.
type TableView struct {
	Table     string    `json:"table"`
	Index     string    `json:"index"`
	Columns   []string  `json:"columns"`
	Rows      []DataRow `json:"rows"`
	RowCount  int64     `json:"row_count"`
	Exact     bool      `json:"exact"`
	Truncated bool      `json:"truncated"`
	// Supremum holds locks on the gap above the highest key — the one every
	// ascending key lands in, and the reason a UUIDv7 insert waits.
	Supremum []RowLock `json:"supremum,omitempty"`
	// Unmatched are locks on this index that no visible row accounts for: keys
	// inserted but not committed, or rows already deleted.
	Unmatched []RowLock `json:"unmatched,omitempty"`
	// OtherIndex holds locks this table has in an index the view is not ordered
	// by. They are real and they are not drawn on anything here, because their
	// keys are not this index's keys -- switching index is what shows them.
	OtherIndex []RowLock `json:"other_index,omitempty"`
	At         time.Time `json:"at"`
	// LocksAt is when the snapshot the annotations came from was taken.
	LocksAt time.Time `json:"locks_at,omitempty"`
	Err     string    `json:"err,omitempty"`
}

// maxDataRows bounds a table view. A scenario's tables are small by design;
// the cap is there for the one that seeds a million rows to make a point about
// a full scan.
const maxDataRows = 200

const tablesQuery = `
SELECT TABLE_NAME, COALESCE(ENGINE, ''), COALESCE(TABLE_ROWS, 0)
FROM information_schema.TABLES
WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE'
ORDER BY TABLE_NAME`

const indexesQuery = `
SELECT TABLE_NAME, INDEX_NAME, NON_UNIQUE, COALESCE(INDEX_TYPE, ''),
       COALESCE(COLUMN_NAME, ''), COALESCE(EXPRESSION, '')
FROM information_schema.STATISTICS
WHERE TABLE_SCHEMA = ?
ORDER BY TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX`

// Tables lists the scratch database's tables and their indexes.
func Tables(ctx context.Context, db *sql.DB, schema string) ([]TableInfo, error) {
	rows, err := db.QueryContext(ctx, tablesQuery, schema)
	if err != nil {
		return nil, err
	}
	var (
		out    []TableInfo
		byName = map[string]int{}
	)
	for rows.Next() {
		var t TableInfo
		if err := rows.Scan(&t.Name, &t.Engine, &t.Estimate); err != nil {
			rows.Close()
			return nil, err
		}
		byName[t.Name] = len(out)
		out = append(out, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	irows, err := db.QueryContext(ctx, indexesQuery, schema)
	if err != nil {
		return out, err
	}
	defer irows.Close()
	for irows.Next() {
		var table, name, indexType, column, expression string
		var nonUnique int
		if err := irows.Scan(&table, &name, &nonUnique, &indexType, &column, &expression); err != nil {
			return out, err
		}
		at, ok := byName[table]
		if !ok {
			continue
		}
		t := &out[at]
		if n := len(t.Indexes); n > 0 && t.Indexes[n-1].Name == name {
			t.Indexes[n-1].Columns = append(t.Indexes[n-1].Columns, columnOrExpr(column, expression))
			if column == "" {
				t.Indexes[n-1].Orderable = false
			}
			continue
		}
		t.Indexes = append(t.Indexes, IndexInfo{
			Name:      name,
			Unique:    nonUnique == 0,
			Type:      indexType,
			Columns:   []string{columnOrExpr(column, expression)},
			Orderable: column != "",
		})
	}

	for i := range out {
		sortIndexes(out[i].Indexes)
		pk := out[i].PrimaryKey()
		for j := range out[i].Indexes {
			out[i].Indexes[j].KeyColumns = keyColumns(out[i].Indexes[j], pk)
		}
	}
	return out, nil
}

func columnOrExpr(column, expression string) string {
	if column != "" {
		return column
	}
	if expression != "" {
		return "(" + expression + ")"
	}
	return "?"
}

// sortIndexes puts PRIMARY first: it is the row order, and the one a reader
// wants by default.
func sortIndexes(idx []IndexInfo) {
	sort.SliceStable(idx, func(i, j int) bool {
		if (idx[i].Name == "PRIMARY") != (idx[j].Name == "PRIMARY") {
			return idx[i].Name == "PRIMARY"
		}
		return idx[i].Name < idx[j].Name
	})
}

// keyColumns is what a lock on this index reports in LOCK_DATA: the index's own
// columns, and for a secondary index the primary key appended, because that is
// what InnoDB stores in a secondary index entry.
func keyColumns(idx IndexInfo, pk []string) []string {
	out := append([]string{}, idx.Columns...)
	if idx.Name == "PRIMARY" {
		return out
	}
	have := map[string]bool{}
	for _, c := range out {
		have[c] = true
	}
	for _, c := range pk {
		if !have[c] {
			out = append(out, c)
		}
	}
	return out
}

// FetchTable reads one table in the order of one index.
//
// Errors are returned in the view rather than as a Go error: a table that
// cannot be read right now — usually because a DDL step is holding a metadata
// lock — is itself worth showing, and it should not blank the pane beside it.
func FetchTable(ctx context.Context, db *sql.DB, table TableInfo, indexName string, limit int) TableView {
	if limit <= 0 || limit > maxDataRows {
		limit = maxDataRows
	}
	idx, _ := table.Index(indexName)
	view := TableView{Table: table.Name, Index: idx.Name, At: time.Now(), RowCount: table.Estimate}

	quoted, err := quoteIdent(table.Name)
	if err != nil {
		view.Err = err.Error()
		return view
	}
	query := "SELECT * FROM " + quoted
	if order := orderBy(idx); order != "" {
		query += " ORDER BY " + order
	}
	// One row past the limit, so "there are more" needs no second query.
	query += fmt.Sprintf(" LIMIT %d", limit+1)

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		view.Err = err.Error()
		return view
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		view.Err = err.Error()
		return view
	}
	view.Columns = cols

	holders := make([]sql.RawBytes, len(cols))
	scan := make([]any, len(cols))
	for i := range holders {
		scan[i] = &holders[i]
	}
	keyAt := columnPositions(cols, idx.KeyColumns)

	for rows.Next() {
		if len(view.Rows) >= limit {
			view.Truncated = true
			break
		}
		if err := rows.Scan(scan...); err != nil {
			view.Err = err.Error()
			return view
		}
		row := DataRow{
			Values: make([]string, len(cols)),
			Null:   make([]bool, len(cols)),
		}
		for i, h := range holders {
			if h == nil {
				row.Null[i] = true
				continue
			}
			row.Values[i] = string(h)
		}
		for _, at := range keyAt {
			if at >= 0 && !row.Null[at] {
				row.Key = append(row.Key, row.Values[at])
			} else if at >= 0 {
				row.Key = append(row.Key, "NULL")
			}
		}
		view.Rows = append(view.Rows, row)
	}
	if err := rows.Err(); err != nil && view.Err == "" {
		view.Err = err.Error()
	}
	if !view.Truncated {
		view.RowCount = int64(len(view.Rows))
		view.Exact = true
	}
	return view
}

// orderBy renders the index's columns as an ORDER BY clause, or "" when the
// index cannot be sorted by (a functional index) or does not exist.
func orderBy(idx IndexInfo) string {
	if !idx.Orderable || len(idx.KeyColumns) == 0 {
		return ""
	}
	parts := make([]string, 0, len(idx.KeyColumns))
	for _, c := range idx.KeyColumns {
		q, err := quoteIdent(c)
		if err != nil {
			return ""
		}
		parts = append(parts, q)
	}
	return strings.Join(parts, ", ")
}

// columnPositions maps key column names onto their position in the result set.
func columnPositions(cols, keys []string) []int {
	out := make([]int, 0, len(keys))
	for _, k := range keys {
		at := -1
		for i, c := range cols {
			if strings.EqualFold(c, k) {
				at = i
				break
			}
		}
		out = append(out, at)
	}
	return out
}

// quoteIdent renders an identifier for interpolation. Everything here comes
// from information_schema for a database this process created, so this is a
// belt-and-braces check rather than the only thing standing between a table
// name and the parser -- but a table called `a` + backtick would still be a
// syntax error at best, so it is refused rather than sent.
func quoteIdent(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty identifier")
	}
	if strings.ContainsAny(name, "`\x00") {
		return "", fmt.Errorf("identifier %q cannot be quoted safely", name)
	}
	return "`" + name + "`", nil
}

// Annotate draws a lock snapshot onto a table view: every lock lands on the row
// it locks, the gap before that row, the gap above the last key, or — when the
// key belongs to no visible row — in the unmatched list.
func Annotate(view *TableView, table TableInfo, snap *LockSnapshot) {
	if view == nil || snap == nil {
		return
	}
	view.LocksAt = snap.At
	view.Supremum = nil
	view.Unmatched = nil
	view.OtherIndex = nil
	for i := range view.Rows {
		view.Rows[i].Locks = nil
		view.Rows[i].GapLocks = nil
	}

	// Rows are in index order, so a key can be found by walking once. There are
	// at most a few hundred of each.
	for _, l := range snap.Locks {
		if !strings.EqualFold(baseTable(l.Table), table.Name) {
			continue
		}
		rl := RowLock{
			Actor: l.Actor, Mode: l.LockMode, Status: l.LockStatus,
			Index: l.Index, Data: l.LockData, Kind: lockKind(l), Explain: l.Explain,
		}
		if !strings.EqualFold(l.Table, table.Name) {
			rl.Object = l.Table
		}
		if rl.Kind == "table" {
			// A table-level intention lock covers no particular row; it belongs to
			// the Locks tab, which shows it already.
			continue
		}
		// A lock in another index is about that index's keys, which are not the
		// ones on screen. Drawing it on a row here would be a guess; hiding it
		// would be worse.
		if l.Index != "" && view.Index != "" && !strings.EqualFold(l.Index, view.Index) {
			view.OtherIndex = append(view.OtherIndex, rl)
			continue
		}
		if strings.Contains(strings.ToLower(l.LockData), "supremum") {
			view.Supremum = append(view.Supremum, rl)
			continue
		}
		at := findRow(view.Rows, l.Index, view.Index, l.LockData)
		if at < 0 {
			view.Unmatched = append(view.Unmatched, rl)
			continue
		}
		if coversGap(rl.Kind) {
			view.Rows[at].GapLocks = append(view.Rows[at].GapLocks, rl)
		}
		if coversRecord(rl.Kind) {
			view.Rows[at].Locks = append(view.Rows[at].Locks, rl)
		}
	}
}

// baseTable strips the partition suffix InnoDB puts in OBJECT_NAME: a lock on a
// partitioned table is reported as "orders#p#p2024". Without this, the one
// scenario in the library about partition pruning would show no locks at all on
// the table they are plainly on.
func baseTable(name string) string {
	if i := strings.Index(name, "#"); i > 0 {
		return name[:i]
	}
	return name
}

// lockKind names what a lock actually covers, which is the distinction the mode
// string buries in a comma-separated list.
func lockKind(l Lock) string {
	if strings.EqualFold(l.LockType, "TABLE") {
		return "table"
	}
	upper := strings.ToUpper(l.LockMode)
	switch {
	case strings.Contains(upper, "INSERT_INTENTION"):
		return "insert-intention"
	case strings.Contains(upper, "REC_NOT_GAP"):
		return "record"
	case strings.Contains(upper, "GAP"):
		return "gap"
	default:
		return "next-key"
	}
}

// coversRecord reports whether the lock stops another transaction touching the
// row itself; coversGap, whether it stops an insert landing before it.
func coversRecord(kind string) bool { return kind == "record" || kind == "next-key" }
func coversGap(kind string) bool {
	return kind == "gap" || kind == "next-key" || kind == "insert-intention"
}

// findRow locates the row a lock's key names, or -1.
//
// A lock is on one index and the view is ordered by one index; when they differ
// the key components are not comparable, so the lock goes to the unmatched list
// rather than being drawn on a row it does not describe.
func findRow(rows []DataRow, lockIndex, viewIndex, lockData string) int {
	if lockIndex != "" && viewIndex != "" && !strings.EqualFold(lockIndex, viewIndex) {
		return -1
	}
	want := SplitLockData(lockData)
	if len(want) == 0 {
		return -1
	}
	for i := range rows {
		if keyMatches(rows[i].Key, want) {
			return i
		}
	}
	return -1
}

// keyMatches compares a row's key with a lock's, component by component.
//
// Either can be the shorter: a lock on a secondary index reports the primary
// key after the indexed columns, and a view may have been read before a column
// was added. A prefix match on the components both have is the honest answer.
func keyMatches(rowKey, lockKey []string) bool {
	n := len(rowKey)
	if len(lockKey) < n {
		n = len(lockKey)
	}
	if n == 0 {
		return false
	}
	for i := 0; i < n; i++ {
		if !componentMatches(rowKey[i], lockKey[i]) {
			return false
		}
	}
	return true
}

// componentMatches compares one key component against one from LOCK_DATA.
//
// LOCK_DATA quotes strings and renders binary as 0x hex, while the row comes
// back as the raw value, so those two spellings are reconciled here.
func componentMatches(value, lock string) bool {
	if value == lock {
		return true
	}
	if strings.EqualFold(lock, "0x"+hex.EncodeToString([]byte(value))) {
		return true
	}
	// A trailing space is stripped from a CHAR column by the server but kept in
	// LOCK_DATA, and vice versa depending on the type.
	return strings.TrimRight(value, " ") == strings.TrimRight(lock, " ")
}

// SplitLockData breaks a LOCK_DATA value into its key components.
//
// The format is comma-separated, with string values in single quotes; a comma
// inside a quoted value is part of the value, which is why this is not a
// strings.Split.
func SplitLockData(data string) []string {
	data = strings.TrimSpace(data)
	if data == "" || strings.Contains(strings.ToLower(data), "supremum") {
		return nil
	}
	var (
		out     []string
		current strings.Builder
		inQuote bool
	)
	flush := func() {
		out = append(out, unquoteComponent(current.String()))
		current.Reset()
	}
	for i := 0; i < len(data); i++ {
		c := data[i]
		switch {
		case c == '\'':
			// A doubled quote inside a quoted value is an escaped quote, not the
			// end of it.
			if inQuote && i+1 < len(data) && data[i+1] == '\'' {
				current.WriteByte(c)
				current.WriteByte(c)
				i++
				continue
			}
			inQuote = !inQuote
			current.WriteByte(c)
		case c == ',' && !inQuote:
			flush()
		default:
			current.WriteByte(c)
		}
	}
	flush()
	return out
}

func unquoteComponent(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		s = s[1 : len(s)-1]
		s = strings.ReplaceAll(s, "''", "'")
	}
	return s
}

// ------------------------------------------------------------- run access

// dataTimeout bounds a read of the scratch database. A plain SELECT takes no
// row locks, but it can still queue behind a metadata lock a DDL step is
// holding — which is a scenario this tool ships, so the read has to give up and
// say so rather than hang the pane.
const dataTimeout = 5 * time.Second

// observer returns the connection reads run on: a session of the run's own,
// outside every actor's transaction, in autocommit.
func (r *Run) observer() (*sql.DB, string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.setupDB, r.database, !r.closed && r.setupDB != nil
}

// TableList returns the tables of this run's scratch database.
func (r *Run) TableList(ctx context.Context) ([]TableInfo, error) {
	db, schema, ok := r.observer()
	if !ok {
		return nil, fmt.Errorf("this run has no database to read: it is closed, or still starting")
	}
	ctx, cancel := context.WithTimeout(ctx, dataTimeout)
	defer cancel()
	return Tables(ctx, db, schema)
}

// TableView reads one table in the order of one index and annotates it with the
// run's most recent lock snapshot.
//
// The annotation deliberately uses the last published snapshot rather than
// taking a fresh one: the Locks tab and this view are two readings of the same
// moment, and they would be worse than useless if they disagreed about which
// locks were held.
func (r *Run) TableView(ctx context.Context, table, index string, limit int) (TableView, error) {
	db, schema, ok := r.observer()
	if !ok {
		return TableView{}, fmt.Errorf("this run has no database to read: it is closed, or still starting")
	}
	ctx, cancel := context.WithTimeout(ctx, dataTimeout)
	defer cancel()

	tables, err := Tables(ctx, db, schema)
	if err != nil {
		return TableView{}, err
	}
	var info TableInfo
	found := false
	for _, t := range tables {
		if strings.EqualFold(t.Name, table) {
			info, found = t, true
			break
		}
	}
	if !found {
		return TableView{}, fmt.Errorf("no table %q in %s", table, schema)
	}

	view := FetchTable(ctx, db, info, index, limit)

	r.mu.Lock()
	snap := r.lastLocks
	r.mu.Unlock()
	Annotate(&view, info, snap)
	return view, nil
}
