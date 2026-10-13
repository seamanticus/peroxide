package database

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	bolt "go.etcd.io/bbolt"

	"github.com/seamanticus/peroxide/protonmail"
)

func serializeUID(uid uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uid)
	return b
}

func unserializeUID(b []byte) uint32 {
	return binary.BigEndian.Uint32(b)
}

// mailboxCounts are how many entries a mailbox holds and how many of those have
// an unread message stored. They are kept for the mailboxes reset since the
// database was opened, and change in the same transactions as the entries and
// messages.
type mailboxCounts struct {
	total, unread uint32
}

func getCounts(tx *bolt.Tx, labelID string) (c mailboxCounts, ok bool) {
	b := tx.Bucket(countsBucket)
	if b == nil {
		return c, false
	}
	v := b.Get([]byte(labelID))
	if v == nil {
		return c, false
	}
	return mailboxCounts{binary.BigEndian.Uint32(v), binary.BigEndian.Uint32(v[4:])}, true
}

func putCounts(tx *bolt.Tx, labelID string, c mailboxCounts) error {
	b, err := tx.CreateBucketIfNotExists(countsBucket)
	if err != nil {
		return err
	}
	v := make([]byte, 8)
	binary.BigEndian.PutUint32(v, c.total)
	binary.BigEndian.PutUint32(v[4:], c.unread)
	return b.Put([]byte(labelID), v)
}

// adjustCounts adds total and unread to the counts of a mailbox, if they are
// kept.
func adjustCounts(tx *bolt.Tx, labelID string, total, unread int) error {
	c, ok := getCounts(tx, labelID)
	if !ok {
		return nil
	}
	newTotal, newUnread := int64(c.total)+int64(total), int64(c.unread)+int64(unread)
	if newTotal < 0 || newUnread < 0 || newUnread > newTotal {
		return fmt.Errorf("counts of mailbox %v out of sync: %v messages, %v unread", labelID, newTotal, newUnread)
	}
	return putCounts(tx, labelID, mailboxCounts{uint32(newTotal), uint32(newUnread)})
}

func unreadCount(unread bool) int {
	if unread {
		return 1
	}
	return 0
}

func mailboxContains(b *bolt.Bucket, apiID string) bool {
	want := []byte(apiID)
	c := b.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		if bytes.Equal(v, want) {
			return true
		}
	}
	return false
}

func mailboxCreateMessage(b *bolt.Bucket, labelID, apiID string) (seqNum uint32, err error) {
	want := []byte(apiID)
	c := b.Cursor()
	var n uint32 = 1
	for k, v := c.First(); k != nil; k, v = c.Next() {
		if bytes.Equal(v, want) {
			return n, nil
		}
		n++
	}

	id, _ := b.NextSequence()
	uid := uint32(id)
	if err := b.Put(serializeUID(uid), want); err != nil {
		return 0, err
	}

	unread, err := messageUnread(b.Tx().Bucket(messagesBucket), apiID)
	if err != nil {
		return 0, err
	}
	return n, adjustCounts(b.Tx(), labelID, 1, unreadCount(unread))
}

func mailboxDeleteMessage(b *bolt.Bucket, labelID, apiID string) (seqNum uint32, err error) {
	want := []byte(apiID)
	c := b.Cursor()
	var n uint32 = 1
	for k, v := c.First(); k != nil; k, v = c.Next() {
		if bytes.Equal(v, want) {
			unread, err := messageUnread(b.Tx().Bucket(messagesBucket), apiID)
			if err != nil {
				return 0, err
			}
			if err := b.Delete(k); err != nil {
				return 0, err
			}
			return n, adjustCounts(b.Tx(), labelID, -1, -unreadCount(unread))
		}
		n++
	}
	return 0, nil
}

type Mailbox struct {
	labelID string
	u       *User
}

func (mbox *Mailbox) bucket(tx *bolt.Tx) (*bolt.Bucket, error) {
	b := tx.Bucket(mailboxesBucket)
	if b == nil {
		return nil, errors.New("cannot find mailboxes bucket")
	}
	b = b.Bucket([]byte(mbox.labelID))
	if b == nil {
		return nil, errors.New("cannot find mailbox bucket")
	}
	return b, nil
}

func (mbox *Mailbox) Sync(messages []*protonmail.Message) error {
	return mbox.u.db.Update(func(tx *bolt.Tx) error {
		b, err := mbox.bucket(tx)
		if err != nil {
			return err
		}

		for _, msg := range messages {
			if _, err := mailboxCreateMessage(b, mbox.labelID, msg.ID); err != nil {
				return err
			}
		}

		return userSync(tx, messages)
	})
}

func (mbox *Mailbox) UidNext() (uint32, error) {
	var uid uint32
	err := mbox.u.db.View(func(tx *bolt.Tx) error {
		b, err := mbox.bucket(tx)
		if err != nil {
			return err
		}

		uid = uint32(b.Sequence() + 1)
		return nil
	})
	return uid, err
}

// Count returns the number of messages in the mailbox, which is also its
// highest sequence number, and how many of them are unread. Counts are only
// kept once the mailbox has been reset.
func (mbox *Mailbox) Count() (total, unread int, err error) {
	err = mbox.u.db.View(func(tx *bolt.Tx) error {
		c, ok := getCounts(tx, mbox.labelID)
		if !ok {
			return fmt.Errorf("no counts kept for mailbox %v", mbox.labelID)
		}
		total, unread = int(c.total), int(c.unread)
		return nil
	})
	return total, unread, err
}

func (mbox *Mailbox) FromUid(uid uint32) (apiID string, err error) {
	err = mbox.u.db.View(func(tx *bolt.Tx) error {
		b, err := mbox.bucket(tx)
		if err != nil {
			return err
		}

		k := serializeUID(uid)
		v := b.Get(k)
		if v == nil {
			return ErrNotFound
		}
		apiID = string(v)
		return nil
	})
	return
}

func (mbox *Mailbox) FromSeqNum(seqNum uint32) (apiID string, err error) {
	err = mbox.u.db.View(func(tx *bolt.Tx) error {
		b, err := mbox.bucket(tx)
		if err != nil {
			return err
		}

		c := b.Cursor()
		var n uint32 = 1
		for k, v := c.First(); k != nil; k, v = c.Next() {
			if seqNum == n {
				apiID = string(v)
				return nil
			}
			n++
		}

		return ErrNotFound
	})
	return
}

func (mbox *Mailbox) FromApiID(apiID string) (seqNum uint32, uid uint32, err error) {
	err = mbox.u.db.View(func(tx *bolt.Tx) error {
		b, err := mbox.bucket(tx)
		if err != nil {
			return err
		}

		want := []byte(apiID)
		c := b.Cursor()
		var n uint32 = 1
		for k, v := c.First(); k != nil; k, v = c.Next() {
			if bytes.Equal(v, want) {
				seqNum = n
				uid = unserializeUID(k)
				return nil
			}
			n++
		}

		return ErrNotFound
	})
	return
}

func (mbox *Mailbox) ForEach(f func(seqNum, uid uint32, apiID string) error) error {
	return mbox.u.db.View(func(tx *bolt.Tx) error {
		b, err := mbox.bucket(tx)
		if err != nil {
			return err
		}

		c := b.Cursor()
		var n uint32 = 1
		for k, v := c.First(); k != nil; k, v = c.Next() {
			if err := f(n, unserializeUID(k), string(v)); err != nil {
				return err
			}
			n++
		}

		return nil
	})
}

func (mbox *Mailbox) Reset() error {
	return mbox.u.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(mailboxesBucket)
		if b == nil {
			return errors.New("cannot find mailboxes bucket")
		}
		k := []byte(mbox.labelID)
		if err := b.DeleteBucket(k); err != nil {
			return err
		}
		if _, err := b.CreateBucket(k); err != nil {
			return err
		}
		return putCounts(tx, mbox.labelID, mailboxCounts{})
	})
}
