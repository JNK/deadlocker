#!/usr/bin/env python3
"""Run every scenario in the library end to end and report expectation verdicts.

This is the regression test for the case library: it starts a real run for each
case, steps through it (waiting when an actor is legitimately blocked), and
prints any step whose observed outcome did not match what the scenario claims.

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


def drive(run_id):
    """Advance until every step has been submitted, tolerating blocked actors."""
    stalls = 0
    while True:
        res = call("POST", "/run/%s/step" % run_id)
        if res.get("ok"):
            stalls = 0
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

    for cid in cases:
        print("- %s" % cid)
        try:
            run_id = start_run(cid)
        except urllib.error.HTTPError as e:
            print("    could not start: %s" % e)
            failures.append(cid)
            continue

        drive(run_id)
        time.sleep(1.2)  # let any late-resolving blocked step land

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
        call("POST", "/run/%s/close" % run_id)
        print()

    if failures:
        print("mismatches in %d of %d scenarios: %s" % (len(failures), len(cases), ", ".join(failures)))
        return 1
    print("all %d scenarios behaved as documented" % len(cases))
    return 0


if __name__ == "__main__":
    sys.exit(main())
