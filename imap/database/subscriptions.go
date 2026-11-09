package database

import (
	bolt "go.etcd.io/bbolt"
)

// unsubscribedBucket holds the label IDs of the mailboxes a client
// unsubscribed from, as keys with no value: a mailbox is subscribed unless it
// is here, as gluon subscribes every new mailbox. wipe leaves it, so a
// rebuild keeps subscriptions; Mailbox.Delete drops a mailbox's entry.
var unsubscribedBucket = []byte("unsubscribed")

// SetSubscribed subscribes to the mailbox labelID or unsubscribes from it.
// Doing what is already done is no error.
func (u *User) SetSubscribed(labelID string, subscribed bool) error {
	return u.db.Update(func(tx *bolt.Tx) error {
		if subscribed {
			if b := tx.Bucket(unsubscribedBucket); b != nil {
				return b.Delete([]byte(labelID))
			}
			return nil
		}
		b, err := tx.CreateBucketIfNotExists(unsubscribedBucket)
		if err != nil {
			return err
		}
		return b.Put([]byte(labelID), nil)
	})
}

// Unsubscribed returns the label IDs of the mailboxes unsubscribed from.
func (u *User) Unsubscribed() (map[string]bool, error) {
	ids := make(map[string]bool)
	err := u.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(unsubscribedBucket)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, _ []byte) error {
			ids[string(k)] = true
			return nil
		})
	})
	return ids, err
}
