package database

import (
	"cmp"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	bolt "go.etcd.io/bbolt"
	boltErrors "go.etcd.io/bbolt/errors"

	"github.com/seamanticus/peroxide/config"
	"github.com/seamanticus/peroxide/protonmail"
)

var ErrNotFound = errors.New("message not found in local database")

// ErrInUse is returned by Open when another process holds the database: bolt
// lets one process open a file at a time.
var ErrInUse = errors.New("the database is in use by another peroxide process")

var (
	mailboxesBucket = []byte("mailboxes")
	messagesBucket  = []byte("messages")
	countsBucket    = []byte("counts")
	eventsBucket    = []byte("events")
	// metaBucket holds what outlives a wipe (the schema version, the last
	// UIDVALIDITY given) and the state of the account's download.
	metaBucket = []byte("meta")
)

// Keys of metaBucket.
var (
	versionKey         = []byte("version")
	lastUIDValidityKey = []byte("lastuidvalidity")
	// syncCursorKey is the ID of the oldest message the download has stored:
	// the next page asks for the messages before it (bridge's
	// LastSyncedMessageID).
	syncCursorKey = []byte("synccursor")
	// syncDoneKey is set once every message has been downloaded (bridge's
	// HasMessages).
	syncDoneKey = []byte("syncdone")
	// syncStartEventKey is the last event ID applied when the download began
	// (bridge's StartSyncEventID): once done, the events after it are
	// applied again, to correct pages fetched before an event and stored
	// after it.
	syncStartEventKey = []byte("syncstartevent")
	// rebuildKey asks the next login to wipe the database and download the
	// account again (bridge's ShouldResync).
	rebuildKey = []byte("rebuild")
)

// schemaVersion is the layout of the database. Open wipes a database of any
// other version: it is a copy of what Proton has, rebuilt by downloading it.
const schemaVersion uint32 = 2

// lastEventIDKey holds, in eventsBucket, the ID of the last event applied.
var lastEventIDKey = []byte("last")

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
	if errors.Is(err, ErrNotFound) {
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
		if b == nil {
			continue
		}
		contains, err := mailboxContains(b, apiID)
		if err != nil {
			return fmt.Errorf("mailbox %v: %w", labelID, err)
		}
		if !contains {
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

// MessageChange tells, per mailbox (by label ID), what storing a message did:
// Removed holds the sequence number it had in each mailbox it left, Added the
// sequence number it got in each mailbox it entered.
type MessageChange struct {
	Removed map[string]uint32
	Added   map[string]uint32
}

// putMessage stores msg, as Proton has it now, and files it in the mailboxes
// of its labels that have a bucket. A message stored before leaves the
// mailboxes its labels no longer list: removed first, while the stored message
// still says whether it was unread, then the new one counted where it stays.
func putMessage(tx *bolt.Tx, msg *protonmail.Message) (MessageChange, error) {
	change := MessageChange{Removed: make(map[string]uint32), Added: make(map[string]uint32)}
	messages, err := tx.CreateBucketIfNotExists(messagesBucket)
	if err != nil {
		return change, err
	}
	mailboxes, err := tx.CreateBucketIfNotExists(mailboxesBucket)
	if err != nil {
		return change, err
	}

	old, err := userMessage(messages, msg.ID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return change, err
	}
	if old != nil {
		for _, labelID := range old.LabelIDs {
			if slices.Contains(msg.LabelIDs, labelID) {
				continue
			}
			mbox := mailboxes.Bucket([]byte(labelID))
			if mbox == nil {
				continue
			}
			seqNum, deleted, err := mailboxDeleteMessage(mbox, labelID, msg.ID)
			if err != nil {
				return change, err
			}
			if deleted {
				change.Removed[labelID] = seqNum
			}
		}
	}

	if err := userCreateMessage(messages, msg); err != nil {
		return change, err
	}

	for _, labelID := range msg.LabelIDs {
		// Labels peroxide shows no mailbox for (categories, custom labels,
		// which are flags) have no bucket.
		mbox := mailboxes.Bucket([]byte(labelID))
		if mbox == nil {
			continue
		}
		seqNum, created, err := mailboxCreateMessage(mbox, labelID, msg.ID)
		if err != nil {
			return change, err
		}
		if created {
			change.Added[labelID] = seqNum
		}
	}
	return change, nil
}

type User struct {
	db    *bolt.DB
	store *store
}

// Mailbox returns the mailbox of labelID, creating it, with a new
// UIDVALIDITY, if the database has none. A mailbox created after messages
// were stored (a folder that appears later) gets those of them it holds,
// newest first as the download files them.
func (u *User) Mailbox(labelID string) (*Mailbox, error) {
	err := u.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(mailboxesBucket)
		if err != nil {
			return err
		}
		if b.Bucket([]byte(labelID)) != nil {
			return nil
		}
		if err := createMailboxBucket(tx, b, labelID); err != nil {
			return err
		}
		return fillMailbox(tx, b.Bucket([]byte(labelID)), labelID)
	})
	if err != nil {
		return nil, err
	}

	return &Mailbox{labelID, u}, nil
}

// fillMailbox files in the empty mailbox mbox the stored messages labelled
// labelID, by descending Order (Proton's arrival order), then ID.
func fillMailbox(tx *bolt.Tx, mbox *bolt.Bucket, labelID string) error {
	messages := tx.Bucket(messagesBucket)
	if messages == nil {
		return nil
	}
	var held []*protonmail.Message
	err := messages.ForEach(func(k, v []byte) error {
		msg := &protonmail.Message{}
		if err := json.Unmarshal(v, msg); err != nil {
			return fmt.Errorf("message %s: %w", k, err)
		}
		if slices.Contains(msg.LabelIDs, labelID) {
			held = append(held, msg)
		}
		return nil
	})
	if err != nil {
		return err
	}
	slices.SortFunc(held, func(a, b *protonmail.Message) int {
		return cmp.Or(cmp.Compare(b.Order, a.Order), cmp.Compare(a.ID, b.ID))
	})
	for _, msg := range held {
		if _, _, err := mailboxCreateMessage(mbox, labelID, msg.ID); err != nil {
			return err
		}
	}
	return nil
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

// CreateMessage stores msg, as a create event gives it, and files it in the
// mailboxes of its labels. A message already stored (listed by the download
// before its event came) keeps its UIDs.
func (u *User) CreateMessage(msg *protonmail.Message) (change MessageChange, err error) {
	err = u.db.Update(func(tx *bolt.Tx) error {
		change, err = putMessage(tx, msg)
		return err
	})
	return change, err
}

// SyncPage stores a page of the download, newest message first, and records
// the last (oldest) as where the next page starts: both in one transaction, so
// a download stopped at any point resumes without a gap. It returns what each
// message changed, in page order.
func (u *User) SyncPage(msgs []*protonmail.Message) (changes []MessageChange, err error) {
	if len(msgs) == 0 {
		return nil, errors.New("cannot store an empty download page")
	}
	err = u.db.Update(func(tx *bolt.Tx) error {
		changes = changes[:0]
		for _, msg := range msgs {
			change, err := putMessage(tx, msg)
			if err != nil {
				return fmt.Errorf("message %v: %w", msg.ID, err)
			}
			changes = append(changes, change)
		}
		meta, err := tx.CreateBucketIfNotExists(metaBucket)
		if err != nil {
			return err
		}
		return meta.Put(syncCursorKey, []byte(msgs[len(msgs)-1].ID))
	})
	return changes, err
}

// SyncState is where the download stands.
type SyncState struct {
	// Cursor is the ID of the oldest message stored, if Started.
	Cursor  string
	Started bool // a page was stored
	Done    bool // every message was stored
}

// SyncState returns where the download stands.
func (u *User) SyncState() (SyncState, error) {
	var s SyncState
	err := u.db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket(metaBucket)
		if meta == nil {
			return nil
		}
		if v := meta.Get(syncCursorKey); v != nil {
			s.Cursor, s.Started = string(v), true
		}
		s.Done = meta.Get(syncDoneKey) != nil
		return nil
	})
	return s, err
}

// SetSyncDone records that every message has been downloaded, and returns
// the event ID the download began at (SetSyncStart), forgotten with it, for
// the events after it to be applied again. Empty if none was recorded. The
// same transaction stores it as the last event applied, so a restart before
// the replay ends replays still.
func (u *User) SetSyncDone() (startEventID string, err error) {
	err = u.db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucketIfNotExists(metaBucket)
		if err != nil {
			return err
		}
		startEventID = string(meta.Get(syncStartEventKey))
		if err := meta.Delete(syncStartEventKey); err != nil {
			return err
		}
		if startEventID != "" {
			events, err := tx.CreateBucketIfNotExists(eventsBucket)
			if err != nil {
				return err
			}
			if err := events.Put(lastEventIDKey, []byte(startEventID)); err != nil {
				return err
			}
		}
		return meta.Put(syncDoneKey, []byte{1})
	})
	return startEventID, err
}

// SetSyncStart records eventID as the event ID the download began at, unless
// one is recorded already: a download resumed keeps the first.
func (u *User) SetSyncStart(eventID string) error {
	return u.db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucketIfNotExists(metaBucket)
		if err != nil {
			return err
		}
		if meta.Get(syncStartEventKey) != nil {
			return nil
		}
		return meta.Put(syncStartEventKey, []byte(eventID))
	})
}

// SetNeedsRebuild asks the next login to wipe the database and download the
// account again, as proton-bridge's ShouldResync. Wipe clears it: a rebuild
// stopped halfway resumes its download, as any other (SyncState), rather than
// wiping again, so a client that reconnects doesn't restart it.
func (u *User) SetNeedsRebuild() error {
	return u.putMeta(rebuildKey, []byte{1})
}

// NeedsRebuild reports whether SetNeedsRebuild was called since the last Wipe.
func (u *User) NeedsRebuild() (bool, error) {
	var rebuild bool
	err := u.db.View(func(tx *bolt.Tx) error {
		if meta := tx.Bucket(metaBucket); meta != nil {
			rebuild = meta.Get(rebuildKey) != nil
		}
		return nil
	})
	return rebuild, err
}

func (u *User) putMeta(k, v []byte) error {
	return u.db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucketIfNotExists(metaBucket)
		if err != nil {
			return err
		}
		return meta.Put(k, v)
	})
}

// Wipe empties the database: messages, mailboxes, counts, the last event ID,
// the download's state, a rebuild asked for and the stored messages go, so
// the next login starts as the first did. Kept: the schema version, the last
// UIDVALIDITY given, so that mailboxes made again get higher ones, and the
// store key's check value, as the key is the same.
func (u *User) Wipe() error {
	if err := u.db.Update(wipe); err != nil {
		return err
	}
	// After the records are gone, as a file without a record is unused.
	return u.store.removeAll()
}

// wipe empties the database in tx. The caller removes the stored files once
// tx is committed.
func wipe(tx *bolt.Tx) error {
	for _, name := range [][]byte{messagesBucket, mailboxesBucket, countsBucket, eventsBucket, bodiesBucket, failedBucket} {
		if tx.Bucket(name) == nil {
			continue
		}
		if err := tx.DeleteBucket(name); err != nil {
			return fmt.Errorf("cannot delete bucket %s: %w", name, err)
		}
	}
	meta, err := tx.CreateBucketIfNotExists(metaBucket)
	if err != nil {
		return err
	}
	for _, k := range [][]byte{syncCursorKey, syncDoneKey, syncStartEventKey, rebuildKey} {
		if err := meta.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

// UpdateMessage applies update to the stored message apiID. ErrNotFound means
// the database doesn't have it.
func (u *User) UpdateMessage(apiID string, update *protonmail.EventMessageUpdate) (createdSeqNums map[string]uint32, deletedSeqNums map[string]uint32, err error) {
	createdSeqNums = make(map[string]uint32)
	deletedSeqNums = make(map[string]uint32)
	err = u.db.Update(func(tx *bolt.Tx) error {
		messages := tx.Bucket(messagesBucket)
		if messages == nil {
			return ErrNotFound
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
			mbox := mailboxes.Bucket([]byte(labelID))
			if mbox == nil {
				continue
			}

			seqNum, created, err := mailboxCreateMessage(mbox, labelID, apiID)
			if err != nil {
				return err
			}
			if created {
				createdSeqNums[labelID] = seqNum
			}
		}
		for _, labelID := range removedLabels {
			mbox := mailboxes.Bucket([]byte(labelID))
			if mbox == nil {
				continue
			}

			seqNum, deleted, err := mailboxDeleteMessage(mbox, labelID, apiID)
			if err != nil {
				return err
			}
			if deleted {
				deletedSeqNums[labelID] = seqNum
			}
		}

		update.Patch(msg)
		return userCreateMessage(messages, msg)
	})
	return
}

func (u *User) DeleteMessage(apiID string) (seqNums map[string]uint32, err error) {
	seqNums = make(map[string]uint32)
	var file string
	err = u.db.Update(func(tx *bolt.Tx) error {
		var err error
		if file, err = u.deleteBodyRecord(tx, apiID); err != nil {
			return err
		}
		if b := tx.Bucket(failedBucket); b != nil {
			if err := b.Delete([]byte(apiID)); err != nil {
				return err
			}
		}
		messages := tx.Bucket(messagesBucket)
		if messages == nil {
			return nil
		}

		msg, err := userMessage(messages, apiID)
		if errors.Is(err, ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}

		mailboxes := tx.Bucket(mailboxesBucket)
		if mailboxes != nil {
			for _, labelID := range msg.LabelIDs {
				mbox := mailboxes.Bucket([]byte(labelID))
				if mbox == nil {
					continue
				}

				seqNum, deleted, err := mailboxDeleteMessage(mbox, labelID, msg.ID)
				if err != nil {
					return err
				}
				if deleted {
					seqNums[labelID] = seqNum
				}
			}
		}

		// After the entries: their counts read the stored message.
		return userDeleteMessage(messages, msg)
	})
	if err != nil {
		return nil, err
	}
	// After the record, as a file without a record is unused.
	return seqNums, u.store.remove(file)
}

func (u *User) Close() error {
	return u.db.Close()
}

// LastEventID returns the ID of the last Proton event applied to the database,
// and whether one was.
func (u *User) LastEventID() (id string, ok bool, err error) {
	err = u.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(eventsBucket)
		if b == nil {
			return nil
		}
		if v := b.Get(lastEventIDKey); v != nil {
			id, ok = string(v), true
		}
		return nil
	})
	return id, ok, err
}

// SetLastEventID records id as the last Proton event applied: the next session
// resumes from the event after it.
func (u *User) SetLastEventID(id string) error {
	return u.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(eventsBucket)
		if err != nil {
			return err
		}
		return b.Put(lastEventIDKey, []byte(id))
	})
}

// openTimeout is how long Open waits for another process to let go of the
// database before it gives up with ErrInUse.
const openTimeout = time.Second

// Open opens the database of the account name, <name>.db in peroxide's
// configuration directory, with its message store in the data directory,
// sealed under key. A database of another schema version, as left by an
// older peroxide, is wiped; so is one whose messages were stored under
// another key (a new or lost bridge password), which keyChanged reports: the
// store is rebuilt by downloading it again, as after a Refresh.
func Open(name string, key [32]byte) (u *User, keyChanged bool, err error) {
	p, err := config.Path(name + ".db")
	if err != nil {
		return nil, false, err
	}
	dir, err := config.DataPath(name)
	if err != nil {
		return nil, false, err
	}
	s, err := newStore(dir, &key)
	if err != nil {
		return nil, false, err
	}

	db, err := bolt.Open(p, 0700, &bolt.Options{Timeout: openTimeout})
	if errors.Is(err, boltErrors.ErrTimeout) {
		return nil, false, fmt.Errorf("cannot open %v: %w", p, ErrInUse)
	} else if err != nil {
		return nil, false, fmt.Errorf("cannot open %v: %w", p, err)
	}

	check := storeKeyCheck(&key)
	var wiped bool
	err = db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucketIfNotExists(metaBucket)
		if err != nil {
			return err
		}
		if v := meta.Get(versionKey); len(v) != 4 || unserializeUID(v) != schemaVersion {
			if err := wipeAll(tx, meta); err != nil {
				return err
			}
			wiped = true
		}
		if old := meta.Get(storeKeyCheckKey); old != nil && !hmac.Equal(old, check) {
			if err := wipe(tx); err != nil {
				return err
			}
			wiped, keyChanged = true, true
		}
		return meta.Put(storeKeyCheckKey, check)
	})
	if err == nil && wiped {
		err = s.removeAll()
	}
	if err != nil {
		return nil, false, errors.Join(fmt.Errorf("cannot open %v: %w", p, err), db.Close())
	}

	return &User{db: db, store: s}, keyChanged, nil
}

// wipeAll empties a database of another schema version: every bucket but
// meta, those of older versions too, then stamps the version.
func wipeAll(tx *bolt.Tx, meta *bolt.Bucket) error {
	// Collected first, as bolt forbids deleting buckets while iterating
	// over them.
	var names [][]byte
	if err := tx.ForEach(func(name []byte, _ *bolt.Bucket) error {
		if string(name) != string(metaBucket) {
			names = append(names, slices.Clone(name))
		}
		return nil
	}); err != nil {
		return err
	}
	for _, name := range names {
		if err := tx.DeleteBucket(name); err != nil {
			return err
		}
	}
	if err := wipe(tx); err != nil {
		return err
	}
	return meta.Put(versionKey, serializeUID(schemaVersion))
}
