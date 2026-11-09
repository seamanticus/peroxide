package database

import (
	"maps"
	"testing"

	"github.com/seamanticus/peroxide/protonmail"
)

// Subscriptions: every mailbox starts subscribed; unsubscribing and
// subscribing again, each twice, end where asked without error; a rebuild's
// wipe keeps them, as does a reopen; deleting the mailbox drops its entry.
// evidence: witnesses
func TestSubscriptions(t *testing.T) {
	db, reopen := openTestDB(t)
	check := func(step string, want map[string]bool) {
		t.Helper()
		got, err := db.Unsubscribed()
		if err != nil {
			t.Fatalf("%v: Unsubscribed() = %v", step, err)
		}
		if !maps.Equal(got, want) {
			t.Errorf("%v: unsubscribed %v, want %v", step, got, want)
		}
	}
	check("new", map[string]bool{})

	if _, err := db.Mailbox("w"); err != nil {
		t.Fatal(err)
	}
	for _, labelID := range []string{"w", "w", protonmail.LabelInbox} {
		if err := db.SetSubscribed(labelID, false); err != nil {
			t.Fatalf("SetSubscribed(%v, false) = %v", labelID, err)
		}
	}
	check("unsubscribed", map[string]bool{"w": true, protonmail.LabelInbox: true})

	for range 2 {
		if err := db.SetSubscribed(protonmail.LabelInbox, true); err != nil {
			t.Fatalf("SetSubscribed(INBOX, true) = %v", err)
		}
	}
	check("subscribed again", map[string]bool{"w": true})

	if err := db.Wipe(); err != nil {
		t.Fatal(err)
	}
	check("after a wipe", map[string]bool{"w": true})
	db = reopen()
	check("after a reopen", map[string]bool{"w": true})

	work, err := db.Mailbox("w")
	if err != nil {
		t.Fatal(err)
	}
	if err := work.Delete(); err != nil {
		t.Fatal(err)
	}
	check("after delete", map[string]bool{})
}
