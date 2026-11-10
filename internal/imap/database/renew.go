package database

import (
	"errors"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// Renewal is a message given new UIDs: in each mailbox it was in, the
// sequence number it had, which clients are to expunge, and the one it has
// now, the mailbox's new message count.
type Renewal struct {
	Expunged map[string]uint32 // by label ID
	Created  map[string]uint32 // by label ID
}

// RenewMessage gives the message apiID a new UID in every mailbox it is in,
// as gluon does to a message whose contents changed (an edited draft): RFC
// 3501 section 2.3.1.1 lets a UID name only one content. literal becomes the
// stored message, with info, in the same transaction, so that no reader sees
// the new bytes under an old UID. A message the database doesn't have is
// ErrNotFound.
func (u *User) RenewMessage(apiID string, literal []byte, info BodyInfo) (Renewal, error) {
	r := Renewal{Expunged: make(map[string]uint32), Created: make(map[string]uint32)}
	newFile, encoded, err := u.writeVersion(apiID, literal, info)
	if err != nil {
		return Renewal{}, err
	}
	var oldFile string
	err = u.db.Update(func(tx *bolt.Tx) error {
		messages := tx.Bucket(messagesBucket)
		if messages == nil {
			return ErrNotFound
		}
		msg, err := userMessage(messages, apiID)
		if err != nil {
			return err
		}
		if oldFile, err = u.deleteBodyRecord(tx, apiID); err != nil {
			return err
		}
		b, err := tx.CreateBucketIfNotExists(bodiesBucket)
		if err != nil {
			return err
		}
		if err := b.Put([]byte(apiID), u.store.aead.Seal(nil, nil, encoded, recordAD(apiID))); err != nil {
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
			seqNum, deleted, err := mailboxDeleteMessage(mbox, labelID, apiID)
			if err != nil {
				return err
			}
			if !deleted {
				continue
			}
			r.Expunged[labelID] = seqNum
			seqNum, created, err := mailboxCreateMessage(mbox, labelID, apiID)
			if err != nil {
				return err
			}
			if !created {
				return fmt.Errorf("message %v still in mailbox %v after its removal", apiID, labelID)
			}
			r.Created[labelID] = seqNum
		}
		return nil
	})
	if err != nil {
		return Renewal{}, errors.Join(err, u.store.remove(newFile))
	}
	return r, u.store.remove(oldFile)
}

// writeVersion writes literal as a new file of apiID, and returns its name
// and its sealed record's plaintext, for the caller to put in bodiesBucket.
func (u *User) writeVersion(apiID string, literal []byte, info BodyInfo) (string, []byte, error) {
	sr, encoded, err := newRecord(apiID, literal, info)
	if err != nil {
		return "", nil, err
	}
	if err := u.store.write(sr.File, apiID, literal); err != nil {
		return "", nil, err
	}
	return sr.File, encoded, nil
}
