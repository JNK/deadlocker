#!/usr/bin/env python3
"""Run every scenario in the library end to end and report expectation verdicts.

This is the regression test for the case library: it starts a real run for each
case, steps through it (waiting when an actor is legitimately blocked), and
prints any step whose observed outcome did not match what the scenario claims.

It also samples the lock table after every step and reports, at the end, which
InnoDB lock modes the library actually demonstrates. That is the check behind
the claim that the library covers every lock type: not that a scenario is
*named* after a gap lock, but that a gap lock was observed in
performance_schema.data_locks while it ran.

Start the server first, then:  hack/verify.py [base-url]
"""
import json
import sys
import time
import urllib.error
import urllib.request

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8899"


def call(method, path, payload=None):
    data = None
    headers = {}
    if payload is not None:
        data = json.dumps(payload).encode()
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(BASE + path, data=data, headers=headers, method=method)
    with urllib.request.urlopen(req, timeout=420) as r:
        return json.load(r)


def list_cases():
    """Scrape case ids out of the sidebar rather than adding an API just for this."""
    with urllib.request.urlopen(BASE + "/", timeout=30) as r:
        html = r.read().decode()
    ids, seen = [], set()
    for chunk in html.split('href="/case/')[1:]:
        cid = chunk.split('"')[0]
        if cid and cid not in seen:
            seen.add(cid)
            ids.append(cid)
    return ids


def start_run(case_id):
    req = urllib.request.Request(
        BASE + "/run",
        data=("case_id=" + case_id).encode(),
        headers={"Content-Type": "application/x-www-form-urlencoded"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=420) as r:
        return r.url.rstrip("/").split("/")[-1]


def sample_locks(run_id, seen):
    """Record which lock modes are visible right now."""
    try:
        res = call("POST", "/run/%s/snapshot" % run_id)
    except Exception:
        return
    snap = res.get("locks") or {}
    for lock in snap.get("locks") or []:
        key = (lock.get("lock_type") or "?", lock.get("lock_mode") or "?")
        seen.setdefault(key, set())
        if lock.get("lock_data") == "supremum pseudo-record":
            seen.setdefault(("RECORD", "supremum pseudo-record"), set())
    for mdl in snap.get("metadata_locks") or []:
        seen.setdefault(("METADATA", mdl.get("lock_type") or "?"), set())


def drive(run_id, seen=None):
    """Advance until every step has been submitted, tolerating blocked actors."""
    stalls = 0
    while True:
        res = call("POST", "/run/%s/step" % run_id)
        if res.get("ok"):
            stalls = 0
            if seen is not None:
                sample_locks(run_id, seen)
            continue
        if res.get("done"):
            return True
        if res.get("blocked_actor"):
            # The next step belongs to an actor still waiting on a lock. That is
            # legitimate mid-scenario, so wait for the server to resolve it.
            stalls += 1
            if stalls > 60:
                print("    gave up: %s" % res.get("error"))
                return False
            time.sleep(0.5)
            continue
        print("    step error: %s" % res.get("error"))
        return False


def main():
    cases = list_cases()
    print("verifying %d scenarios against %s\n" % (len(cases), BASE))
    failures = []
    seen = {}

    for cid in cases:
        print("- %s" % cid)
        try:
            run_id = start_run(cid)
        except urllib.error.HTTPError as e:
            print("    could not start: %s" % e)
            failures.append(cid)
            continue

        drive(run_id, seen)
        time.sleep(1.2)  # let any late-resolving blocked step land
        sample_locks(run_id, seen)

        report = call("GET", "/run/%s/export" % run_id)
        bad = 0
        for st in report["steps"]:
            verdict = st.get("verdict") or "--"
            if verdict == "mismatch":
                bad += 1
            print("    %2d %-4s %-8s %-9s %s" % (
                st["index"], st["actor"], st["status"], verdict,
                st.get("verdict_note") or ""))
        if report["state"].get("deadlock_report"):
            print("    + InnoDB deadlock report captured")
        if bad:
            failures.append(cid)
            print("    ==> %d mismatch(es)" % bad)
        for key in seen:
            seen[key].add(cid)
        call("POST", "/run/%s/close" % run_id)
        print()

    report_lock_coverage(seen)

    if failures:
        print("mismatches in %d of %d scenarios: %s" % (len(failures), len(cases), ", ".join(failures)))
        return 1
    print("all %d scenarios behaved as documented" % len(cases))
    return 0


EXPECTED_MODES = [
    ("TABLE", "IS"),
    ("TABLE", "IX"),
    ("TABLE", "S"),
    ("TABLE", "X"),
    ("RECORD", "S"),
    ("RECORD", "S,REC_NOT_GAP"),
    ("RECORD", "S,GAP"),
    ("RECORD", "X"),
    ("RECORD", "X,REC_NOT_GAP"),
    ("RECORD", "X,GAP"),
    ("RECORD", "X,INSERT_INTENTION"),
    ("RECORD", "supremum pseudo-record"),
    ("METADATA", "SHARED_READ"),
    ("METADATA", "SHARED_WRITE"),
    ("METADATA", "EXCLUSIVE"),
]


def report_lock_coverage(seen):
    print("lock modes observed across the library")
    missing = []
    for key in EXPECTED_MODES:
        kind, mode = key
        if key in seen:
            print("  ok   %-9s %s" % (kind, mode))
        else:
            missing.append(key)
            print("  --   %-9s %s   (not demonstrated)" % (kind, mode))

    extra = sorted(k for k in seen if k not in EXPECTED_MODES)
    for kind, mode in extra:
        print("  ok   %-9s %s   (not on the checklist)" % (kind, mode))
    print()
    if missing:
        print("%d lock mode(s) never appeared: %s" % (
            len(missing), ", ".join("%s %s" % k for k in missing)))
    else:
        print("every lock mode on the checklist was demonstrated")
    print()


if __name__ == "__main__":
    sys.exit(main())
