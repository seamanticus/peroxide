package database

import (
	"encoding/json"
	"errors"
	"fmt"

	bolt "go.etcd.io/bbolt"

	"github.com/seamanticus/peroxide/config"
	"github.com/seamanticus/peroxide/protonmail"
)

var ErrNotFound = errors.New("message not found in local database")

var (
	mailboxesBucket = []byte("mailboxes")
	messagesBucket  = []byte("messages")
	countsBucket    = []byte("counts")
)

func userMessage(b *bolt.Bucket, apiID string) (*protonmail.Message, error) {
	k := []byte(apiID)
	v := b.Get(k)
	if v == nil {
		return nil, ErrNotFound
	}

	msg := &protonmail.Message{}
	err := json.Unmarshal(v, msg)
	return msg, err
}

// messageUnread reports whether the stored message apiID is unread. A message
// missing from the local database counts as read.
func messageUnread(b *bolt.Bucket, apiID string) (bool, error) {
	if b == nil {
		return false, nil
	}
	msg, err := userMessage(b, apiID)
	if err == ErrNotFound {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("message %v: %w", apiID, err)
	}
	return msg.Unread == 1, nil
}

// countedMailboxes returns the IDs of the mailboxes whose counts are kept.
func countedMailboxes(tx *bolt.Tx) ([]string, error) {
	counts := tx.Bucket(countsBucket)
	if counts == nil {
		return nil, nil
	}
	var labelIDs []string
	err := counts.ForEach(func(k, _ []byte) error {
		labelIDs = append(labelIDs, string(k))
		return nil
	})
	return labelIDs, err
}

// adjustUnread updates the unread counts of every mailbox holding apiID after
// the stored message went from wasUnread to unread. Every counted mailbox is
// checked, as one may still hold a message whose labels no longer list it.
func adjustUnread(tx *bolt.Tx, apiID string, wasUnread, unread bool) error {
	if wasUnread == unread {
		return nil
	}
	mailboxes := tx.Bucket(mailboxesBucket)
	if mailboxes == nil {
		return nil
	}
	labelIDs, err := countedMailboxes(tx)
	if err != nil {
		return err
	}
	for _, labelID := range labelIDs {
		b := mailboxes.Bucket([]byte(labelID))
		if b == nil || !mailboxContains(b, apiID) {
			continue
		}
		if err := adjustCounts(tx, labelID, 0, unreadCount(unread)-unreadCount(wasUnread)); err != nil {
			return err
		}
	}
	return nil
}

func userCreateMessage(b *bolt.Bucket, msg *protonmail.Message) error {
	wasUnread, err := messageUnread(b, msg.ID)
	if err != nil {
		return err
	}

	k := []byte(msg.ID)
	v, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if err := b.Put(k, v); err != nil {
		return err
	}
	return adjustUnread(b.Tx(), msg.ID, wasUnread, msg.Unread == 1)
}

func userDeleteMessage(b *bolt.Bucket, msg *protonmail.Message) error {
	if err := b.Delete([]byte(msg.ID)); err != nil {
		return err
	}
	return adjustUnread(b.Tx(), msg.ID, msg.Unread == 1, false)
}

func userSync(tx *bolt.Tx, messages []*protonmail.Message) error {
	b, err := tx.CreateBucketIfNotExists(messagesBucket)
	if err != nil {
		return err
	}

	for _, msg := range messages {
		if err := userCreateMessage(b, msg); err != nil {
			return err
		}
	}

	return nil
}

type User struct {
	db *bolt.DB
}

func (u *User) Mailbox(labelID string) (*Mailbox, error) {
	err := u.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(mailboxesBucket)
		if err != nil {
			return err
		}
		_, err = b.CreateBucketIfNotExists([]byte(labelID))
		return err
	})
	if err != nil {
		return nil, err
	}

	return &Mailbox{labelID, u}, nil
}

func (u *User) Message(apiID string) (*protonmail.Message, error) {
	var msg *protonmail.Message
	err := u.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(messagesBucket)
		if b == nil {
			return ErrNotFound
		}

		var err error
		msg, err = userMessage(b, apiID)
		return err
	})
	return msg, err
}

func (u *User) ResetMessages() error {
	return u.db.Update(func(tx *bolt.Tx) error {
		if err := tx.DeleteBucket(messagesBucket); err != nil {
			return err
		}

		// Entries left without their message count as read.
		labelIDs, err := countedMailboxes(tx)
		if err != nil {
			return err
		}
		for _, labelID := range labelIDs {
			c, _ := getCounts(tx, labelID)
			c.unread = 0
			if err := putCounts(tx, labelID, c); err != nil {
				return err
			}
		}
		return nil
	})
}

func (u *User) CreateMessage(msg *protonmail.Message) (seqNums map[string]uint32, err error) {
	seqNums = make(map[string]uint32)
	err = u.db.Update(func(tx *bolt.Tx) error {
		messages, err := tx.CreateBucketIfNotExists(messagesBucket)
		if err != nil {
			return err
		}

		if err := userCreateMessage(messages, msg); err != nil {
			return err
		}

		mailboxes, err := tx.CreateBucketIfNotExists(mailboxesBucket)
		if err != nil {
			return err
		}
		for _, labelID := range msg.LabelIDs {
			mbox, err := mailboxes.CreateBucketIfNotExists([]byte(labelID))
			if err != nil {
				return err
			}

			seqNum, err := mailboxCreateMessage(mbox, labelID, msg.ID)
			if err != nil {
				return err
			}
			seqNums[labelID] = seqNum
		}

		return nil
	})
	return
}

func (u *User) UpdateMessage(apiID string, update *protonmail.EventMessageUpdate) (createdSeqNums map[string]uint32, deletedSeqNums map[string]uint32, err error) {
	createdSeqNums = make(map[string]uint32)
	deletedSeqNums = make(map[string]uint32)
	err = u.db.Update(func(tx *bolt.Tx) error {
		messages := tx.Bucket(messagesBucket)
		if messages == nil {
			return errors.New("cannot update message in local DB: messages bucket doesn't exist")
		}

		msg, err := userMessage(messages, apiID)
		if err != nil {
			return err
		}

		addedLabels, removedLabels := update.DiffLabelIDs(msg.LabelIDs)

		mailboxes, err := tx.CreateBucketIfNotExists(mailboxesBucket)
		if err != nil {
			return err
		}
		for _, labelID := range addedLabels {
			mbox, err := mailboxes.CreateBucketIfNotExists([]byte(labelID))
			if err != nil {
				return err
			}

			seqNum, err := mailboxCreateMessage(mbox, labelID, apiID)
			if err != nil {
				return err
			}
			createdSeqNums[labelID] = seqNum
		}
		for _, labelID := range removedLabels {
			mbox := mailboxes.Bucket([]byte(labelID))
			if mbox == nil {
				continue
			}

			seqNum, err := mailboxDeleteMessage(mbox, labelID, apiID)
			if err != nil {
				return err
			}
			deletedSeqNums[labelID] = seqNum
		}

		update.Patch(msg)
		return userCreateMessage(messages, msg)
	})
	return
}

func (u *User) DeleteMessage(apiID string) (seqNums map[string]uint32, err error) {
	seqNums = make(map[string]uint32)
	err = u.db.Update(func(tx *bolt.Tx) error {
		messages := tx.Bucket(messagesBucket)
		if messages == nil {
			return nil
		}

		msg, err := userMessage(messages, apiID)
		if err == ErrNotFound {
			return nil
		} else if err != nil {
			return err
		}

		if err := userDeleteMessage(messages, msg); err != nil {
			return err
		}

		mailboxes := tx.Bucket(mailboxesBucket)
		if mailboxes == nil {
			return nil
		}
		for _, labelID := range msg.LabelIDs {
			mbox := mailboxes.Bucket([]byte(labelID))
			if mbox == nil {
				continue
			}

			seqNum, err := mailboxDeleteMessage(mbox, labelID, msg.ID)
			if err != nil {
				return err
			}
			seqNums[labelID] = seqNum
		}

		return nil
	})
	return
}

func (u *User) Close() error {
	return u.db.Close()
}

func Open(filename string) (*User, error) {
	p, err := config.Path(filename)
	if err != nil {
		return nil, err
	}

	db, err := bolt.Open(p, 0700, nil)
	if err != nil {
		return nil, err
	}

	// Counts left by a previous run may have been made stale by an older
	// peroxide; mailboxes rebuild theirs when synchronized.
	err = db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket(countsBucket) == nil {
			return nil
		}
		return tx.DeleteBucket(countsBucket)
	})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("cannot drop message counts: %w", err), db.Close())
	}

	return &User{db}, nil
}
