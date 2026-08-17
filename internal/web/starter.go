package web

// starterYAML is the scenario the playground opens with. It is deliberately a
// complete, runnable example rather than an empty file: the fastest way to
// understand the format is to run it and then change one thing.
const starterYAML = `name: My experiment
category: Playground
description: |
  Two sessions, one table. Change the statements below and run it to see
  which locks each one takes and where they collide.
tags: [scratch]

mysql:
  image: mysql:8.4
  isolation: REPEATABLE READ
  lock_wait_timeout: 8

schema:
  - |
    CREATE TABLE accounts (
      id     INT PRIMARY KEY,
      owner  VARCHAR(64) NOT NULL,
      cents  BIGINT NOT NULL,
      KEY idx_owner (owner)
    ) ENGINE=InnoDB

seed:
  - INSERT INTO accounts (id, owner, cents) VALUES (10, 'ada', 5000), (20, 'grace', 7000)

actors:
  - id: a
    name: Session A
    accent: blue
  - id: b
    name: Session B
    accent: amber

steps:
  - actor: a
    label: Open a transaction
    sql: BEGIN
    expect: ok

  - actor: b
    label: Open a transaction
    sql: BEGIN
    expect: ok

  - actor: a
    label: Lock account 10
    sql: SELECT * FROM accounts WHERE id = 10 FOR UPDATE
    note: A record lock on the primary key, because the row exists and id is unique.
    expect: ok

  - actor: b
    label: Try to lock the same row
    sql: SELECT * FROM accounts WHERE id = 10 FOR UPDATE
    note: Blocks until Session A commits or rolls back.
    expect: blocks

  - actor: a
    label: Commit
    sql: COMMIT
    note: Session B's statement completes as soon as this lands.
    expect: ok

  - actor: b
    label: Commit
    sql: COMMIT
    expect: ok
`
