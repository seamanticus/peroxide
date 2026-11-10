package config

import (
	"errors"
	"syscall"
	"testing"
)

// The serve lock: one holder at a time, seen by ServeRunning, released for
// the next holder. Two opens of the file in one process conflict as they do
// across processes (flock locks an open file, not a process).
// evidence: witnesses
func TestServeLock(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if running, err := ServeRunning(); err != nil || running {
		t.Fatalf("ServeRunning() before = %v, %v; want false", running, err)
	}
	l, err := LockServe()
	if err != nil {
		t.Fatalf("LockServe() = %v", err)
	}
	if running, err := ServeRunning(); err != nil || !running {
		t.Errorf("ServeRunning() while held = %v, %v; want true", running, err)
	}
	if _, err := LockServe(); !errors.Is(err, ErrServeRunning) {
		t.Errorf("second LockServe() = %v, want ErrServeRunning", err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if running, err := ServeRunning(); err != nil || running {
		t.Errorf("ServeRunning() after release = %v, %v; want false", running, err)
	}
	l, err = LockServe()
	if err != nil {
		t.Fatalf("LockServe() after release = %v", err)
	}
	l.Release()
}

// A check in progress, holding the lock shared, is not a serve: another
// check still finds none running, and LockServe waits it out.
// evidence: witnesses
func TestServeRunningChecksDontConflict(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	check, err := openServeLock()
	if err != nil {
		t.Fatal(err)
	}
	if held, err := tryLock(check, syscall.LOCK_SH); err != nil || held {
		t.Fatalf("shared lock = %v, %v", held, err)
	}
	if running, err := ServeRunning(); err != nil || running {
		t.Errorf("ServeRunning() beside a check = %v, %v; want false", running, err)
	}

	// Held for good, the check looks like a serve that never ends.
	if _, err := LockServe(); !errors.Is(err, ErrServeRunning) {
		t.Errorf("LockServe() beside a lasting shared lock = %v, want ErrServeRunning", err)
	}
	// Released, it is gone.
	if err := check.Close(); err != nil {
		t.Fatal(err)
	}
	l, err := LockServe()
	if err != nil {
		t.Fatalf("LockServe() after the check = %v", err)
	}
	l.Release()
}
