package database

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"

	"github.com/emersion/go-imap"
	bolt "go.etcd.io/bbolt"
)

// The message store keeps every message as served to clients, its RFC 822
// bytes, one file per message, with a record of what FETCH derives from them
// (BODYSTRUCTURE, ENVELOPE) in bodiesBucket. Both are sealed with AES-256-GCM
// under the account's store key (auth.StoreKey), which is never stored: meta
// keeps only a check value telling whether a key is the one that sealed them.
//
// A file is written before its record and removed after it, so a record
// always has its file but for a file removed from outside; a crash may leave
// a file without a record, which the next put overwrites and Wipe removes.

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

// path names the file of apiID by the hex of the ID, so that no character of
// an ID Proton sends ends up in a path.
func (s *store) path(apiID string) string {
	return filepath.Join(s.dir, hex.EncodeToString([]byte(apiID)))
}

// write seals literal into the file of apiID, replacing it atomically: a
// reader sees the old file or the new one, never a part.
func (s *store) write(apiID string, literal []byte) error {
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
		err = os.Rename(tmp.Name(), s.path(apiID))
	}
	if err != nil {
		return errors.Join(fmt.Errorf("cannot store message %v: %w", apiID, err), os.Remove(tmp.Name()))
	}
	return nil
}

func (s *store) read(apiID string) ([]byte, error) {
	sealed, err := os.ReadFile(s.path(apiID))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, fmt.Errorf("cannot read stored message %v: %w", apiID, err)
	}
	literal, err := s.aead.Open(nil, nil, sealed, fileAD(apiID))
	if err != nil {
		return nil, fmt.Errorf("cannot open stored message %v: %w", apiID, err)
	}
	return literal, nil
}

func (s *store) remove(apiID string) error {
	err := os.Remove(s.path(apiID))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cannot remove stored message %v: %w", apiID, err)
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
		if err := os.Remove(filepath.Join(s.dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
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

// PutBody stores literal, the message apiID as served, with info, replacing
// what was stored for it.
func (u *User) PutBody(apiID string, literal []byte, info BodyInfo) error {
	if len(literal) > math.MaxUint32 {
		return fmt.Errorf("cannot store message %v: %v bytes is more than IMAP can tell", apiID, len(literal))
	}
	rec, err := json.Marshal(BodyRecord{Size: uint32(len(literal)), BodyInfo: info}, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("cannot encode the record of message %v: %w", apiID, err)
	}
	if err := u.store.write(apiID, literal); err != nil {
		return err
	}
	return u.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bodiesBucket)
		if err != nil {
			return err
		}
		return b.Put([]byte(apiID), u.store.aead.Seal(nil, nil, rec, recordAD(apiID)))
	})
}

// Body returns the stored bytes of apiID. ErrNotFound means none are stored.
func (u *User) Body(apiID string) ([]byte, error) {
	if _, err := u.BodyRecord(apiID); err != nil {
		return nil, err
	}
	return u.store.read(apiID)
}

// BodyRecord returns the record of the stored message apiID. ErrNotFound
// means none is stored.
func (u *User) BodyRecord(apiID string) (*BodyRecord, error) {
	var sealed []byte
	err := u.db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket(bodiesBucket); b != nil {
			// Only valid during the transaction: copied out.
			if v := b.Get([]byte(apiID)); v != nil {
				sealed = append([]byte(nil), v...)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if sealed == nil {
		return nil, ErrNotFound
	}
	plain, err := u.store.aead.Open(nil, nil, sealed, recordAD(apiID))
	if err != nil {
		return nil, fmt.Errorf("cannot open the record of message %v: %w", apiID, err)
	}
	rec := new(BodyRecord)
	if err := json.Unmarshal(plain, rec); err != nil {
		return nil, fmt.Errorf("cannot decode the record of message %v: %w", apiID, err)
	}
	return rec, nil
}

// DeleteBody removes what is stored of apiID, if anything.
func (u *User) DeleteBody(apiID string) error {
	err := u.db.Update(func(tx *bolt.Tx) error {
		if b := tx.Bucket(bodiesBucket); b != nil {
			return b.Delete([]byte(apiID))
		}
		return nil
	})
	if err != nil {
		return err
	}
	return u.store.remove(apiID)
}
