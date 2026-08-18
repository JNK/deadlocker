package mysqlbox

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Container ownership.
//
// Every container this process starts is labelled with the id of the process
// that started it, and that process holds an exclusive lock on a file named
// after the id for as long as it lives. That is what makes "is the owner still
// running?" answerable: the kernel drops a flock when the process exits, however
// it exits, so a lock nobody holds is an owner that is gone.
//
// The alternative — reaping every container carrying the tool's label — meant
// that starting a second Deadlocker, or running `deadlocker run` while the UI
// was open, deleted the MySQL out from under the first one. A PID would not have
// fixed it either: PIDs are reused, and the failure mode of guessing wrong is
// deleting a container someone is using.

const (
	// LabelOwner carries the id of the instance that created the container.
	LabelOwner = "io.jnk.deadlocker.owner"

	instancesDir = "instances"
)

// Owner is this process's claim on the containers it starts.
type Owner struct {
	id   string
	path string
	file *os.File
}

// NewOwner registers this process as a container owner under dir, which is
// normally the directory holding the state file. The returned Owner holds a
// lock until it is closed.
func NewOwner(dir string) (*Owner, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(buf[:])

	locks := filepath.Join(dir, instancesDir)
	if err := os.MkdirAll(locks, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(locks, id+".lock")

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	// The contents are for a human reading the directory; the lock is the truth.
	fmt.Fprintf(f, "pid %d\nstarted %s\n", os.Getpid(), time.Now().Format(time.RFC3339))

	return &Owner{id: id, path: path, file: f}, nil
}

// ID is the value written into the container label.
func (o *Owner) ID() string {
	if o == nil {
		return ""
	}
	return o.id
}

// Close releases the claim and removes the lock file.
func (o *Owner) Close() error {
	if o == nil || o.file == nil {
		return nil
	}
	_ = syscall.Flock(int(o.file.Fd()), syscall.LOCK_UN)
	err := o.file.Close()
	o.file = nil
	_ = os.Remove(o.path)
	return err
}

// ownerAlive reports whether the instance that labelled a container is still
// running. An owner with no lock file, or one whose lock can be taken, is gone.
func ownerAlive(dir, id string) bool {
	if id == "" {
		return false
	}
	// The id goes into a path, so it must not be able to point outside it.
	if strings.ContainsAny(id, `/\.`) {
		return false
	}
	path := filepath.Join(dir, instancesDir, id+".lock")
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// Somebody holds it: the owner is alive.
		return true
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	// Nobody holds it, so the file is litter from an instance that is gone.
	_ = os.Remove(path)
	return false
}
