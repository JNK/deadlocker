package engine

import (
	"reflect"
	"testing"
)

func TestSplitLockData(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"5", []string{"5"}},
		{"'alice'", []string{"alice"}},
		{"5, 'alice'", []string{"5", "alice"}},
		{"'o''brien', 7", []string{"o'brien", "7"}},
		{"'a, b', 2", []string{"a, b", "2"}},
		{"supremum pseudo-record", nil},
		{"", nil},
	}
	for _, c := range cases {
		got := SplitLockData(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("SplitLockData(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestKeyColumnsAppendsThePrimaryKey(t *testing.T) {
	secondary := IndexInfo{Name: "idx_guest", Columns: []string{"guest"}}
	got := keyColumns(secondary, []string{"id"})
	if want := []string{"guest", "id"}; !reflect.DeepEqual(got, want) {
		t.Errorf("secondary key columns = %v, want %v", got, want)
	}

	primary := IndexInfo{Name: "PRIMARY", Columns: []string{"id"}}
	if got := keyColumns(primary, []string{"id"}); !reflect.DeepEqual(got, []string{"id"}) {
		t.Errorf("primary key columns = %v, want [id]", got)
	}

	// A secondary index that already covers the primary key does not repeat it.
	covering := IndexInfo{Name: "idx_id_guest", Columns: []string{"id", "guest"}}
	if got := keyColumns(covering, []string{"id"}); !reflect.DeepEqual(got, []string{"id", "guest"}) {
		t.Errorf("covering key columns = %v, want [id guest]", got)
	}
}

func TestLockKind(t *testing.T) {
	cases := map[string]string{
		"X":                      "next-key",
		"X,GAP":                  "gap",
		"X,REC_NOT_GAP":          "record",
		"X,INSERT_INTENTION":     "insert-intention",
		"S,GAP,INSERT_INTENTION": "insert-intention",
	}
	for mode, want := range cases {
		if got := lockKind(Lock{LockType: "RECORD", LockMode: mode}); got != want {
			t.Errorf("lockKind(%q) = %q, want %q", mode, got, want)
		}
	}
	if got := lockKind(Lock{LockType: "TABLE", LockMode: "IX"}); got != "table" {
		t.Errorf("a table lock should be kind table, got %q", got)
	}
}

// Annotate is where the whole feature lives or dies: a gap lock has to land on
// the gap before the row it names, a record lock on the row itself, and a lock
// on a key nobody can see has to be reported rather than quietly dropped.
func TestAnnotatePlacesLocks(t *testing.T) {
	table := TableInfo{
		Name: "bookings",
		Indexes: []IndexInfo{
			{Name: "PRIMARY", Columns: []string{"id"}, KeyColumns: []string{"id"}, Orderable: true},
		},
	}
	view := TableView{
		Table:   "bookings",
		Index:   "PRIMARY",
		Columns: []string{"id", "guest"},
		Rows: []DataRow{
			{Values: []string{"1", "ada"}, Null: []bool{false, false}, Key: []string{"1"}},
			{Values: []string{"9", "grace"}, Null: []bool{false, false}, Key: []string{"9"}},
		},
	}
	snap := &LockSnapshot{
		Locks: []Lock{
			{Table: "bookings", Index: "PRIMARY", LockType: "RECORD", LockMode: "X,REC_NOT_GAP", LockData: "1", Actor: "a", LockStatus: "GRANTED"},
			{Table: "bookings", Index: "PRIMARY", LockType: "RECORD", LockMode: "X,GAP", LockData: "9", Actor: "a", LockStatus: "GRANTED"},
			{Table: "bookings", Index: "PRIMARY", LockType: "RECORD", LockMode: "X", LockData: "supremum pseudo-record", Actor: "b", LockStatus: "GRANTED"},
			{Table: "bookings", Index: "PRIMARY", LockType: "RECORD", LockMode: "X,REC_NOT_GAP", LockData: "42", Actor: "b", LockStatus: "WAITING"},
			{Table: "other", Index: "PRIMARY", LockType: "RECORD", LockMode: "X", LockData: "1", Actor: "b", LockStatus: "GRANTED"},
			{Table: "bookings", LockType: "TABLE", LockMode: "IX", Actor: "a", LockStatus: "GRANTED"},
		},
	}

	Annotate(&view, table, snap)

	if len(view.Rows[0].Locks) != 1 || view.Rows[0].Locks[0].Kind != "record" {
		t.Errorf("row 1 should carry the record lock, got %#v", view.Rows[0].Locks)
	}
	if len(view.Rows[0].GapLocks) != 0 {
		t.Errorf("a REC_NOT_GAP lock leaves the gap free, got %#v", view.Rows[0].GapLocks)
	}
	if len(view.Rows[1].GapLocks) != 1 || view.Rows[1].GapLocks[0].Kind != "gap" {
		t.Errorf("row 9 should carry a gap lock before it, got %#v", view.Rows[1].GapLocks)
	}
	if len(view.Rows[1].Locks) != 0 {
		t.Errorf("a GAP lock does not lock the record, got %#v", view.Rows[1].Locks)
	}
	if len(view.Supremum) != 1 {
		t.Errorf("the supremum lock should be kept apart, got %#v", view.Supremum)
	}
	if len(view.Unmatched) != 1 || view.Unmatched[0].Data != "42" {
		t.Errorf("a lock on an invisible key should be reported, got %#v", view.Unmatched)
	}
}

// A lock taken through a different index describes a key this view is not
// ordered by, so drawing it on a row would be a guess. It is reported as what it
// is — a lock in another index — rather than as a key nobody can account for.
func TestAnnotateSeparatesOtherIndexes(t *testing.T) {
	table := TableInfo{Name: "t", Indexes: []IndexInfo{{Name: "PRIMARY", Columns: []string{"id"}, KeyColumns: []string{"id"}}}}
	view := TableView{
		Table: "t", Index: "PRIMARY",
		Rows: []DataRow{{Values: []string{"1"}, Key: []string{"1"}}},
	}
	snap := &LockSnapshot{Locks: []Lock{
		{Table: "t", Index: "idx_guest", LockType: "RECORD", LockMode: "X", LockData: "1", Actor: "a"},
	}}
	Annotate(&view, table, snap)
	if len(view.Rows[0].Locks) != 0 || len(view.Rows[0].GapLocks) != 0 {
		t.Errorf("a secondary index lock must not be drawn on the primary key row")
	}
	if len(view.OtherIndex) != 1 || view.OtherIndex[0].Index != "idx_guest" {
		t.Errorf("it belongs in the other-index list, got %#v", view.OtherIndex)
	}
	if len(view.Unmatched) != 0 {
		t.Errorf("and not in the unmatched list, got %#v", view.Unmatched)
	}
}

// Re-annotating replaces what was there: the pane refreshes after every step,
// and locks that have been released must not linger.
func TestAnnotateIsIdempotent(t *testing.T) {
	table := TableInfo{Name: "t", Indexes: []IndexInfo{{Name: "PRIMARY", Columns: []string{"id"}, KeyColumns: []string{"id"}}}}
	view := TableView{Table: "t", Index: "PRIMARY", Rows: []DataRow{{Values: []string{"1"}, Key: []string{"1"}}}}
	held := &LockSnapshot{Locks: []Lock{
		{Table: "t", Index: "PRIMARY", LockType: "RECORD", LockMode: "X", LockData: "1", Actor: "a"},
	}}
	Annotate(&view, table, held)
	Annotate(&view, table, held)
	if got := len(view.Rows[0].Locks); got != 1 {
		t.Errorf("annotating twice duplicated the lock: %d", got)
	}
	Annotate(&view, table, &LockSnapshot{})
	if got := len(view.Rows[0].Locks); got != 0 {
		t.Errorf("a released lock should be gone, still have %d", got)
	}
}

func TestComponentMatchesBinaryKeys(t *testing.T) {
	if !componentMatches("abc", "0x616263") {
		t.Error("a binary key rendered as hex in LOCK_DATA should match its raw value")
	}
	if componentMatches("abc", "0x000000") {
		t.Error("different bytes must not match")
	}
}
