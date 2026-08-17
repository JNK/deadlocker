package chat

import (
	"math/rand"
	"strings"
)

// BuildPrompts is the pool the builder draws its starting suggestions from.
//
// Each one names a distinct, real locking phenomenon rather than a vague
// prompt, because the assistant does better with a concrete target and the user
// gets a menu of things worth demonstrating.
var BuildPrompts = []string{
	"A gap lock on a UUIDv7 primary key blocking an unrelated insert",
	"Two transactions deadlocking on the same unique key",
	"An UPDATE with no usable index locking every row it scans",
	"Check-then-insert: two compatible gap locks that deadlock the moment both insert",
	"The classic AB-BA deadlock on two rows touched in opposite order",
	"A lock wait timeout leaving the transaction open and still holding its locks",
	"The same gap lock scenario under REPEATABLE READ and under READ COMMITTED",
	"SERIALIZABLE turning a plain SELECT into a locking read",
	"A foreign key making a child insert lock the parent row",
	"A secondary index lookup locking both the index entry and the clustered record",
	"A range scan taking next-key locks past the end of the requested range",
	"An insert intention lock waiting on another transaction's gap lock",
	"INSERT ... ON DUPLICATE KEY UPDATE taking an exclusive lock when it conflicts",
	"REPLACE INTO deleting and reinserting a row under lock",
	"SELECT ... FOR UPDATE SKIP LOCKED used as a work queue consumer",
	"SELECT ... FOR UPDATE NOWAIT failing immediately instead of waiting",
	"A deadlock between an UPDATE and an INSERT competing for the same gap",
	"Auto-increment contention between two concurrent inserts",
	"A gap lock on a secondary index blocking a value that does not exist yet",
	"Two sessions upgrading a shared lock to exclusive and deadlocking",
	"Phantom prevention: the same range query returning the same rows twice",
	"An unindexed DELETE locking every row in the table",
	"Lock ordering as a deadlock fix: the same rows touched in a consistent order",
	"A long-running transaction blocking an ALTER TABLE behind a metadata lock",
	"innodb_deadlock_detect turned off, so a deadlock becomes two timeouts",
	"A covering index avoiding clustered index locks entirely",
	"FOR SHARE and FOR UPDATE compatibility on the same row",
	"A deadlock caused by two WHERE clauses matching rows in opposite order",
	"An INSERT blocking on a gap lock taken by a SELECT against an empty table",
	"What locks a WHERE clause takes when it matches no rows at all",
	"A duplicate key check taking a shared lock before it reports the error",
	"Contention on a single hot counter row updated by everyone",
	"Optimistic locking with a version column versus SELECT ... FOR UPDATE",
	"Locks held until COMMIT, long after the statement itself finished",
	"A ROLLBACK releasing locks and unblocking a waiting transaction",
	"SAVEPOINT and a partial rollback, with locks retained across it",
	"A unique index lookup that finds a row and takes no gap lock at all",
	"A DELETE and an INSERT racing for the same primary key",
	"Two transactions inserting different keys into the same gap",
	"Reading your own uncommitted write inside a transaction",
	"A consistent read that never blocks behind an uncommitted UPDATE",
	"READ UNCOMMITTED showing a value that is later rolled back",
	"A three-way deadlock spanning three transactions",
	"Locking a row that another transaction has already deleted",
	"An UPDATE that changes an indexed column and moves the row within the index",
	"A batch UPDATE holding hundreds of row locks at once",
	"Prepared statements taking exactly the same locks as plain queries",
	"A SELECT ... FOR UPDATE on a range that is entirely empty",
	"Two transactions deadlocking through a foreign key parent row",
	"An INSERT that waits, then succeeds, because the blocker rolled back",
}

// DiscussPrompts are openers for talking about an existing scenario.
var DiscussPrompts = []string{
	"Why does this block?",
	"What changes under READ COMMITTED?",
	"Run it and show me the locks after each step",
	"Which lock mode is responsible for the wait?",
	"What would make this deadlock instead of just block?",
	"Is the gap lock actually necessary here?",
	"How would adding an index change this?",
	"What happens if I swap the order of the last two steps?",
	"Show me the wait-for graph at the moment it blocks",
	"Would SKIP LOCKED avoid this contention?",
	"What does the InnoDB deadlock report say about it?",
	"How would I avoid this in application code?",
}

// SamplePrompts returns n prompts drawn at random without repeats.
func SamplePrompts(mode Mode, n int) []string {
	pool := BuildPrompts
	if mode == ModeDiscuss {
		pool = DiscussPrompts
	}
	if n <= 0 || n > len(pool) {
		n = 3
	}

	idx := rand.Perm(len(pool))[:n]
	out := make([]string, 0, n)
	for _, i := range idx {
		out = append(out, strings.TrimSpace(pool[i]))
	}
	return out
}
