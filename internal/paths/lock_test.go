package paths

import (
	"errors"
	"os"
	"path/filepath"
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

// The configuration and data directories are left closed to other users,
// even when they existed before with a wider mode; one closed already keeps
// its mode.
// evidence: witnesses
func TestDirectoriesOwnerOnly(t *testing.T) {
	configHome, dataHome := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_DATA_HOME", dataHome)
	for _, tc := range []struct {
		name string
		dir  string
		path func(string) (string, error)
	}{
		{"Path", filepath.Join(configHome, "peroxide"), Path},
		{"DataPath", filepath.Join(dataHome, "peroxide"), DataPath},
	} {
		for _, tm := range []struct{ before, want os.FileMode }{{0, 0o700}, {0o755, 0o700}, {0o777, 0o700}, {0o500, 0o500}} {
			before := tm.before
			if before != 0 {
				// os.Chmod: Mkdir's mode is narrowed by the umask.
				if err := os.MkdirAll(tc.dir, before); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(tc.dir, before); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := tc.path("x"); err != nil {
				t.Fatalf("%v() with the directory %o = %v", tc.name, before, err)
			}
			fi, err := os.Stat(tc.dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := fi.Mode().Perm(); got != tm.want {
				t.Errorf("%v() with the directory %o left it %o, want %o", tc.name, before, got, tm.want)
			}
			if err := os.RemoveAll(tc.dir); err != nil {
				t.Fatal(err)
			}
		}
	}
}
