package database

import (
	"cmp"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"

	"github.com/emersion/go-imap"
	bolt "go.etcd.io/bbolt"
)

// The message store keeps every message as served to clients, its RFC 822
// bytes, one file per message, with a record of what FETCH derives from them
// (BODYSTRUCTURE, ENVELOPE) in bodiesBucket. Both are sealed with AES-256-GCM
// under the account's store key (auth.StoreKey), which is never stored: meta
// keeps only a check value telling whether a key is the one that sealed them.
//
// Each version of a message gets a file of its own, which its record names:
// a reader takes the record, then its file, so it never pairs a record with
// another version's bytes. A file is written before the record naming it and
// removed after; a record is only written in the transaction that finds the
// message's metadata, and DeleteMessage removes it with the metadata. A file
// may outlive its record (a crash, a reader holding the name): Wipe removes
// those.

// bodiesBucket holds the sealed record of every stored message, by message ID.
var bodiesBucket = []byte("bodies")

// storeKeyCheckKey, in metaBucket, holds the check value of the store key
// that sealed the stored messages (storeKeyCheck).
var storeKeyCheckKey = []byte("storekeycheck")

// storeKeyCheckLabel is what the check value authenticates: knowing it tells
// nothing of the key.
const storeKeyCheckLabel = "peroxide message store key check v1"

// Additional data of the sealed files and records: a file renamed to another
// message's name, or a record moved under another ID or taken for a file,
// fails to open.
func fileAD(apiID string) []byte   { return []byte("message " + apiID) }
func recordAD(apiID string) []byte { return []byte("record " + apiID) }

func storeKeyCheck(key *[32]byte) []byte {
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte(storeKeyCheckLabel))
	return mac.Sum(nil)
}

// store is the sealed message store of a User.
type store struct {
	dir  string // holds the files, readable by their owner only
	aead cipher.AEAD
}

func newStore(dir string, key *[32]byte) (*store, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("cannot make the store cipher: %w", err)
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, fmt.Errorf("cannot make the store cipher: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("cannot make the message store %v: %w", dir, err)
	}
	return &store{dir: dir, aead: aead}, nil
}

// newFileName names a new version of apiID's file: the hex of the ID, so that
// no character of an ID Proton sends ends up in a path, and a random version.
func newFileName(apiID string) string {
	return hex.EncodeToString([]byte(apiID)) + "." + rand.Text()
}

// write seals literal into the file name, which it creates whole or not at
// all: a reader never sees a part.
func (s *store) write(name, apiID string, literal []byte) error {
	sealed := s.aead.Seal(nil, nil, literal, fileAD(apiID))
	tmp, err := os.CreateTemp(s.dir, "tmp-*")
	if err != nil {
		return fmt.Errorf("cannot store message %v: %w", apiID, err)
	}
	_, err = tmp.Write(sealed)
	if err == nil {
		err = tmp.Sync()
	}
	err = errors.Join(err, tmp.Close())
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(s.dir, name))
	}
	if err != nil {
		return errors.Join(fmt.Errorf("cannot store message %v: %w", apiID, err), os.Remove(tmp.Name()))
	}
	return nil
}

// read opens the file name of apiID. A missing file is fs.ErrNotExist.
func (s *store) read(name, apiID string) ([]byte, error) {
	sealed, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		return nil, fmt.Errorf("cannot read stored message %v: %w", apiID, err)
	}
	literal, err := s.aead.Open(nil, nil, sealed, fileAD(apiID))
	if err != nil {
		return nil, fmt.Errorf("cannot open stored message %v: %w", apiID, err)
	}
	return literal, nil
}

func (s *store) remove(name string) error {
	if name == "" {
		return nil
	}
	err := os.Remove(filepath.Join(s.dir, name))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cannot remove stored message file %v: %w", name, err)
	}
	return nil
}

// removeAll empties the directory, files left by a crash included.
func (s *store) removeAll() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("cannot list the message store: %w", err)
	}
	var errs []error
	for _, e := range entries {
		if err := s.remove(e.Name()); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("cannot empty the message store: %w", errors.Join(errs...))
	}
	return nil
}

// BodyInfo is what FETCH derives from a stored message's bytes, computed once
// when it is stored.
type BodyInfo struct {
	// BodyStructure is the extended one (FETCH BODYSTRUCTURE).
	BodyStructure *imap.BodyStructure
	Envelope      *imap.Envelope
	// Fallback is set when the message was built without decrypting all of
	// it (an armored body, a <name>.pgp attachment, PGP/MIME passed
	// through): unlocking another key may change it.
	Fallback bool
}

// BodyRecord is the record of a stored message.
type BodyRecord struct {
	// Size is the length of the stored bytes, set by PutBody: RFC822.SIZE.
	Size uint32
	BodyInfo
}

// recordOptions encode records so that they decode to what was encoded: nil
// slices and maps stay nil, which go-imap writes as NIL where an empty one
// would be an empty list, which RFC 3501's grammar has no room for (e.g. the
// body language).
func recordOptions() json.Options {
	return json.JoinOptions(json.Deterministic(true), json.FormatNilSliceAsNull(true), json.FormatNilMapAsNull(true))
}

// sealedRecord is a BodyRecord as sealed in bodiesBucket, with the name of
// the file holding its version of the message.
type sealedRecord struct {
	File string
	BodyRecord
}

// getRecord returns the record of apiID in tx, or nil if there is none.
func (u *User) getRecord(tx *bolt.Tx, apiID string) (*sealedRecord, error) {
	b := tx.Bucket(bodiesBucket)
	if b == nil {
		return nil, nil
	}
	sealed := b.Get([]byte(apiID))
	if sealed == nil {
		return nil, nil
	}
	plain, err := u.store.aead.Open(nil, nil, sealed, recordAD(apiID))
	if err != nil {
		return nil, fmt.Errorf("cannot open the record of message %v: %w", apiID, err)
	}
	rec := new(sealedRecord)
	if err := json.Unmarshal(plain, rec); err != nil {
		return nil, fmt.Errorf("cannot decode the record of message %v: %w", apiID, err)
	}
	return rec, nil
}

// PutBody stores literal, the message apiID as served, with info, replacing
// what was stored for it, takes it out of the failed messages, and returns
// its record. A message the database doesn't have (deleted meanwhile) isn't
// stored: stored is false.
func (u *User) PutBody(apiID string, literal []byte, info BodyInfo) (rec *BodyRecord, stored bool, err error) {
	if len(literal) > math.MaxUint32 {
		return nil, false, fmt.Errorf("cannot store message %v: %v bytes is more than IMAP can tell", apiID, len(literal))
	}
	sr := sealedRecord{File: newFileName(apiID), Size: uint32(len(literal)), BodyInfo: info}
	encoded, err := json.Marshal(sr, recordOptions())
	if err != nil {
		return nil, false, fmt.Errorf("cannot encode the record of message %v: %w", apiID, err)
	}
	if err := u.store.write(sr.File, apiID, literal); err != nil {
		return nil, false, err
	}
	var oldFile string
	err = u.db.Update(func(tx *bolt.Tx) error {
		messages := tx.Bucket(messagesBucket)
		if messages == nil || messages.Get([]byte(apiID)) == nil {
			return nil
		}
		// An unreadable old record is replaced all the same; its file is
		// left for Wipe.
		if old, err := u.getRecord(tx, apiID); err == nil && old != nil {
			oldFile = old.File
		}
		b, err := tx.CreateBucketIfNotExists(bodiesBucket)
		if err != nil {
			return err
		}
		if fb := tx.Bucket(failedBucket); fb != nil {
			if err := fb.Delete([]byte(apiID)); err != nil {
				return err
			}
		}
		stored = true
		return b.Put([]byte(apiID), u.store.aead.Seal(nil, nil, encoded, recordAD(apiID)))
	})
	if err != nil {
		return nil, false, errors.Join(err, u.store.remove(sr.File))
	}
	if !stored {
		return nil, false, u.store.remove(sr.File)
	}
	return &sr.BodyRecord, true, u.store.remove(oldFile)
}

// BodyRecord returns the record of the stored message apiID. ErrNotFound
// means none is stored.
func (u *User) BodyRecord(apiID string) (*BodyRecord, error) {
	var rec *sealedRecord
	err := u.db.View(func(tx *bolt.Tx) error {
		var err error
		rec, err = u.getRecord(tx, apiID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, ErrNotFound
	}
	return &rec.BodyRecord, nil
}

// Body returns the stored message apiID, its record and its bytes, which
// belong to the same version. ErrNotFound means none is stored, or its file
// was replaced while being read: then ask again. A record whose file is gone
// (removed from outside) is dropped, so that the message is stored again.
func (u *User) Body(apiID string) (*BodyRecord, []byte, error) {
	var rec *sealedRecord
	err := u.db.View(func(tx *bolt.Tx) error {
		var err error
		rec, err = u.getRecord(tx, apiID)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	if rec == nil {
		return nil, nil, ErrNotFound
	}
	literal, err := u.store.read(rec.File, apiID)
	if errors.Is(err, fs.ErrNotExist) {
		if err := u.dropLostRecord(apiID, rec.File); err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("stored message %v changed while read: %w", apiID, ErrNotFound)
	} else if err != nil {
		return nil, nil, err
	}
	return &rec.BodyRecord, literal, nil
}

// dropLostRecord deletes the record of apiID if it still names file, which is
// gone; one replaced meanwhile is kept.
func (u *User) dropLostRecord(apiID, file string) error {
	return u.db.Update(func(tx *bolt.Tx) error {
		rec, err := u.getRecord(tx, apiID)
		if err != nil || rec == nil || rec.File != file {
			return err
		}
		return tx.Bucket(bodiesBucket).Delete([]byte(apiID))
	})
}

// MissingBody is a message whose body isn't stored.
type MissingBody struct {
	ID   string
	Size int64 // Proton's, an estimate of what downloading it takes
}

// MissingBodies returns the messages whose body isn't stored, newest first
// (Proton's time, then ID), as the body download takes them.
func (u *User) MissingBodies() ([]MissingBody, error) {
	type missing struct {
		MissingBody
		time int64
	}
	var all []missing
	err := u.db.View(func(tx *bolt.Tx) error {
		messages := tx.Bucket(messagesBucket)
		if messages == nil {
			return nil
		}
		bodies := tx.Bucket(bodiesBucket)
		return messages.ForEach(func(k, _ []byte) error {
			if bodies != nil && bodies.Get(k) != nil {
				return nil
			}
			// userMessage: the metadata is encoding/json, not v2.
			msg, err := userMessage(messages, string(k))
			if err != nil {
				return fmt.Errorf("cannot read message %s: %w", k, err)
			}
			all = append(all, missing{MissingBody{msg.ID, msg.Size}, int64(msg.Time)})
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(all, func(a, b missing) int {
		return cmp.Or(cmp.Compare(b.time, a.time), cmp.Compare(a.ID, b.ID))
	})
	out := make([]MissingBody, len(all))
	for i, m := range all {
		out[i] = m.MissingBody
	}
	return out, nil
}

// DeleteBody removes what is stored of apiID, if anything; its metadata
// stays.
func (u *User) DeleteBody(apiID string) error {
	var file string
	err := u.db.Update(func(tx *bolt.Tx) error {
		var err error
		file, err = u.deleteBodyRecord(tx, apiID)
		return err
	})
	if err != nil {
		return err
	}
	return u.store.remove(file)
}

// deleteBodyRecord deletes the record of apiID in tx, and returns the file it
// named, for the caller to remove once tx is committed. An unreadable record
// is deleted too; its file is left for Wipe.
func (u *User) deleteBodyRecord(tx *bolt.Tx, apiID string) (file string, err error) {
	b := tx.Bucket(bodiesBucket)
	if b == nil {
		return "", nil
	}
	if rec, err := u.getRecord(tx, apiID); err == nil && rec != nil {
		file = rec.File
	}
	return file, b.Delete([]byte(apiID))
}
