package config

import (
	"errors"
	"fmt"
	"os"
	"syscall"
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

// tryLock takes an exclusive flock on f without waiting; held reports that
// another open file holds it.
func tryLock(f *os.File) (held bool, err error) {
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	return false, err
}

// LockServe takes the serve lock for the life of the process, or returns
// ErrServeRunning.
func LockServe() (*ServeLock, error) {
	f, err := openServeLock()
	if err != nil {
		return nil, err
	}
	held, err := tryLock(f)
	if err != nil || held {
		f.Close()
		if held {
			return nil, ErrServeRunning
		}
		return nil, fmt.Errorf("cannot take the serve lock: %w", err)
	}
	return &ServeLock{f}, nil
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
	held, err := tryLock(f)
	if err != nil {
		return false, fmt.Errorf("cannot check the serve lock: %w", err)
	}
	// Taken just to look: closing f releases it.
	return held, nil
}
