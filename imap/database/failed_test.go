package database

import (
	"testing"
)

// A failed message stays failed across restarts until stored or deleted;
// Wipe forgets them all.
// evidence: witnesses
func TestFailedSet(t *testing.T) {
	db, reopen := openTestDB(t)
	for _, id := range []string{"a", "b", "c"} {
		putTestBody(t, db, id, []byte(id), BodyInfo{})
		if err := db.AddFailed(id); err != nil {
			t.Fatalf("AddFailed() = %v", err)
		}
	}
	if err := db.AddFailed("a"); err != nil {
		t.Fatalf("AddFailed() again = %v", err)
	}
	db = reopen()
	if n, err := db.FailedCount(); err != nil || n != 3 {
		t.Errorf("FailedCount() = %v, %v; want 3 after a restart", n, err)
	}

	if _, _, err := db.PutBody("a", []byte("a"), BodyInfo{}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DeleteMessage("b"); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{"a": false, "b": false, "c": true} {
		if got, err := db.Failed(id); err != nil || got != want {
			t.Errorf("Failed(%v) = %v, %v; want %v", id, got, err, want)
		}
	}
	if err := db.RemoveFailed("c"); err != nil {
		t.Fatal(err)
	}
	if err := db.RemoveFailed("never"); err != nil {
		t.Errorf("RemoveFailed() of an unknown ID = %v", err)
	}
	if n, _ := db.FailedCount(); n != 0 {
		t.Errorf("FailedCount() = %v, want 0", n)
	}

	if err := db.AddFailed("c"); err != nil {
		t.Fatal(err)
	}
	if err := db.Wipe(); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.FailedCount(); n != 0 {
		t.Errorf("FailedCount() after Wipe = %v, want 0", n)
	}
}
