package database

import (
	"errors"
	"maps"
	"math"
	"slices"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/seamanticus/peroxide/config"
	"github.com/seamanticus/peroxide/protonmail"
)

// testKey seals the message stores of tests.
func testKey() [32]byte { return [32]byte{1, 2, 3} }

// setTestDirs points the configuration and data directories at dir.
func setTestDirs(t *testing.T, dir string) {
	t.Helper()
	// os.UserConfigDir reads XDG_CONFIG_HOME, else HOME; DataPath reads
	// XDG_DATA_HOME, else HOME.
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("HOME", dir)
}

// openTestDB opens the database of the account "test" in a temporary
// directory, and returns it with a function that closes and opens it again,
// as a restart does.
func openTestDB(t *testing.T) (*User, func() *User) {
	t.Helper()
	setTestDirs(t, t.TempDir())

	db, _, err := Open("test", testKey())
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	t.Cleanup(func() { db.Close() })
	reopen := func() *User {
		t.Helper()
		if err := db.Close(); err != nil {
			t.Fatalf("Close() = %v", err)
		}
		var keyChanged bool
		db, keyChanged, err = Open("test", testKey())
		if err != nil {
			t.Fatalf("Open() = %v", err)
		}
		if keyChanged {
			t.Fatal("Open() with the same key reports another key")
		}
		return db
	}
	return db, reopen
}

func message(id string, order int64, unread int, labelIDs ...string) *protonmail.Message {
	return &protonmail.Message{ID: id, Order: order, Unread: unread, LabelIDs: labelIDs}
}

// uidsOf returns the UIDs of mbox by message ID, and its UIDVALIDITY.
func uidsOf(t *testing.T, mbox *Mailbox) (map[string]uint32, uint32) {
	t.Helper()
	uids := make(map[string]uint32)
	if err := mbox.ForEach(func(_, uid uint32, apiID string) error {
		uids[apiID] = uid
		return nil
	}); err != nil {
		t.Fatalf("ForEach() = %v", err)
	}
	uidValidity, err := mbox.UidValidity()
	if err != nil {
		t.Fatalf("UidValidity() = %v", err)
	}
	return uids, uidValidity
}

// UIDVALIDITY is seconds since gluon's epoch, never repeating: a clock set
// back (an NTP correction, two mailboxes made in one second) still gives a
// higher value, and a clock before the epoch, as a dead CMOS battery leaves
// it at the BIOS date, or past what 32 bits hold is refused rather than
// giving a value that may repeat. UIDVALIDITY 0 is not a valid nz-number
// (RFC 3501 section 9), so the epoch itself gives 1.
// evidence: witnesses
func TestNextUIDValidity(t *testing.T) {
	epoch := time.Unix(uidValidityEpoch, 0)
	tests := []struct {
		name    string
		now     time.Time
		last    uint32
		want    uint32
		wantErr error
	}{
		{"first ever", epoch.Add(1000 * time.Second), 0, 1000, nil},
		{"after an older one", epoch.Add(1000 * time.Second), 999, 1000, nil},
		{"same second", epoch.Add(1000 * time.Second), 1000, 1001, nil},
		{"clock set back", epoch.Add(10 * time.Second), 5000, 5001, nil},
		{"at the epoch", epoch, 0, 1, nil},
		{"BIOS date", time.Date(2009, 1, 1, 0, 0, 0, 0, time.UTC), 0, 0, ErrClockBeforeEpoch},
		{"BIOS date after use", time.Date(2009, 1, 1, 0, 0, 0, 0, time.UTC), 5000, 0, ErrClockBeforeEpoch},
		{"Unix epoch", time.Unix(0, 0), 0, 0, ErrClockBeforeEpoch},
		{"one second early", epoch.Add(-time.Second), 0, 0, ErrClockBeforeEpoch},
		{"last 32-bit second", epoch.Add(math.MaxUint32 * time.Second), 0, math.MaxUint32, nil},
		{"past 32 bits", epoch.Add((math.MaxUint32 + 1) * time.Second), 0, 0, errors.New("any")},
		{"every value given", epoch.Add(1000 * time.Second), math.MaxUint32, 0, errors.New("any")},
	}
	for _, tt := range tests {
		got, err := nextUIDValidity(tt.now, tt.last)
		switch {
		case tt.wantErr == nil && err != nil:
			t.Errorf("%v: nextUIDValidity() = %v", tt.name, err)
		case tt.wantErr == nil && got != tt.want:
			t.Errorf("%v: nextUIDValidity() = %v, want %v", tt.name, got, tt.want)
		case tt.wantErr == ErrClockBeforeEpoch && !errors.Is(err, ErrClockBeforeEpoch):
			t.Errorf("%v: nextUIDValidity() = %v, %v; want ErrClockBeforeEpoch", tt.name, got, err)
		case tt.wantErr != nil && err == nil:
			t.Errorf("%v: nextUIDValidity() = %v, want an error", tt.name, got)
		}
	}
}

// UIDs, UIDVALIDITY and counts survive a restart: a message added after it
// gets the next UID, and one deleted leaves a gap that is never filled.
func TestUIDsPersist(t *testing.T) {
	db, reopen := openTestDB(t)
	inbox, err := db.Mailbox(protonmail.LabelInbox)
	if err != nil {
		t.Fatalf("Mailbox() = %v", err)
	}
	// A page of the download, newest first.
	if _, err := db.SyncPage([]*protonmail.Message{
		message("c", 3, 1, protonmail.LabelInbox),
		message("b", 2, 0, protonmail.LabelInbox),
		message("a", 1, 0, protonmail.LabelInbox),
	}); err != nil {
		t.Fatalf("SyncPage() = %v", err)
	}
	if _, err := db.DeleteMessage("b"); err != nil {
		t.Fatalf("DeleteMessage() = %v", err)
	}
	before, validity := uidsOf(t, inbox)

	db = reopen()
	inbox, err = db.Mailbox(protonmail.LabelInbox)
	if err != nil {
		t.Fatalf("Mailbox() = %v", err)
	}
	after, validityAfter := uidsOf(t, inbox)
	want := map[string]uint32{"c": 1, "a": 3}
	if !maps.Equal(before, want) || !maps.Equal(after, want) || validity != validityAfter {
		t.Errorf("UIDs %v (UIDVALIDITY %v) before the restart, %v (%v) after, want %v both times", before, validity, after, validityAfter, want)
	}
	if total, unread, err := inbox.Count(); err != nil || total != 2 || unread != 1 {
		t.Errorf("Count() = %v, %v, %v after the restart, want 2, 1", total, unread, err)
	}

	if _, err := db.CreateMessage(message("d", 4, 0, protonmail.LabelInbox)); err != nil {
		t.Fatalf("CreateMessage() = %v", err)
	}
	if seqNum, uid, err := inbox.FromApiID("d"); err != nil || seqNum != 3 || uid != 4 {
		t.Errorf("FromApiID(d) = %v, %v, %v; want sequence number 3, UID 4", seqNum, uid, err)
	}
}

// A message both listed by the download and created by an event, in either
// order, has one UID: the second store only updates it. Stored again with
// other labels, it leaves the mailboxes it no longer has, and is counted where
// it stays.
func TestPutMessageIdempotent(t *testing.T) {
	db, _ := openTestDB(t)
	inbox, err := db.Mailbox(protonmail.LabelInbox)
	if err != nil {
		t.Fatalf("Mailbox() = %v", err)
	}
	archive, err := db.Mailbox(protonmail.LabelArchive)
	if err != nil {
		t.Fatalf("Mailbox() = %v", err)
	}

	if _, err := db.CreateMessage(message("a", 1, 1, protonmail.LabelInbox)); err != nil {
		t.Fatalf("CreateMessage() = %v", err)
	}
	changes, err := db.SyncPage([]*protonmail.Message{message("a", 1, 1, protonmail.LabelInbox)})
	if err != nil {
		t.Fatalf("SyncPage() = %v", err)
	}
	if len(changes[0].Added) != 0 || len(changes[0].Removed) != 0 {
		t.Errorf("listing a stored message changed %+v, want nothing", changes[0])
	}
	if uids, _ := uidsOf(t, inbox); !maps.Equal(uids, map[string]uint32{"a": 1}) {
		t.Errorf("INBOX UIDs = %v, want a: 1", uids)
	}

	change, err := db.CreateMessage(message("a", 1, 0, protonmail.LabelArchive))
	if err != nil {
		t.Fatalf("CreateMessage() = %v", err)
	}
	if change.Removed[protonmail.LabelInbox] != 1 || change.Added[protonmail.LabelArchive] != 1 {
		t.Errorf("moving to Archive changed %+v, want INBOX 1 removed, Archive 1 added", change)
	}
	if total, unread, err := inbox.Count(); err != nil || total != 0 || unread != 0 {
		t.Errorf("INBOX Count() = %v, %v, %v; want 0, 0", total, unread, err)
	}
	if total, unread, err := archive.Count(); err != nil || total != 1 || unread != 0 {
		t.Errorf("Archive Count() = %v, %v, %v; want 1, 0", total, unread, err)
	}
}

// A database of another schema version (any before 2, which has no version)
// is wiped when opened, with the version then stamped; one held open by
// another handle fails with ErrInUse rather than waiting.
func TestOpenWipesOldSchemaAndRefusesSecondOpener(t *testing.T) {
	setTestDirs(t, t.TempDir())
	p, err := config.Path("old.db")
	if err != nil {
		t.Fatal(err)
	}
	// What a peroxide before schema version 2 leaves: buckets, no meta.
	old, err := bolt.Open(p, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucket([]byte("mailboxes"))
		if err != nil {
			return err
		}
		_, err = b.CreateBucket([]byte("0"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	db, _, err := Open("old", testKey())
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	t.Cleanup(func() { db.Close() })
	inbox, err := db.Mailbox("0")
	if err != nil {
		t.Fatalf("Mailbox() = %v", err)
	}
	if s, err := inbox.Status(); err != nil || s.Total != 0 || s.UidValidity == 0 {
		t.Errorf("Status() of the wiped INBOX = %+v, %v; want empty with a UIDVALIDITY", s, err)
	}

	if _, _, err := Open("old", testKey()); !errors.Is(err, ErrInUse) {
		t.Errorf("second Open() = %v, want ErrInUse", err)
	}
}

// The download's cursor is stored with each page, so it resumes after a
// restart; Wipe forgets it, the event ID, the messages and the rebuild asked
// for, and mailboxes made again get a higher UIDVALIDITY.
func TestWipe(t *testing.T) {
	db, reopen := openTestDB(t)
	inbox, err := db.Mailbox(protonmail.LabelInbox)
	if err != nil {
		t.Fatalf("Mailbox() = %v", err)
	}
	if _, err := db.SyncPage([]*protonmail.Message{message("b", 2, 0, protonmail.LabelInbox), message("a", 1, 0, protonmail.LabelInbox)}); err != nil {
		t.Fatalf("SyncPage() = %v", err)
	}
	if err := db.SetLastEventID("e"); err != nil {
		t.Fatalf("SetLastEventID() = %v", err)
	}
	if err := db.SetNeedsRebuild(); err != nil {
		t.Fatalf("SetNeedsRebuild() = %v", err)
	}
	_, validity := uidsOf(t, inbox)

	db = reopen()
	if s, err := db.SyncState(); err != nil || s != (SyncState{Cursor: "a", Started: true}) {
		t.Errorf("SyncState() = %+v, %v after a restart; want cursor a, started", s, err)
	}
	if rebuild, err := db.NeedsRebuild(); err != nil || !rebuild {
		t.Errorf("NeedsRebuild() = %v, %v after a restart; want true", rebuild, err)
	}

	if err := db.Wipe(); err != nil {
		t.Fatalf("Wipe() = %v", err)
	}
	if s, err := db.SyncState(); err != nil || s != (SyncState{}) {
		t.Errorf("SyncState() = %+v, %v after Wipe; want not started", s, err)
	}
	if rebuild, err := db.NeedsRebuild(); err != nil || rebuild {
		t.Errorf("NeedsRebuild() = %v, %v after Wipe; want false", rebuild, err)
	}
	if _, ok, err := db.LastEventID(); err != nil || ok {
		t.Errorf("LastEventID() stored = %v, %v after Wipe; want none", ok, err)
	}
	if _, err := db.Message("a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Message(a) = %v after Wipe, want ErrNotFound", err)
	}
	inbox, err = db.Mailbox(protonmail.LabelInbox)
	if err != nil {
		t.Fatalf("Mailbox() = %v", err)
	}
	uids, newValidity := uidsOf(t, inbox)
	if len(uids) != 0 || newValidity <= validity {
		t.Errorf("INBOX after Wipe has UIDs %v, UIDVALIDITY %v; want none, above %v", uids, newValidity, validity)
	}
}

// A mailbox made after messages were stored (a folder that appears with a
// Labels event) holds those labelled with it, newest first.
func TestMailboxFilledFromStoredMessages(t *testing.T) {
	db, _ := openTestDB(t)
	if _, err := db.SyncPage([]*protonmail.Message{
		message("c", 30, 1, "f"),
		message("b", 20, 0, protonmail.LabelInbox),
		message("a", 10, 0, "f"),
	}); err != nil {
		t.Fatalf("SyncPage() = %v", err)
	}
	folder, err := db.Mailbox("f")
	if err != nil {
		t.Fatalf("Mailbox() = %v", err)
	}
	var ids []string
	if err := folder.ForEach(func(_, _ uint32, apiID string) error {
		ids = append(ids, apiID)
		return nil
	}); err != nil {
		t.Fatalf("ForEach() = %v", err)
	}
	if !slices.Equal(ids, []string{"c", "a"}) {
		t.Errorf("folder holds %v, want [c a]", ids)
	}
	if total, unread, err := folder.Count(); err != nil || total != 2 || unread != 1 {
		t.Errorf("Count() = %v, %v, %v; want 2, 1", total, unread, err)
	}
}
