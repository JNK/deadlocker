package agentapi

// FormatDoc is the canonical description of the scenario file format. It is
// served as an MCP resource and injected into the built-in chat's system
// prompt, so there is exactly one description of the format to keep correct.
const FormatDoc = "" + `# Deadlocker scenario format

A scenario is a YAML document describing a schema, a set of actors — each with
its own dedicated MySQL connection — and an ordered list of statements. Running
one steps through those statements while reporting the locks each takes.

## Complete example

` + "```yaml" + `
name: SELECT FOR UPDATE on a missing row blocks the next insert
category: Gap locks          # optional; the folder name is used otherwise
tags: [gap-lock, uuidv7]
description: |
  Markdown. Explain what the scenario demonstrates and what to watch for.

mysql:
  image: mysql:8.4           # optional, this is the default
  isolation: REPEATABLE READ # READ UNCOMMITTED | READ COMMITTED | REPEATABLE READ | SERIALIZABLE
  lock_wait_timeout: 300     # innodb_lock_wait_timeout in seconds, per session
  deadlock_detect: true      # optional global; false turns deadlocks into timeouts
  prepared: false            # optional; true uses prepared statements (binary protocol)
  vars:                      # optional extra SET SESSION variables
    sql_mode: STRICT_ALL_TABLES

schema:
  - |
    CREATE TABLE bookings (
      id    CHAR(36) NOT NULL,
      guest VARCHAR(64) NOT NULL,
      PRIMARY KEY (id)
    ) ENGINE=InnoDB

seed:
  - INSERT INTO bookings (id, guest) VALUES ('01912f00-0000-7000-8000-000000000001', 'ada')

actors:
  - id: a
    name: Request A
    accent: blue             # blue | amber | violet | teal | rose
  - id: b
    name: Request B
    accent: amber

steps:
  - actor: a
    label: Open a transaction   # optional; defaults to a summary of the SQL
    sql: BEGIN
    note: Prose shown beside the step in the timeline.
    expect: ok

  - actor: a
    label: Look up a row that does not exist
    sql: SELECT * FROM bookings WHERE id = '01912f90-0000-7000-8000-0000000000aa' FOR UPDATE
    expect: ok

  - actor: b
    label: Open a transaction
    sql: BEGIN
    expect: ok

  - actor: b
    label: Insert a new booking
    sql: INSERT INTO bookings (id, guest) VALUES ('01912f95-0000-7000-8000-0000000000bb', 'marie')
    note: Blocks on the gap lock Request A took over the supremum gap.
    expect: blocks

  - actor: a
    label: Commit
    sql: COMMIT
    expect: ok

  - actor: b
    label: Commit
    sql: COMMIT
    expect: ok
` + "```" + `

## Required fields

- ` + "`name`" + ` — a human readable title.
- ` + "`actors`" + ` — at least one, each with a unique ` + "`id`" + `.
- ` + "`steps`" + ` — at least one, each with an ` + "`actor`" + ` that exists and an ` + "`sql`" + `.

Everything else is optional. Unknown keys are a hard error, so do not invent
fields.

## expect values

| value      | meaning                                                        |
|------------|----------------------------------------------------------------|
| ` + "`ok`" + `       | completes without error                                        |
| ` + "`blocks`" + `   | hits a lock wait; still satisfied if it later completes or fails |
| ` + "`error`" + `    | any failure, including deadlock and timeout                    |
| ` + "`deadlock`" + ` | errno 1213, this transaction chosen as the victim              |
| ` + "`timeout`" + `  | errno 1205, innodb_lock_wait_timeout fired                     |

Omit ` + "`expect`" + ` when the outcome is genuinely not deterministic — for instance
which transaction InnoDB picks as the victim in a symmetric deadlock.

## The ordering rule — the most common mistake

Each actor has one connection, and a connection runs one statement at a time.
An actor whose statement is blocked **cannot** be given another statement. If
step N for actor ` + "`b`" + ` is expected to block, the next step for ` + "`b`" + ` must come
after whatever releases the lock — usually a ` + "`COMMIT`" + ` or ` + "`ROLLBACK`" + ` on the
other actor.

Wrong — the scenario wedges forever, because ` + "`b`" + ` can never run step 3 and
step 4 is never reached:

    1. a: SELECT ... FOR UPDATE
    2. b: SELECT ... FOR UPDATE   # expect: blocks
    3. b: SELECT something else
    4. a: COMMIT

Right:

    1. a: SELECT ... FOR UPDATE
    2. b: SELECT ... FOR UPDATE   # expect: blocks
    3. a: COMMIT                  # releases b
    4. b: SELECT something else

If you need a third concurrent statement while two actors are already busy,
add a third actor rather than reusing a blocked one.

## Practical guidance

- Use ` + "`ENGINE=InnoDB`" + `. Gap locks, next-key locks and deadlock detection are
  InnoDB features.
- Give every actor an explicit ` + "`BEGIN`" + ` before the statements that should share
  a transaction. Without one, autocommit releases locks after each statement
  and nothing interesting happens.
- Keep ` + "`lock_wait_timeout`" + ` high (300 is the default) unless the timeout itself
  is the lesson. Scenarios are stepped through by hand and a statement expiring
  while the user reads an explanation teaches the wrong thing.
- Gap locks only exist in REPEATABLE READ and SERIALIZABLE. To demonstrate that
  a gap lock is responsible for a block, make the same scenario under
  READ COMMITTED as a companion — it will not block.
- Lock modes worth aiming at: ` + "`X,REC_NOT_GAP`" + ` (record only),
  ` + "`X,GAP`" + ` (gap only), bare ` + "`X`" + ` (next-key: record plus the gap before it),
  and ` + "`X,INSERT_INTENTION`" + ` (what an INSERT needs, and what conflicts with
  another transaction's gap lock).
- To provoke a deadlock, have two transactions take locks in opposite order,
  or have both take a gap lock over the same gap and then both insert into it.

## Workflow

1. Read existing scenarios with ` + "`list_scenarios`" + ` and ` + "`get_scenario`" + ` for
   patterns to follow.
2. Call ` + "`validate_scenario`" + ` and fix any error or warning.
3. Call ` + "`start_run`" + `, then ` + "`step_run`" + ` through the whole thing to confirm it
   behaves as claimed. Each step returns the lock state it produced.
4. Only then ` + "`create_scenario`" + ` or ` + "`update_scenario`" + `. Pass a short
   ` + "`note`" + ` when updating: every write is kept in the scenario's version
   history and the note is what makes that history readable later.
5. Call ` + "`close_run`" + ` when done, which drops the scratch database.

Editing is safe to attempt: nothing is ever overwritten irrecoverably. Use
` + "`list_scenario_versions`" + ` to see what a scenario used to say,
` + "`get_scenario_version`" + ` to read one revision, and
` + "`restore_scenario_version`" + ` to put it back. Restoring appends rather than
truncates, so it can itself be undone.
`
