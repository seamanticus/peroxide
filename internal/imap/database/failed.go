package database

import (
	bolt "go.etcd.io/bbolt"
)

// failedBucket holds the IDs of the messages that failed to download or build
// for a reason that retrying soon won't change, as proton-bridge keeps them
// (syncservice.Status.FailedMessages). The background download skips them;
// a FETCH, an event about the message or download-mailbox tries again, and
// success takes them out. Wipe empties it, as a resync does bridge's.
var failedBucket = []byte("failed")

// AddFailed records that apiID failed to download or build.
func (u *User) AddFailed(apiID string) error {
	return u.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(failedBucket)
		if err != nil {
			return err
		}
		return b.Put([]byte(apiID), nil)
	})
}

// RemoveFailed takes apiID out of the failed messages, if it is there.
func (u *User) RemoveFailed(apiID string) error {
	return u.db.Update(func(tx *bolt.Tx) error {
		if b := tx.Bucket(failedBucket); b != nil {
			return b.Delete([]byte(apiID))
		}
		return nil
	})
}

// Failed reports whether apiID is among the failed messages.
func (u *User) Failed(apiID string) (bool, error) {
	var failed bool
	err := u.db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket(failedBucket); b != nil {
			failed = b.Get([]byte(apiID)) != nil
		}
		return nil
	})
	return failed, err
}

// FailedCount returns the number of failed messages.
func (u *User) FailedCount() (int, error) {
	var n int
	err := u.db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket(failedBucket); b != nil {
			n = b.Stats().KeyN
		}
		return nil
	})
	return n, err
}
