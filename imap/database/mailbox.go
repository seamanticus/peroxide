package database

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"

	bolt "go.etcd.io/bbolt"
)

// A mailbox's bucket, under mailboxesBucket and named by its label ID, holds
// its UIDVALIDITY and two indexes of its messages: UID to message ID, in UID
// (so sequence number) order, and message ID to UID.
var (
	uidsBucket     = []byte("uids")
	idsBucket      = []byte("ids")
	uidValidityKey = []byte("uidvalidity")
)

// uidValidityEpoch is 2023-02-01T00:00:00Z, the epoch of gluon's
// DefaultEpochUIDValidityGenerator, in Unix seconds.
const uidValidityEpoch = 1675209600

// ErrClockBeforeEpoch is returned when a mailbox is created while the system
// clock reads a time before uidValidityEpoch, as after a dead CMOS battery
// resets it: a UIDVALIDITY derived from it could repeat one given before.
var ErrClockBeforeEpoch = errors.New("the system clock is set before 2023-02-01: set it right")

// nextUIDValidity returns the UIDVALIDITY for a new mailbox at now, after last
// was the previous one given: seconds since uidValidityEpoch, as gluon's
// EpochUIDValidityGenerator, but at least last+1, so that values never repeat
// even when the clock goes back or two mailboxes are made in one second.
func nextUIDValidity(now time.Time, last uint32) (uint32, error) {
	secs := now.Unix() - uidValidityEpoch
	if secs < 0 {
		return 0, fmt.Errorf("%w (it reads %v)", ErrClockBeforeEpoch, now.UTC().Format(time.RFC3339))
	}
	if secs > math.MaxUint32 {
		return 0, errors.New("cannot make a UIDVALIDITY: the clock is past 2159")
	}
	if last == math.MaxUint32 {
		return 0, errors.New("cannot make a UIDVALIDITY: every value has been given")
	}
	return max(uint32(secs), last+1), nil
}

func serializeUID(uid uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uid)
	return b
}

func unserializeUID(b []byte) uint32 {
	return binary.BigEndian.Uint32(b)
}

// mailboxCounts are how many entries a mailbox holds and how many of those have
// an unread message stored. Every mailbox has them, changed in the same
// transactions as its entries and messages.
type mailboxCounts struct {
	total, unread uint32
}

func getCounts(tx *bolt.Tx, labelID string) (c mailboxCounts, err error) {
	var v []byte
	if b := tx.Bucket(countsBucket); b != nil {
		v = b.Get([]byte(labelID))
	}
	if len(v) != 8 {
		return c, fmt.Errorf("no counts kept for mailbox %v", labelID)
	}
	return mailboxCounts{binary.BigEndian.Uint32(v), binary.BigEndian.Uint32(v[4:])}, nil
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

// adjustCounts adds total and unread to the counts of a mailbox.
func adjustCounts(tx *bolt.Tx, labelID string, total, unread int) error {
	c, err := getCounts(tx, labelID)
	if err != nil {
		return err
	}
	newTotal, newUnread := int64(c.total)+int64(total), int64(c.unread)+int64(unread)
	if newTotal < 0 || newUnread < 0 || newUnread > newTotal || newTotal > math.MaxUint32 {
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

// mailboxIndexes returns the indexes of the mailbox bucket b.
func mailboxIndexes(b *bolt.Bucket) (uids, ids *bolt.Bucket, err error) {
	uids, ids = b.Bucket(uidsBucket), b.Bucket(idsBucket)
	if uids == nil || ids == nil {
		return nil, nil, errors.New("mailbox bucket has no UID index")
	}
	return uids, ids, nil
}

// createMailboxBucket makes the bucket of mailbox labelID in mailboxes, with
// a new UIDVALIDITY and counts of zero.
func createMailboxBucket(tx *bolt.Tx, mailboxes *bolt.Bucket, labelID string) error {
	meta, err := tx.CreateBucketIfNotExists(metaBucket)
	if err != nil {
		return err
	}
	var last uint32
	if v := meta.Get(lastUIDValidityKey); len(v) == 4 {
		last = unserializeUID(v)
	}
	uidValidity, err := nextUIDValidity(time.Now(), last)
	if err != nil {
		return fmt.Errorf("cannot create mailbox %v: %w", labelID, err)
	}
	if err := meta.Put(lastUIDValidityKey, serializeUID(uidValidity)); err != nil {
		return err
	}

	b, err := mailboxes.CreateBucket([]byte(labelID))
	if err != nil {
		return err
	}
	if _, err := b.CreateBucket(uidsBucket); err != nil {
		return err
	}
	if _, err := b.CreateBucket(idsBucket); err != nil {
		return err
	}
	if err := b.Put(uidValidityKey, serializeUID(uidValidity)); err != nil {
		return err
	}
	return putCounts(tx, labelID, mailboxCounts{})
}

func mailboxContains(b *bolt.Bucket, apiID string) (bool, error) {
	_, ids, err := mailboxIndexes(b)
	if err != nil {
		return false, err
	}
	return ids.Get([]byte(apiID)) != nil, nil
}

// mailboxCreateMessage adds apiID to the mailbox with the next UID, unless it's
// there already, and returns its sequence number. The highest UID is the last
// message, so that is the new message count.
func mailboxCreateMessage(b *bolt.Bucket, labelID, apiID string) (seqNum uint32, created bool, err error) {
	uids, ids, err := mailboxIndexes(b)
	if err != nil {
		return 0, false, err
	}
	k := []byte(apiID)
	if ids.Get(k) != nil {
		return 0, false, nil
	}

	next, err := uids.NextSequence()
	if err != nil {
		return 0, false, err
	}
	if next > math.MaxUint32 {
		return 0, false, fmt.Errorf("no UID left in mailbox %v", labelID)
	}
	uid := serializeUID(uint32(next))
	if err := uids.Put(uid, k); err != nil {
		return 0, false, err
	}
	if err := ids.Put(k, uid); err != nil {
		return 0, false, err
	}

	unread, err := messageUnread(b.Tx().Bucket(messagesBucket), apiID)
	if err != nil {
		return 0, false, err
	}
	if err := adjustCounts(b.Tx(), labelID, 1, unreadCount(unread)); err != nil {
		return 0, false, err
	}
	c, err := getCounts(b.Tx(), labelID)
	return c.total, true, err
}

// seqNumOf returns the sequence number of uid in uids, 0 if it isn't there.
func seqNumOf(uids *bolt.Bucket, uid uint32) uint32 {
	var n uint32 = 1
	c := uids.Cursor()
	for k, _ := c.First(); k != nil; k, _ = c.Next() {
		if unserializeUID(k) == uid {
			return n
		}
		n++
	}
	return 0
}

// mailboxDeleteMessage removes apiID from the mailbox, and returns the sequence
// number it had, or 0 if it wasn't there.
func mailboxDeleteMessage(b *bolt.Bucket, labelID, apiID string) (seqNum uint32, err error) {
	uids, ids, err := mailboxIndexes(b)
	if err != nil {
		return 0, err
	}
	k := []byte(apiID)
	uid := ids.Get(k)
	if uid == nil {
		return 0, nil
	}
	seqNum = seqNumOf(uids, unserializeUID(uid))
	if seqNum == 0 {
		return 0, fmt.Errorf("message %v has UID %v in mailbox %v, which lists no such UID", apiID, unserializeUID(uid), labelID)
	}

	unread, err := messageUnread(b.Tx().Bucket(messagesBucket), apiID)
	if err != nil {
		return 0, err
	}
	if err := uids.Delete(uid); err != nil {
		return 0, err
	}
	if err := ids.Delete(k); err != nil {
		return 0, err
	}
	return seqNum, adjustCounts(b.Tx(), labelID, -1, -unreadCount(unread))
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
		return nil, fmt.Errorf("cannot find bucket of mailbox %v", mbox.labelID)
	}
	return b, nil
}

func (mbox *Mailbox) indexes(tx *bolt.Tx) (uids, ids *bolt.Bucket, err error) {
	b, err := mbox.bucket(tx)
	if err != nil {
		return nil, nil, err
	}
	uids, ids, err = mailboxIndexes(b)
	if err != nil {
		return nil, nil, fmt.Errorf("mailbox %v: %w", mbox.labelID, err)
	}
	return uids, ids, nil
}

// UidNext returns the UID the next message added will get.
func (mbox *Mailbox) UidNext() (uint32, error) {
	var uid uint32
	err := mbox.u.db.View(func(tx *bolt.Tx) error {
		uids, _, err := mbox.indexes(tx)
		if err != nil {
			return err
		}
		seq := uids.Sequence()
		if seq >= math.MaxUint32 {
			return fmt.Errorf("no UID left in mailbox %v", mbox.labelID)
		}
		uid = uint32(seq + 1)
		return nil
	})
	return uid, err
}

// UidValidity returns the mailbox's UIDVALIDITY, given when it was created.
func (mbox *Mailbox) UidValidity() (uint32, error) {
	var uidValidity uint32
	err := mbox.u.db.View(func(tx *bolt.Tx) error {
		b, err := mbox.bucket(tx)
		if err != nil {
			return err
		}
		v := b.Get(uidValidityKey)
		if len(v) != 4 {
			return fmt.Errorf("mailbox %v has no UIDVALIDITY", mbox.labelID)
		}
		uidValidity = unserializeUID(v)
		return nil
	})
	return uidValidity, err
}

// Count returns the number of messages in the mailbox, which is also its
// highest sequence number, and how many of them are unread.
func (mbox *Mailbox) Count() (total, unread int, err error) {
	err = mbox.u.db.View(func(tx *bolt.Tx) error {
		c, err := getCounts(tx, mbox.labelID)
		total, unread = int(c.total), int(c.unread)
		return err
	})
	return total, unread, err
}

func (mbox *Mailbox) FromUid(uid uint32) (apiID string, err error) {
	err = mbox.u.db.View(func(tx *bolt.Tx) error {
		uids, _, err := mbox.indexes(tx)
		if err != nil {
			return err
		}
		v := uids.Get(serializeUID(uid))
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
		uids, _, err := mbox.indexes(tx)
		if err != nil {
			return err
		}

		c := uids.Cursor()
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
		uids, ids, err := mbox.indexes(tx)
		if err != nil {
			return err
		}
		v := ids.Get([]byte(apiID))
		if v == nil {
			return ErrNotFound
		}
		uid = unserializeUID(v)
		if seqNum = seqNumOf(uids, uid); seqNum == 0 {
			return fmt.Errorf("message %v has UID %v in mailbox %v, which lists no such UID", apiID, uid, mbox.labelID)
		}
		return nil
	})
	return
}

func (mbox *Mailbox) ForEach(f func(seqNum, uid uint32, apiID string) error) error {
	return mbox.u.db.View(func(tx *bolt.Tx) error {
		uids, _, err := mbox.indexes(tx)
		if err != nil {
			return err
		}

		c := uids.Cursor()
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

// Delete removes the mailbox's entries and counts. The messages stay, as other
// mailboxes may hold them.
func (mbox *Mailbox) Delete() error {
	return mbox.u.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(mailboxesBucket)
		if b == nil {
			return errors.New("cannot find mailboxes bucket")
		}
		if err := b.DeleteBucket([]byte(mbox.labelID)); err != nil {
			return fmt.Errorf("cannot delete mailbox %v: %w", mbox.labelID, err)
		}
		if counts := tx.Bucket(countsBucket); counts != nil {
			return counts.Delete([]byte(mbox.labelID))
		}
		return nil
	})
}
