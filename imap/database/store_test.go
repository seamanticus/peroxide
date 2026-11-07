package database

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/emersion/go-imap"

	"github.com/seamanticus/peroxide/config"
	"github.com/seamanticus/peroxide/protonmail"
)

func storedFiles(t *testing.T) []string {
	t.Helper()
	dir, err := config.DataPath("test")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// A stored message reads back as stored, with its record, after a restart
// too; its file, named by the hex of the ID, holds neither the bytes nor the
// ID in clear. One never stored, or deleted, is ErrNotFound.
// evidence: witnesses
func TestBodyRoundTrip(t *testing.T) {
	db, reopen := openTestDB(t)
	literal := []byte("Subject: secret words\r\n\r\nsecret words\r\n")
	info := BodyInfo{
		BodyStructure: &imap.BodyStructure{MIMEType: "text", MIMESubType: "plain", Size: 14, Lines: 1},
		Envelope:      &imap.Envelope{Subject: "secret words"},
		Fallback:      true,
	}
	if err := db.PutBody("../a/b", literal, info); err != nil {
		t.Fatalf("PutBody() = %v", err)
	}

	db = reopen()
	got, err := db.Body("../a/b")
	if err != nil || !bytes.Equal(got, literal) {
		t.Errorf("Body() = %q, %v; want %q", got, err, literal)
	}
	rec, err := db.BodyRecord("../a/b")
	if err != nil {
		t.Fatalf("BodyRecord() = %v", err)
	}
	if rec.Size != uint32(len(literal)) || !rec.Fallback || rec.Envelope.Subject != "secret words" ||
		rec.BodyStructure.MIMESubType != "plain" || rec.BodyStructure.Lines != 1 {
		t.Errorf("BodyRecord() = %+v, want what was stored with Size %v", rec, len(literal))
	}

	files := storedFiles(t)
	if len(files) != 1 || files[0] != "2e2e2f612f62" {
		t.Fatalf("stored files %v, want the hex of the ID", files)
	}
	dir, _ := config.DataPath("test")
	raw, err := os.ReadFile(filepath.Join(dir, files[0]))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("secret")) {
		t.Error("the stored file holds the message in clear")
	}

	if _, err := db.Body("never"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Body() of a message never stored = %v, want ErrNotFound", err)
	}
	if err := db.DeleteBody("../a/b"); err != nil {
		t.Fatalf("DeleteBody() = %v", err)
	}
	if _, err := db.BodyRecord("../a/b"); !errors.Is(err, ErrNotFound) {
		t.Errorf("BodyRecord() after DeleteBody = %v, want ErrNotFound", err)
	}
	if files := storedFiles(t); len(files) != 0 {
		t.Errorf("stored files %v after DeleteBody, want none", files)
	}
	if err := db.DeleteBody("../a/b"); err != nil {
		t.Errorf("second DeleteBody() = %v, want nil", err)
	}
}

// A file put in another message's place, or changed, fails to open: the
// message ID is the additional data, and GCM authenticates the bytes.
// evidence: witnesses
func TestBodyFileSwapFails(t *testing.T) {
	db, _ := openTestDB(t)
	for _, id := range []string{"a", "b"} {
		if err := db.PutBody(id, []byte("message "+id), BodyInfo{}); err != nil {
			t.Fatal(err)
		}
	}
	dir, _ := config.DataPath("test")
	pathA, pathB := filepath.Join(dir, "61"), filepath.Join(dir, "62")
	if err := os.Rename(pathB, pathA); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Body("a"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("Body() of a file renamed from another message = %v, want an open error", err)
	}

	if err := db.PutBody("a", []byte("message a"), BodyInfo{}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(pathA)
	raw[len(raw)-1] ^= 1
	if err := os.WriteFile(pathA, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Body("a"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("Body() of a changed file = %v, want an open error", err)
	}
}

// Wipe removes the stored messages, files left without a record included.
// evidence: witnesses
func TestWipeRemovesBodies(t *testing.T) {
	db, _ := openTestDB(t)
	if err := db.PutBody("a", []byte("a"), BodyInfo{}); err != nil {
		t.Fatal(err)
	}
	dir, _ := config.DataPath("test")
	if err := os.WriteFile(filepath.Join(dir, "tmp-crashed"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := db.Wipe(); err != nil {
		t.Fatalf("Wipe() = %v", err)
	}
	if _, err := db.BodyRecord("a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("BodyRecord() after Wipe = %v, want ErrNotFound", err)
	}
	if files := storedFiles(t); len(files) != 0 {
		t.Errorf("stored files %v after Wipe, want none", files)
	}
}

// Opened with another store key (a new or lost bridge password), the
// database is wiped, messages and stored bodies, and says so; the next Open
// with that key finds it as left. The UIDVALIDITY counter is kept.
// evidence: witnesses
func TestOpenWithAnotherKeyWipes(t *testing.T) {
	db, _ := openTestDB(t)
	inbox, err := db.Mailbox(protonmail.LabelInbox)
	if err != nil {
		t.Fatal(err)
	}
	msg := &protonmail.Message{ID: "a", LabelIDs: []string{protonmail.LabelInbox}}
	if _, err := db.CreateMessage(msg); err != nil {
		t.Fatal(err)
	}
	if err := db.PutBody("a", []byte("a"), BodyInfo{}); err != nil {
		t.Fatal(err)
	}
	before, err := inbox.Status()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	other := [32]byte{9}
	db, keyChanged, err := Open("test", other)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	if !keyChanged {
		t.Error("Open() with another key: keyChanged = false")
	}
	if _, err := db.Message("a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Message() after a key change = %v, want ErrNotFound", err)
	}
	if files := storedFiles(t); len(files) != 0 {
		t.Errorf("stored files %v after a key change, want none", files)
	}
	inbox, err = db.Mailbox(protonmail.LabelInbox)
	if err != nil {
		t.Fatal(err)
	}
	if after, err := inbox.Status(); err != nil || after.UidValidity <= before.UidValidity {
		t.Errorf("UIDVALIDITY %v after a key change, %v; want above %v", after.UidValidity, err, before.UidValidity)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, keyChanged, err = Open("test", other)
	if err != nil || keyChanged {
		t.Errorf("Open() again with the new key: keyChanged %v, %v; want false", keyChanged, err)
	}
	if err == nil {
		db.Close()
	}
}
