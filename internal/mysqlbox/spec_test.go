package mysqlbox

import (
	"slices"
	"testing"
)

// Two runs share a container whenever they can, because starting one is the
// slowest thing this tool does. They must not share one when a scenario needs a
// setting MySQL only exposes globally: a run that turned innodb_deadlock_detect
// off used to turn it off for every concurrent run on the same server, quietly
// converting their deadlocks into lock waits.
func TestSpecKeySeparatesOnlyWhatCannotBeSetPerSession(t *testing.T) {
	on, off := true, false

	same := []Spec{
		{Image: "mysql:8.4"},
		{Image: "mysql:8.4", DeadlockDetect: nil},
		{Image: "mysql:8.4", DeadlockDetect: &on},
	}
	for _, s := range same[1:] {
		if s.Key() != same[0].Key() {
			t.Errorf("%+v should share a container with the default, got key %q vs %q",
				s, s.Key(), same[0].Key())
		}
	}

	offKey := Spec{Image: "mysql:8.4", DeadlockDetect: &off}.Key()
	if offKey == same[0].Key() {
		t.Fatal("a scenario needing innodb_deadlock_detect=OFF must get a container of its own")
	}

	if k := (Spec{}).Key(); k != DefaultImage {
		t.Errorf("an unnamed image should key on the default, got %q", k)
	}
	if a, b := (Spec{Image: "mysql:5.7"}).Key(), (Spec{Image: "mysql:8.4"}).Key(); a == b {
		t.Error("different images must not share a container")
	}
}

// The setting is applied at startup rather than with SET GLOBAL, so the server
// is right from birth and no run ever reconfigures another's.
func TestServerArgsCarryDeadlockDetect(t *testing.T) {
	off := false
	args := serverArgsFor(Spec{Image: "mysql:8.4", DeadlockDetect: &off})
	if !slices.Contains(args, "--innodb-deadlock-detect=OFF") {
		t.Fatalf("want the flag at startup, got %v", args)
	}

	// The default is on; passing the flag explicitly would only be one more
	// thing an old server could reject.
	on := true
	for _, spec := range []Spec{{Image: "mysql:8.4"}, {Image: "mysql:8.4", DeadlockDetect: &on}} {
		for _, a := range serverArgsFor(spec) {
			if a == "--innodb-deadlock-detect=OFF" {
				t.Fatalf("%+v should not disable deadlock detection", spec)
			}
		}
	}

	// Every flag has to exist in every server the version matrix sweeps: 5.7
	// aborts at startup on an unknown variable.
	old := serverArgsFor(Spec{Image: "mysql:5.7"})
	if slices.Contains(old, "--innodb-redo-log-capacity=16777216") {
		t.Error("5.7 does not know innodb_redo_log_capacity")
	}
	if !slices.Contains(old, "--innodb-log-file-size=16M") {
		t.Error("5.7 wants the pre-8.0.30 spelling")
	}
}
