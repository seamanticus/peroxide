package paths

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// serveLockFile is held by peroxide serve while it runs, so that other
// peroxide commands know not to log in an account themselves: Proton's
// refresh tokens are single use, and a second process refreshing an account
// serve holds would leave serve's token spent.
const serveLockFile = "serve.lock"

// ServeLock is the lock peroxide serve holds; the kernel releases it when the
// process ends, however it ends.
type ServeLock struct{ f *os.File }

// ErrServeRunning is returned by LockServe when another process holds the
// lock.
var ErrServeRunning = errors.New("peroxide serve is already running")

func openServeLock() (*os.File, error) {
	p, err := Path(serveLockFile)
	if err != nil {
		return nil, fmt.Errorf("cannot find the serve lock: %w", err)
	}
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cannot open the serve lock: %w", err)
	}
	return f, nil
}

// tryLock takes a flock of kind (LOCK_EX or LOCK_SH) on f without waiting;
// held reports that another open file holds a lock in the way.
func tryLock(f *os.File, kind int) (held bool, err error) {
	err = syscall.Flock(int(f.Fd()), kind|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	return false, err
}

// lockServeTries and lockServeWait bound how long LockServe waits out a
// ServeRunning check, which holds the lock shared for an instant; a serve holds
// it for good.
const (
	lockServeTries = 20
	lockServeWait  = 10 * time.Millisecond
)

// LockServe takes the serve lock for the life of the process, or returns
// ErrServeRunning.
func LockServe() (*ServeLock, error) {
	f, err := openServeLock()
	if err != nil {
		return nil, err
	}
	for range lockServeTries {
		held, err := tryLock(f, syscall.LOCK_EX)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("cannot take the serve lock: %w", err)
		}
		if !held {
			return &ServeLock{f}, nil
		}
		time.Sleep(lockServeWait)
	}
	f.Close()
	return nil, ErrServeRunning
}

// Release gives the lock back.
func (l *ServeLock) Release() error {
	return l.f.Close()
}

// ServeRunning reports whether a peroxide serve holds the serve lock.
func ServeRunning() (bool, error) {
	f, err := openServeLock()
	if err != nil {
		return false, err
	}
	defer f.Close()
	// Shared, so that checks running at once don't see each other as a
	// serve; only serve's exclusive lock is in the way. Closing f releases it.
	held, err := tryLock(f, syscall.LOCK_SH)
	if err != nil {
		return false, fmt.Errorf("cannot check the serve lock: %w", err)
	}
	return held, nil
}
