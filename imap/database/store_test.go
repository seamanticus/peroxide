package database

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

// putTestBody stores literal as the body of a message apiID made first.
func putTestBody(t *testing.T, db *User, apiID string, literal []byte, info BodyInfo) {
	t.Helper()
	if _, err := db.CreateMessage(&protonmail.Message{ID: apiID, LabelIDs: []string{protonmail.LabelInbox}}); err != nil {
		t.Fatal(err)
	}
	if _, stored, err := db.PutBody(apiID, literal, info); err != nil || !stored {
		t.Fatalf("PutBody() = %v, %v; want stored", stored, err)
	}
}

// A stored message reads back as stored, with its record, after a restart
// too; its file, named by the hex of the ID and a version, holds neither the
// bytes nor the ID in clear. Storing it again replaces the file. One never
// stored, or deleted, is ErrNotFound.
// evidence: witnesses
func TestBodyRoundTrip(t *testing.T) {
	db, reopen := openTestDB(t)
	literal := []byte("Subject: secret words\r\n\r\nsecret words\r\n")
	info := BodyInfo{
		BodyStructure: &imap.BodyStructure{MIMEType: "text", MIMESubType: "plain", Size: 14, Lines: 1},
		Envelope:      &imap.Envelope{Subject: "secret words"},
		Fallback:      true,
	}
	putTestBody(t, db, "../a/b", literal, info)

	db = reopen()
	rec, got, err := db.Body("../a/b")
	if err != nil || !bytes.Equal(got, literal) {
		t.Errorf("Body() = %q, %v; want %q", got, err, literal)
	}
	if rec.Size != uint32(len(literal)) || !reflect.DeepEqual(rec.BodyInfo, info) {
		t.Errorf("Body() record = %+v, want %+v with Size %v (nil slices and maps nil)", rec, info, len(literal))
	}
	if again, err := db.BodyRecord("../a/b"); err != nil || again.Size != rec.Size {
		t.Errorf("BodyRecord() = %+v, %v; want the record Body gave", again, err)
	}

	files := storedFiles(t)
	if len(files) != 1 || !strings.HasPrefix(files[0], "2e2e2f612f62.") {
		t.Fatalf("stored files %v, want one named by the hex of the ID", files)
	}
	dir, _ := config.DataPath("test")
	raw, err := os.ReadFile(filepath.Join(dir, files[0]))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("secret")) {
		t.Error("the stored file holds the message in clear")
	}

	if _, _, err := db.PutBody("../a/b", []byte("v2"), BodyInfo{}); err != nil {
		t.Fatal(err)
	}
	if _, got, err := db.Body("../a/b"); err != nil || string(got) != "v2" {
		t.Errorf("Body() after storing again = %q, %v; want v2", got, err)
	}
	if again := storedFiles(t); len(again) != 1 || again[0] == files[0] {
		t.Errorf("stored files %v after storing again, want one new file", again)
	}

	if _, _, err := db.Body("never"); !errors.Is(err, ErrNotFound) {
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
	if _, err := db.Message("../a/b"); err != nil {
		t.Errorf("Message() after DeleteBody = %v, want the metadata kept", err)
	}
}

// A message deleted takes its body with it, and a body stored for a message
// the database doesn't have (deleted meanwhile) isn't kept.
// evidence: witnesses
func TestBodyFollowsItsMessage(t *testing.T) {
	db, _ := openTestDB(t)
	putTestBody(t, db, "a", []byte("a"), BodyInfo{})
	if _, err := db.DeleteMessage("a"); err != nil {
		t.Fatalf("DeleteMessage() = %v", err)
	}
	if _, err := db.BodyRecord("a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("BodyRecord() after DeleteMessage = %v, want ErrNotFound", err)
	}
	if files := storedFiles(t); len(files) != 0 {
		t.Errorf("stored files %v after DeleteMessage, want none", files)
	}

	rec, stored, err := db.PutBody("gone", []byte("gone"), BodyInfo{})
	if err != nil || stored || rec != nil {
		t.Errorf("PutBody() of an unknown message = %v, %v, %v; want not stored", rec, stored, err)
	}
	if files := storedFiles(t); len(files) != 0 {
		t.Errorf("stored files %v after PutBody of an unknown message, want none", files)
	}
}

// A file put in another message's place, or changed, fails to open: the
// message ID is the additional data, and GCM authenticates the bytes. A file
// removed from outside drops its record, so the message is stored again.
// evidence: witnesses
func TestBodyFileSwapFails(t *testing.T) {
	db, _ := openTestDB(t)
	for _, id := range []string{"a", "b"} {
		putTestBody(t, db, id, []byte("message "+id), BodyInfo{})
	}
	dir, _ := config.DataPath("test")
	fileOf := func(prefix string) string {
		for _, f := range storedFiles(t) {
			if strings.HasPrefix(f, prefix+".") {
				return filepath.Join(dir, f)
			}
		}
		t.Fatalf("no stored file for %v", prefix)
		return ""
	}
	pathA, pathB := fileOf("61"), fileOf("62")
	if err := os.Rename(pathB, pathA); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.Body("a"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("Body() of a file renamed from another message = %v, want an open error", err)
	}

	if _, _, err := db.PutBody("a", []byte("message a"), BodyInfo{}); err != nil {
		t.Fatal(err)
	}
	pathA = fileOf("61")
	raw, _ := os.ReadFile(pathA)
	raw[len(raw)-1] ^= 1
	if err := os.WriteFile(pathA, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.Body("a"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("Body() of a changed file = %v, want an open error", err)
	}

	if err := os.Remove(pathA); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.Body("a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Body() of a removed file = %v, want ErrNotFound", err)
	}
	if _, err := db.BodyRecord("a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("BodyRecord() after its file was removed = %v, want ErrNotFound", err)
	}
}

// Wipe removes the stored messages, files left without a record included.
// evidence: witnesses
func TestWipeRemovesBodies(t *testing.T) {
	db, _ := openTestDB(t)
	putTestBody(t, db, "a", []byte("a"), BodyInfo{})
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
	putTestBody(t, db, "a", []byte("a"), BodyInfo{})
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
