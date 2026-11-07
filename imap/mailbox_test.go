package imap

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/emersion/go-imap"
	imapbackend "github.com/emersion/go-imap/backend"
	imapserver "github.com/emersion/go-imap/server"

	"github.com/seamanticus/peroxide/auth"
	"github.com/seamanticus/peroxide/imap/database"
	"github.com/seamanticus/peroxide/protonmail"
)

// setBubbleClock moves the fake clock of the testing/synctest bubble it is
// called in, which starts at 2000-01-01, to 2026-11-01: the database refuses
// to make a mailbox while the clock reads a date before the UIDVALIDITY epoch
// (2023-02-01). Call it first in the bubble, while nothing else runs: the
// sleep then returns at once, the clock moved.
func setBubbleClock() {
	time.Sleep(time.Until(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)))
}

// testStoreKey seals the message stores of tests.
func testStoreKey() auth.StoreKey { return auth.StoreKey{1, 2, 3} }

// openTestDB opens a database in a temporary directory.
func openTestDB(t *testing.T) *database.User {
	t.Helper()
	// os.UserConfigDir reads one of these, depending on the OS; the store
	// goes under XDG_DATA_HOME.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("AppData", dir)

	db, _, err := database.Open("test", testStoreKey())
	if err != nil {
		t.Fatalf("database.Open() = %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("db.Close() = %v", err)
		}
	})
	return db
}

// syncTestMailbox stores msgs in db in the order given, as events would, so
// that the mailbox labelID holds those of msgs labelled with it with UIDs
// 1, 2, ... in that order.
func syncTestMailbox(t *testing.T, db *database.User, labelID string, msgs []*protonmail.Message) *database.Mailbox {
	t.Helper()
	mboxDB, err := db.Mailbox(labelID)
	if err != nil {
		t.Fatalf("db.Mailbox() = %v", err)
	}
	for _, msg := range msgs {
		if _, err := db.CreateMessage(msg); err != nil {
			t.Fatalf("CreateMessage() = %v", err)
		}
	}
	return mboxDB
}

// newTestMailbox returns a synchronized INBOX backed by a temporary database,
// holding msgs.
func newTestMailbox(t *testing.T, msgs []*protonmail.Message, extraLabels ...string) (*mailbox, *database.User) {
	t.Helper()
	db := openTestDB(t)
	for _, labelID := range extraLabels {
		syncTestMailbox(t, db, labelID, nil)
	}
	mboxDB := syncTestMailbox(t, db, protonmail.LabelInbox, msgs)

	return &mailbox{
		name:    imap.InboxName,
		label:   protonmail.LabelInbox,
		u:       &user{db: db, u: &protonmail.User{Name: "bob"}, syncSem: make(chan struct{}, 1), sendSem: make(chan struct{}, 1)},
		db:      mboxDB,
		deleted: make(map[string]struct{}),
	}, db
}

func inboxMessage(id string, unread int) *protonmail.Message {
	return &protonmail.Message{ID: id, Unread: unread, LabelIDs: []string{protonmail.LabelInbox}}
}

func checkStatus(t *testing.T, mbox *mailbox, wantMessages, wantUnseen uint32) {
	t.Helper()
	status, err := mbox.Status([]imap.StatusItem{imap.StatusMessages, imap.StatusUnseen})
	if err != nil {
		t.Fatalf("Status() = %v", err)
	}
	if status.Messages != wantMessages || status.Unseen != wantUnseen {
		t.Errorf("Status() = MESSAGES %v UNSEEN %v, want MESSAGES %v UNSEEN %v",
			status.Messages, status.Unseen, wantMessages, wantUnseen)
	}
}

// FLAGS and PERMANENTFLAGS list label flags by label ID, not in map order:
// with this many labels, map order would differ between runs.
// evidence: witnesses
func TestStatusFlagsSorted(t *testing.T) {
	mbox, _ := newTestMailbox(t, nil)
	mbox.u.flags = map[string]string{"e": "E", "a": "A", "d": "D", "b": "B", "c": "C", "f": "F"}
	status, err := mbox.Status([]imap.StatusItem{imap.StatusMessages})
	if err != nil {
		t.Fatalf("Status() = %v", err)
	}
	want := []string{imap.SeenFlag, imap.DeletedFlag, "A", "B", "C", "D", "E", "F"}
	if !slices.Equal(status.Flags, want) {
		t.Errorf("Flags = %v, want %v", status.Flags, want)
	}
	if !slices.Equal(status.PermanentFlags, want) {
		t.Errorf("PermanentFlags = %v, want %v", status.PermanentFlags, want)
	}
}

// STATUS follows the local database, which events keep up to date.
func TestStatusFollowsEvents(t *testing.T) {
	mbox, db := newTestMailbox(t, []*protonmail.Message{
		inboxMessage("a", 0),
		inboxMessage("b", 0),
	})
	checkStatus(t, mbox, 2, 0)

	if _, err := db.CreateMessage(inboxMessage("c", 1)); err != nil {
		t.Fatalf("CreateMessage() = %v", err)
	}
	checkStatus(t, mbox, 3, 1)

	read := 0
	if _, _, err := db.UpdateMessage("c", &protonmail.EventMessageUpdate{Unread: &read}); err != nil {
		t.Fatalf("UpdateMessage() = %v", err)
	}
	checkStatus(t, mbox, 3, 0)

	unread := 1
	if _, _, err := db.UpdateMessage("a", &protonmail.EventMessageUpdate{Unread: &unread}); err != nil {
		t.Fatalf("UpdateMessage() = %v", err)
	}
	checkStatus(t, mbox, 3, 1)

	if _, err := db.DeleteMessage("a"); err != nil {
		t.Fatalf("DeleteMessage() = %v", err)
	}
	checkStatus(t, mbox, 2, 0)
}

// FETCH 1:* must reach messages added by events after login.
func TestListMessagesStarReachesNewMessages(t *testing.T) {
	mbox, db := newTestMailbox(t, []*protonmail.Message{inboxMessage("a", 0)})
	if _, err := db.CreateMessage(inboxMessage("b", 1)); err != nil {
		t.Fatalf("CreateMessage() = %v", err)
	}

	seqSet, err := imap.ParseSeqSet("1:*")
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan *imap.Message, 10)
	if err := mbox.ListMessages(false, seqSet, []imap.FetchItem{imap.FetchFlags}, ch); err != nil {
		t.Fatalf("ListMessages() = %v", err)
	}

	var seqNums []uint32
	for msg := range ch {
		seqNums = append(seqNums, msg.SeqNum)
	}
	if len(seqNums) != 2 || seqNums[0] != 1 || seqNums[1] != 2 {
		t.Errorf("ListMessages(1:*) returned sequence numbers %v, want [1 2]", seqNums)
	}
}

// recount counts the messages of mboxDB from its entries, the slow way the
// running counts replace.
func recount(t *testing.T, db *database.User, mboxDB *database.Mailbox) (total, unread int) {
	t.Helper()
	err := mboxDB.ForEach(func(_, _ uint32, apiID string) error {
		total++
		msg, err := db.Message(apiID)
		if err == database.ErrNotFound {
			return nil
		} else if err != nil {
			return err
		}
		if msg.Unread == 1 {
			unread++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ForEach() = %v", err)
	}
	return total, unread
}

func TestCountsMatchRecount(t *testing.T) {
	db := openTestDB(t)
	msg := func(id string, unread int, labelIDs ...string) *protonmail.Message {
		return &protonmail.Message{ID: id, Unread: unread, LabelIDs: labelIDs}
	}
	inbox, archive, allMail := protonmail.LabelInbox, protonmail.LabelArchive, protonmail.LabelAllMail
	mailboxes := map[string]*database.Mailbox{
		inbox:   syncTestMailbox(t, db, inbox, nil),
		archive: syncTestMailbox(t, db, archive, nil),
		allMail: syncTestMailbox(t, db, allMail, nil),
	}
	for _, m := range []*protonmail.Message{msg("a", 1, inbox, allMail), msg("b", 0, inbox, allMail), msg("c", 1, archive, allMail)} {
		if _, err := db.CreateMessage(m); err != nil {
			t.Fatalf("CreateMessage() = %v", err)
		}
	}

	read, unread := 0, 1
	steps := []struct {
		name string
		do   func() error
	}{
		{"synchronized", func() error { return nil }},
		{"new unread message", func() error {
			_, err := db.CreateMessage(msg("d", 1, inbox, allMail))
			return err
		}},
		{"move to archive", func() error {
			_, _, err := db.UpdateMessage("a", &protonmail.EventMessageUpdate{LabelIDs: []string{archive, allMail}})
			return err
		}},
		{"mark read", func() error {
			_, _, err := db.UpdateMessage("c", &protonmail.EventMessageUpdate{Unread: &read})
			return err
		}},
		{"mark unread and move", func() error {
			_, _, err := db.UpdateMessage("b", &protonmail.EventMessageUpdate{
				Unread: &unread, LabelIDsAdded: []string{archive}, LabelIDsRemoved: []string{inbox},
			})
			return err
		}},
		{"delete", func() error {
			_, err := db.DeleteMessage("d")
			return err
		}},
		{"recreate existing message", func() error {
			_, err := db.CreateMessage(msg("c", 1, archive, allMail))
			return err
		}},
	}
	for _, step := range steps {
		if err := step.do(); err != nil {
			t.Fatalf("%v: %v", step.name, err)
		}
		for labelID, mboxDB := range mailboxes {
			total, unread, err := mboxDB.Count()
			if err != nil {
				t.Fatalf("%v: Count() of %v = %v", step.name, labelID, err)
			}
			wantTotal, wantUnread := recount(t, db, mboxDB)
			if total != wantTotal || unread != wantUnread {
				t.Errorf("%v: Count() of %v = %v, %v, want %v, %v", step.name, labelID, total, unread, wantTotal, wantUnread)
			}
		}
	}
}

func mailboxNames(u *user) map[string]string {
	names := make(map[string]string)
	for labelID, mbox := range u.mailboxes {
		names[labelID] = mbox.Name()
	}
	return names
}

// A folder created by a Labels event after its messages were stored (moved
// into it in the same page, or downloaded first) opens holding them, its
// counts with it.
func TestLabelEventFolderHoldsStoredMessages(t *testing.T) {
	inbox, db := newTestMailbox(t, nil)
	if _, err := db.CreateMessage(&protonmail.Message{ID: "a", Unread: 1, LabelIDs: []string{"f"}}); err != nil {
		t.Fatal(err)
	}
	u := inbox.u
	u.mailboxes = map[string]*mailbox{protonmail.LabelInbox: inbox}
	u.flags = map[string]string{}
	events := make(chan *protonmail.Event, 1)
	events <- &protonmail.Event{ID: "e1", Labels: []*protonmail.EventLabel{{
		ID: "f", Action: protonmail.EventCreate,
		Label: &protonmail.Label{ID: "f", Name: "Work", Type: protonmail.LabelMessage, Exclusive: 1},
	}}}
	close(events)
	u.receiveEvents(make(chan imapbackend.Update, 10), events)

	folder := u.getMailboxByLabel("f")
	if folder == nil {
		t.Fatal("no mailbox for the folder created")
	}
	checkStatus(t, folder, 1, 1)
}

// "*" is the last message, by UID or sequence number (RFC 3501 section 9),
// so "n:*" holds it even when n is past it, and ranges are taken either way
// round. Gaps in UIDs (expunged messages) are not filled.
// evidence: witnesses
func TestEntriesStar(t *testing.T) {
	mbox, db := newTestMailbox(t, []*protonmail.Message{inboxMessage("a", 0), inboxMessage("b", 0), inboxMessage("c", 0)})
	if _, err := db.DeleteMessage("b"); err != nil {
		t.Fatal(err)
	}
	// a: seq 1 UID 1; c: seq 2 UID 3.
	for _, tc := range []struct {
		set  string
		uid  bool
		want []string
	}{
		{"1:*", false, []string{"a", "c"}},
		{"*", false, []string{"c"}},
		{"5:*", false, []string{"c"}},
		{"2:1", false, []string{"a", "c"}},
		{"3", false, nil},
		{"2", true, nil},
		{"3", true, []string{"c"}},
		{"10:*", true, []string{"c"}},
		{"*:1", true, []string{"a", "c"}},
	} {
		seqSet, err := imap.ParseSeqSet(tc.set)
		if err != nil {
			t.Fatal(err)
		}
		got, err := mbox.fromSeqSet(tc.uid, seqSet)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("fromSeqSet(uid %v, %v) = %v, %v; want %v", tc.uid, tc.set, got, err, tc.want)
		}
	}
}

// BenchmarkFetchFlags is FETCH 1:* (FLAGS) of a mailbox of 5000 messages,
// the database's part of what aerc asks when it opens a folder.
func BenchmarkFetchFlags(b *testing.B) {
	dir := b.TempDir()
	b.Setenv("XDG_CONFIG_HOME", dir)
	b.Setenv("XDG_DATA_HOME", dir)
	b.Setenv("HOME", dir)
	b.Setenv("AppData", dir)
	db, _, err := database.Open("bench", testStoreKey())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	mboxDB, err := db.Mailbox(protonmail.LabelInbox)
	if err != nil {
		b.Fatal(err)
	}
	var page []*protonmail.Message
	for i := range 5000 {
		page = append(page, inboxMessage(fmt.Sprintf("m%05d", i), i%2))
	}
	if _, err := db.SyncPage(page); err != nil {
		b.Fatal(err)
	}
	mbox := &mailbox{
		name: imap.InboxName, label: protonmail.LabelInbox, db: mboxDB, deleted: make(map[string]struct{}),
		u: &user{db: db, u: &protonmail.User{Name: "bob"}, flags: map[string]string{}},
	}
	all, err := imap.ParseSeqSet("1:*")
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		ch := make(chan *imap.Message)
		go func() {
			for range ch {
			}
		}()
		if _, err := mbox.listMessages(false, all, []imap.FetchItem{imap.FetchFlags}, ch); err != nil {
			b.Fatal(err)
		}
	}
}

// Folders and labels created, renamed and deleted in Proton change mailboxes
// and flags without waiting for the next login, and without telling clients.
func TestLabelEvents(t *testing.T) {
	inbox, _ := newTestMailbox(t, nil)
	u := inbox.u
	u.mailboxes = map[string]*mailbox{protonmail.LabelInbox: inbox}
	u.flags = map[string]string{protonmail.LabelStarred: imap.FlaggedFlag}

	folder := func(name string) *protonmail.Label {
		return &protonmail.Label{ID: "f", Name: name, Type: protonmail.LabelMessage, Exclusive: 1}
	}
	label := func(name string) *protonmail.Label {
		return &protonmail.Label{ID: "l", Name: name, Type: protonmail.LabelMessage}
	}
	inboxOnly := map[string]string{protonmail.LabelInbox: imap.InboxName}
	withFolder := map[string]string{protonmail.LabelInbox: imap.InboxName, "f": "Work"}
	renamed := map[string]string{protonmail.LabelInbox: imap.InboxName, "f": "Projects"}
	starredOnly := map[string]string{protonmail.LabelStarred: imap.FlaggedFlag}
	withLabel := map[string]string{protonmail.LabelStarred: imap.FlaggedFlag, "l": "To_do"}

	steps := []struct {
		name          string
		event         *protonmail.EventLabel
		wantMailboxes map[string]string
		wantFlags     map[string]string
	}{
		{"create folder", &protonmail.EventLabel{ID: "f", Action: protonmail.EventCreate, Label: folder("Work")}, withFolder, starredOnly},
		{"create label", &protonmail.EventLabel{ID: "l", Action: protonmail.EventCreate, Label: label("To do")}, withFolder, withLabel},
		{"create contact group", &protonmail.EventLabel{ID: "c", Action: protonmail.EventCreate, Label: &protonmail.Label{ID: "c", Name: "Friends", Type: protonmail.LabelContact}}, withFolder, withLabel},
		// Sent after every read or unread change in the folder.
		{"update without changes", &protonmail.EventLabel{ID: "f", Action: protonmail.EventUpdate, Label: folder("Work")}, withFolder, withLabel},
		// Seen in Proton's events: names and Exclusive don't match INBOX or
		// \Flagged, and the event ID differs from the label ID.
		{"update system folder", &protonmail.EventLabel{ID: "k7rAD-", Action: protonmail.EventUpdate, Label: &protonmail.Label{ID: protonmail.LabelInbox, Name: "Inbox", Type: protonmail.LabelMessage}}, withFolder, withLabel},
		{"update system label", &protonmail.EventLabel{ID: "ZIZtzh", Action: protonmail.EventUpdate, Label: &protonmail.Label{ID: protonmail.LabelStarred, Name: "Starred", Type: protonmail.LabelMessage}}, withFolder, withLabel},
		{"update unknown label", &protonmail.EventLabel{ID: "u", Action: protonmail.EventUpdate, Label: &protonmail.Label{ID: "u", Name: "Primary", Type: protonmail.LabelMessage}}, withFolder, withLabel},
		{"rename folder", &protonmail.EventLabel{ID: "f", Action: protonmail.EventUpdate, Label: folder("Projects")}, renamed, withLabel},
		{"rename label", &protonmail.EventLabel{ID: "l", Action: protonmail.EventUpdateFlags, Label: label("Later on")}, renamed, map[string]string{protonmail.LabelStarred: imap.FlaggedFlag, "l": "Later_on"}},
		{"folder becomes label", &protonmail.EventLabel{ID: "f", Action: protonmail.EventUpdate, Label: &protonmail.Label{ID: "f", Name: "Other", Type: protonmail.LabelMessage}}, renamed, map[string]string{protonmail.LabelStarred: imap.FlaggedFlag, "l": "Later_on"}},
		{"delete label", &protonmail.EventLabel{ID: "l", Action: protonmail.EventDelete}, renamed, starredOnly},
		{"delete folder", &protonmail.EventLabel{ID: "f", Action: protonmail.EventDelete}, inboxOnly, starredOnly},
	}

	var created *mailbox
	for _, step := range steps {
		events := make(chan *protonmail.Event, 1)
		events <- &protonmail.Event{Labels: []*protonmail.EventLabel{step.event}}
		close(events)
		updates := make(chan imapbackend.Update, 10)
		u.receiveEvents(updates, events)

		if n := len(updates); n != 0 {
			t.Errorf("%v: sent %v updates, want none", step.name, n)
		}
		if got := mailboxNames(u); !maps.Equal(got, step.wantMailboxes) {
			t.Errorf("%v: mailboxes = %v, want %v", step.name, got, step.wantMailboxes)
		}
		if !maps.Equal(u.flags, step.wantFlags) {
			t.Errorf("%v: flags = %v, want %v", step.name, u.flags, step.wantFlags)
		}

		if mbox, ok := u.mailboxes["f"]; ok {
			if created == nil {
				created = mbox
			} else if mbox != created {
				// A new mailbox would lose the old one's UIDs and selections.
				t.Errorf("%v: folder's mailbox replaced", step.name)
			}
		}
	}

	if created == nil {
		t.Fatal("folder never created")
	}
	if _, err := created.db.UidNext(); err == nil {
		t.Error("deleted folder's mailbox still in the local database")
	}
}

func addressEmails(addrs []*protonmail.Address) []string {
	var emails []string
	for _, a := range addrs {
		emails = append(emails, a.Email)
	}
	return emails
}

// Addresses created, changed and deleted in Proton change the address list
// drafts take their sender from, without changing a list handed out before.
func TestAddressEvents(t *testing.T) {
	inbox, _ := newTestMailbox(t, nil)
	u := inbox.u
	u.mailboxes = map[string]*mailbox{protonmail.LabelInbox: inbox}
	u.addrs = []*protonmail.Address{{ID: "a1", Email: "bob@proton.me"}}

	steps := []struct {
		name  string
		event *protonmail.EventAddress
		want  []string
	}{
		{"create", &protonmail.EventAddress{ID: "a2", Action: protonmail.EventCreate, Address: &protonmail.Address{ID: "a2", Email: "alias@proton.me"}}, []string{"bob@proton.me", "alias@proton.me"}},
		{"update", &protonmail.EventAddress{ID: "a2", Action: protonmail.EventUpdate, Address: &protonmail.Address{ID: "a2", Email: "alias2@proton.me"}}, []string{"bob@proton.me", "alias2@proton.me"}},
		{"update unknown creates", &protonmail.EventAddress{ID: "a3", Action: protonmail.EventUpdateFlags, Address: &protonmail.Address{ID: "a3", Email: "new@proton.me"}}, []string{"bob@proton.me", "alias2@proton.me", "new@proton.me"}},
		{"update without address", &protonmail.EventAddress{ID: "a1", Action: protonmail.EventUpdate}, []string{"bob@proton.me", "alias2@proton.me", "new@proton.me"}},
		{"delete", &protonmail.EventAddress{ID: "a2", Action: protonmail.EventDelete}, []string{"bob@proton.me", "new@proton.me"}},
		{"delete unknown", &protonmail.EventAddress{ID: "zz", Action: protonmail.EventDelete}, []string{"bob@proton.me", "new@proton.me"}},
		{"unknown action", &protonmail.EventAddress{ID: "a1", Action: 9}, []string{"bob@proton.me", "new@proton.me"}},
	}
	for _, step := range steps {
		before := u.addresses()
		kept := slices.Clone(addressEmails(before))

		events := make(chan *protonmail.Event, 1)
		events <- &protonmail.Event{Addresses: []*protonmail.EventAddress{step.event}}
		close(events)
		u.receiveEvents(make(chan imapbackend.Update, 10), events)

		if got := addressEmails(u.addresses()); !slices.Equal(got, step.want) {
			t.Errorf("%v: addresses = %v, want %v", step.name, got, step.want)
		}
		if got := addressEmails(before); !slices.Equal(got, kept) {
			t.Errorf("%v: list read before changed to %v, was %v", step.name, got, kept)
		}
	}
}

// roundTripFunc answers HTTP requests in memory: unlike an httptest server,
// it starts no goroutine, so it works inside a synctest bubble.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// disconnect finds the connections logged in as a user by their addresses,
// as go-imap gives them to Login, and ends them through the deadline: the
// client sees the connection close, and other connections stay.
func TestDisconnectClosesItsConnections(t *testing.T) {
	be := &backend{users: make(map[string]*user), logins: make(map[string]*user), updates: make(chan imapbackend.Update)}
	s := imapserver.New(be)
	s.Enable(Extension{})
	s.ErrorLog = log.New(io.Discard, "", 0)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(l)
	t.Cleanup(func() { s.Close() })

	dial := func() net.Conn {
		t.Helper()
		c, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		// The greeting: the server has the connection from then on.
		if _, err := bufio.NewReader(c).ReadString('\n'); err != nil {
			t.Fatalf("read greeting: %v", err)
		}
		return c
	}
	kept, ended := dial(), dial()

	bob := &user{username: "bob"}
	// What Login records, with the addresses as the server sees them.
	key := "tcp " + ended.LocalAddr().String() + " " + ended.RemoteAddr().String()
	be.logins[key] = bob
	be.disconnect(bob)

	ended.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadAll(ended); err != nil {
		t.Errorf("reading the ended connection: %v, want EOF", err)
	}
	kept.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := kept.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("reading the other connection = %v, want it still open", err)
	}
}

// The last Logout can come while the events goroutine is still applying a
// page, blocked sending its updates: go-imap calls Logout with its server
// lock held, and its update loop needs that lock to take more, so Logout must
// not wait. The page then finishes with the session intact, and only then is
// the database closed and the account removed from the backend; a login
// meanwhile finds the session closing (getUser waits for it).
func TestLogoutWhileApplyingEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		setBubbleClock()
		inbox, db := newTestMailbox(t, nil)
		// Unbuffered and unread: the page blocks on its first update, as when
		// go-imap's update loop waits for the server lock Logout runs under.
		be := &backend{users: make(map[string]*user), updates: make(chan imapbackend.Update)}
		u := inbox.u
		u.backend = be
		u.username = "bob"
		u.mailboxes = map[string]*mailbox{protonmail.LabelInbox: inbox}
		u.flags = map[string]string{}
		u.numClients = 1
		u.done = make(chan struct{})
		// A delete page makes no request.
		u.c = &protonmail.Client{RootURL: "https://api.test", HTTPClient: &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				t.Errorf("unexpected request %v %v", r.Method, r.URL.Path)
				return nil, errors.New("unexpected request")
			}),
		}}
		be.users["bob"] = u
		syncTestMailbox(t, db, protonmail.LabelInbox, []*protonmail.Message{inboxMessage("a", 0)})

		// A message deleted: its EXPUNGE is the page's one update.
		events := make(chan *protonmail.Event)
		u.startEvents(be.updates, events)
		events <- &protonmail.Event{ID: "e1", Messages: []*protonmail.EventMessage{{
			ID:     "a",
			Action: protonmail.EventDelete,
		}}}
		synctest.Wait() // blocked sending the page's EXPUNGE

		if err := u.Logout(); err != nil {
			t.Fatalf("Logout() = %v", err)
		}
		close(events) // as the receiver does once done is closed
		synctest.Wait()

		be.Lock()
		kept := be.users["bob"] == u
		be.Unlock()
		if !kept {
			t.Error("the account left the backend before its events stopped")
		}
		if _, _, err := db.LastEventID(); err != nil {
			t.Errorf("the database closed before its events stopped: %v", err)
		}

		// What go-imap does once Logout has returned: take the update.
		close((<-be.updates).Done())
		u.eventLoop.Wait()

		be.Lock()
		_, still := be.users["bob"]
		be.Unlock()
		if still {
			t.Error("the account is still in the backend after its events stopped")
		}
		if u.u != nil || u.c != nil || u.privateKeys != nil {
			t.Error("the session was kept after its events stopped")
		}
		if _, _, err := db.LastEventID(); err == nil {
			// Reopening below would fail on bolt's file lock (ErrInUse).
			t.Fatal("the database is still open after its events stopped")
		}

		// The page cut by Logout was recorded before the database closed: the
		// next session resumes after it.
		reopened, _, err := database.Open("test", testStoreKey())
		if err != nil {
			t.Fatalf("reopen database: %v", err)
		}
		if id, ok, err := reopened.LastEventID(); err != nil || !ok || id != "e1" {
			t.Errorf("stored last event ID = %q, %v, %v, want e1", id, ok, err)
		}
		if err := reopened.Close(); err != nil {
			t.Errorf("close reopened database: %v", err)
		}
	})
}

// fakeMetadata answers the download's pages (POST /mail/v4/messages, newest
// first, up to EndID) from msgs, given newest first, as Proton does: the page
// up to EndID starts with EndID. Before answering it calls hook with the
// number of the page (from 1), so that a test can block it. Requests for
// specific IDs (MessagesByID) are refused with status (500 if 0), an error
// only if not quiet. A request whose context is cancelled fails, as a real
// transport's does.
type fakeMetadata struct {
	t      *testing.T
	msgs   []*protonmail.Message
	hook   func(page int)
	quiet  bool
	status int

	mu    sync.Mutex
	pages int
}

func (f *fakeMetadata) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodPost || r.URL.Path != "/mail/v4/messages" || r.Header.Get("X-HTTP-Method-Override") != http.MethodGet {
		f.t.Errorf("unexpected request %v %v", r.Method, r.URL.Path)
		return nil, errors.New("unexpected request")
	}
	var req struct {
		ID       []string
		EndID    string
		Desc     int
		PageSize int
		Sort     string
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.t.Errorf("decode request: %v", err)
	}
	if len(req.ID) > 0 {
		if !f.quiet {
			f.t.Errorf("unexpected request for messages %v", req.ID)
		}
		status := f.status
		if status == 0 {
			status = http.StatusInternalServerError
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"Code":2001,"Error":"refused"}`)),
			Request:    r,
		}, nil
	}
	if req.Desc != 1 || req.Sort != "ID" || req.PageSize != protonmail.MetadataPageSize {
		f.t.Errorf("page asked for with %+v, want Desc 1, Sort ID, PageSize %v", req, protonmail.MetadataPageSize)
	}
	f.mu.Lock()
	f.pages++
	page := f.pages
	f.mu.Unlock()
	if f.hook != nil {
		f.hook(page)
	}
	if err := r.Context().Err(); err != nil {
		return nil, err
	}

	start := 0
	if req.EndID != "" {
		start = slices.IndexFunc(f.msgs, func(m *protonmail.Message) bool { return m.ID == req.EndID })
		if start < 0 {
			f.t.Errorf("EndID %v is no message", req.EndID)
			start = len(f.msgs)
		}
	}
	// A small page, to exercise more than one.
	end := min(start+2, len(f.msgs))
	b, err := json.Marshal(struct {
		Code     int
		Messages []*protonmail.Message
	}{1000, f.msgs[start:end]})
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(b)),
		Request:    r,
	}, nil
}

// newDownloadTestUser returns a user with INBOX whose client answers the
// download from api, and the channel go-imap would read its updates from,
// acknowledged at once. Its events receiver is rewound by the download, and
// records the IDs asked for.
func newDownloadTestUser(t *testing.T, api *fakeMetadata) (*user, *mailbox, chan imapbackend.Update, *fakeRewinder) {
	t.Helper()
	inbox, _ := newTestMailbox(t, nil)
	u := inbox.u
	u.mailboxes = map[string]*mailbox{protonmail.LabelInbox: inbox}
	u.flags = map[string]string{}
	u.done = make(chan struct{})
	u.eventSent = make(chan struct{}, 1)
	updates := make(chan imapbackend.Update)
	u.backend = &backend{updates: updates}
	u.c = &protonmail.Client{RootURL: "https://api.test", HTTPClient: &http.Client{Transport: api}}
	go func() {
		for update := range updates {
			close(update.Done())
		}
	}()
	t.Cleanup(func() { close(updates) })
	rw := &fakeRewinder{}
	u.eventsReceiver = rw
	return u, inbox, updates, rw
}

// fakeRewinder stands for the events receiver: it records the IDs the events
// were rewound to and how often a poll was asked for.
type fakeRewinder struct {
	mu      sync.Mutex
	rewinds []string
	polls   int
}

func (r *fakeRewinder) Rewind(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rewinds = append(r.rewinds, id)
}

func (r *fakeRewinder) Poll() <-chan error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.polls++
	ch := make(chan error, 1)
	ch <- nil
	return ch
}

func uidsByID(t *testing.T, mbox *mailbox) map[string]uint32 {
	t.Helper()
	uids := make(map[string]uint32)
	if err := mbox.db.ForEach(func(_, uid uint32, apiID string) error {
		uids[apiID] = uid
		return nil
	}); err != nil {
		t.Fatalf("ForEach() = %v", err)
	}
	return uids
}

// The download lists every message newest first, as proton-bridge's sync, so
// the newest gets UID 1; it pages with EndID and skips the message each page
// starts with. A page is asked for holding no lock, as bridge's sync: an event
// page arriving while a page hangs is applied at once (its message gets the
// next UID), and the page stored after it leaves that UID alone. Every
// message has one UID, none is given twice, the counts match, and once done
// the events are rewound to where the download began.
func TestDownloadAndEventsInterleave(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		setBubbleClock()
		fetching, release := make(chan struct{}), make(chan struct{})
		api := &fakeMetadata{t: t, msgs: []*protonmail.Message{
			inboxMessage("m5", 0), inboxMessage("m4", 0), inboxMessage("m3", 1),
			inboxMessage("m2", 0), inboxMessage("m1", 0),
		}}
		api.hook = func(page int) {
			if page == 2 {
				close(fetching)
				<-release
			}
		}
		u, inbox, updates, rw := newDownloadTestUser(t, api)
		if err := u.db.SetLastEventID("e0"); err != nil {
			t.Fatal(err)
		}
		u.startDownload()

		<-fetching // page 1 (m5, m4) is stored; page 2 is being fetched
		events := make(chan *protonmail.Event, 1)
		events <- &protonmail.Event{ID: "e1", After: "e0", Messages: []*protonmail.EventMessage{
			{ID: "m6", Action: protonmail.EventCreate, Created: inboxMessage("m6", 1)},
		}}
		close(events)
		applied := make(chan struct{})
		go func() {
			u.receiveEvents(updates, events)
			close(applied)
		}()
		<-applied // while the page still hangs
		if _, uid, err := inbox.db.FromApiID("m6"); err != nil || uid != 3 {
			t.Errorf("FromApiID(m6) = UID %v, %v while a page was fetched; want UID 3", uid, err)
		}
		close(release)
		u.downloads.Wait()

		want := map[string]uint32{"m5": 1, "m4": 2, "m6": 3, "m3": 4, "m2": 5, "m1": 6}
		if got := uidsByID(t, inbox); !maps.Equal(got, want) {
			t.Errorf("UIDs = %v, want %v", got, want)
		}
		checkStatus(t, inbox, 6, 2)
		if s, err := u.db.SyncState(); err != nil || !s.Done {
			t.Errorf("SyncState() = %+v, %v; want done", s, err)
		}
		rw.mu.Lock()
		rewinds, polls := slices.Clone(rw.rewinds), rw.polls
		rw.mu.Unlock()
		if !slices.Equal(rewinds, []string{"e0"}) || polls == 0 {
			t.Errorf("rewound to %v with %v polls once done, want [e0] and a poll", rewinds, polls)
		}
		if id, _, err := u.db.LastEventID(); err != nil || id != "e0" {
			t.Errorf("stored last event ID = %q, %v once done; want e0, the replay's start", id, err)
		}
	})
}

// A page fetched before an event and stored after it brings back what the
// event did (here, a message deleted). Once the download is done the events
// after its start are delivered again: pages asked for before the rewind are
// dropped, and the replay deletes the message again, as bridge's rewind does.
func TestDownloadReplayCorrectsStalePage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		setBubbleClock()
		fetched, release := make(chan struct{}), make(chan struct{})
		api := &fakeMetadata{t: t, msgs: []*protonmail.Message{inboxMessage("m2", 0), inboxMessage("m1", 0)}}
		api.hook = func(page int) {
			if page == 1 {
				close(fetched)
				<-release
			}
		}
		u, inbox, updates, _ := newDownloadTestUser(t, api)
		if err := u.db.SetLastEventID("e0"); err != nil {
			t.Fatal(err)
		}
		events := make(chan *protonmail.Event, 3)
		applied := make(chan struct{})
		go func() {
			u.receiveEvents(updates, events)
			close(applied)
		}()
		u.startDownload()

		<-fetched // Proton answered with m2, m1; it isn't stored yet
		deleteM1 := &protonmail.Event{ID: "e1", After: "e0", Messages: []*protonmail.EventMessage{
			{ID: "m1", Action: protonmail.EventDelete},
		}}
		events <- deleteM1
		synctest.Wait()
		close(release)
		u.downloads.Wait()
		if _, _, err := inbox.db.FromApiID("m1"); err != nil {
			t.Fatalf("FromApiID(m1) = %v: the stale page should have brought it back", err)
		}

		// The receiver: a page asked for before the rewind, then the replay.
		events <- &protonmail.Event{ID: "e2", After: "e1"}
		replay := *deleteM1
		events <- &replay
		close(events)
		<-applied

		if _, _, err := inbox.db.FromApiID("m1"); !errors.Is(err, database.ErrNotFound) {
			t.Errorf("FromApiID(m1) = %v after the replay, want ErrNotFound", err)
		}
		if id, _, err := u.db.LastEventID(); err != nil || id != "e1" {
			t.Errorf("stored last event ID = %q, %v; want e1 (e2 was dropped)", id, err)
		}
	})
}

// An event that fails on the network, a rate limit or a server error is
// retried, as proton-bridge does, not taken for a broken database: its ID
// isn't stored, the events are rewound to the page before it, and nothing
// is marked for a rebuild.
// evidence: witnesses
func TestEventFailureRetried(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"rate limit", http.StatusTooManyRequests},
		{"server error", http.StatusInternalServerError},
		{"bad gateway", http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				setBubbleClock()
				api := &fakeMetadata{t: t, quiet: true, status: tc.status}
				u, _, updates, rw := newDownloadTestUser(t, api)
				if _, err := u.db.SetSyncDone(); err != nil {
					t.Fatal(err)
				}
				events := make(chan *protonmail.Event, 2)
				events <- &protonmail.Event{ID: "e1", After: "e0"}
				events <- &protonmail.Event{ID: "e2", After: "e1", Messages: []*protonmail.EventMessage{
					{ID: "unknown", Action: protonmail.EventUpdateFlags, Updated: &protonmail.EventMessageUpdate{Unread: new(0)}},
				}}
				close(events)
				u.receiveEvents(updates, events)

				if id, _, err := u.db.LastEventID(); err != nil || id != "e1" {
					t.Errorf("stored last event ID = %q, %v; want e1", id, err)
				}
				if rebuild, err := u.db.NeedsRebuild(); err != nil || rebuild {
					t.Errorf("NeedsRebuild() = %v, %v; want false", rebuild, err)
				}
				rw.mu.Lock()
				defer rw.mu.Unlock()
				if !slices.Equal(rw.rewinds, []string{"e1"}) {
					t.Errorf("rewound to %v, want [e1]", rw.rewinds)
				}
			})
		})
	}
}

// A download stopped after a page (peroxide stopped, the last client gone)
// resumes from the page after it, with no message listed twice and UIDs kept.
func TestDownloadResumes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		setBubbleClock()
		msgs := []*protonmail.Message{inboxMessage("m4", 0), inboxMessage("m3", 0), inboxMessage("m2", 0), inboxMessage("m1", 0)}
		stopped := make(chan struct{})
		api := &fakeMetadata{t: t, msgs: msgs}
		u, inbox, _, _ := newDownloadTestUser(t, api)
		api.hook = func(page int) {
			if page == 2 {
				close(u.done) // the last client logs out
				close(stopped)
			}
		}
		u.startDownload()
		<-stopped
		u.downloads.Wait()
		first := uidsByID(t, inbox)

		// The next login.
		u.done = make(chan struct{})
		api.hook = nil
		u.startDownload()
		u.downloads.Wait()

		want := map[string]uint32{"m4": 1, "m3": 2, "m2": 3, "m1": 4}
		if got := uidsByID(t, inbox); !maps.Equal(got, want) {
			t.Errorf("UIDs = %v after resuming (%v before), want %v", got, first, want)
		}
		// The request in flight when the client left is aborted, its page
		// not stored.
		if wantFirst := map[string]uint32{"m4": 1, "m3": 2}; !maps.Equal(first, wantFirst) {
			t.Errorf("UIDs before resuming = %v, want %v", first, wantFirst)
		}
	})
}

// A Refresh, or a message event that can't be applied for a reason a retry
// won't cure, is not stored as the last event: the database is marked for a
// rebuild, the download stops and the account's clients get BYE. The pages
// that follow are not applied.
// evidence: witnesses
func TestEventsAskForRebuild(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event *protonmail.Event
	}{
		{"refresh", &protonmail.Event{ID: "e2", Refresh: protonmail.EventRefreshMail}},
		// After the download, a message the database lacks must be fetched;
		// the fake API refuses it with a 422, which no retry cures.
		{"refused update", &protonmail.Event{ID: "e2", Messages: []*protonmail.EventMessage{
			{ID: "unknown", Action: protonmail.EventUpdateFlags, Updated: &protonmail.EventMessageUpdate{Unread: new(0)}},
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				setBubbleClock()
				api := &fakeMetadata{t: t, quiet: true, status: http.StatusUnprocessableEntity}
				u, inbox, _, _ := newDownloadTestUser(t, api)
				if _, err := u.db.SetSyncDone(); err != nil {
					t.Fatal(err)
				}
				var sent []imapbackend.Update
				updates := make(chan imapbackend.Update)
				done := make(chan struct{})
				go func() {
					defer close(done)
					for up := range updates {
						sent = append(sent, up)
						close(up.Done())
					}
				}()

				events := make(chan *protonmail.Event, 3)
				events <- &protonmail.Event{ID: "e1"}
				events <- tc.event
				events <- &protonmail.Event{ID: "e3", Messages: []*protonmail.EventMessage{
					{ID: "late", Action: protonmail.EventCreate, Created: inboxMessage("late", 0)},
				}}
				close(events)
				u.receiveEvents(updates, events)
				close(updates)
				<-done

				if id, _, err := u.db.LastEventID(); err != nil || id != "e1" {
					t.Errorf("stored last event ID = %q, %v; want e1", id, err)
				}
				if rebuild, err := u.db.NeedsRebuild(); err != nil || !rebuild {
					t.Errorf("NeedsRebuild() = %v, %v; want true", rebuild, err)
				}
				if _, _, err := inbox.db.FromApiID("late"); err == nil {
					t.Error("a page after the rebuild was asked for was applied")
				}
				bye := false
				for _, up := range sent {
					if s, ok := up.(*imapbackend.StatusUpdate); ok && s.Type == imap.StatusRespBye && s.Username() == "bob" {
						bye = true
					}
				}
				if !bye {
					t.Errorf("sent %v, want a BYE to bob's clients", sent)
				}
			})
		})
	}
}

// On a first login (no event ID stored), the latest event ID is asked for
// before the labels are listed (and the download starts), so a change made
// meanwhile comes as an event instead of being lost; it is stored once they
// are loaded. A stored ID is resumed from, with no request for it. A database
// marked for a rebuild is wiped first, and so starts as a first login.
// evidence: witnesses
func TestInitSessionAsksLatestEventFirst(t *testing.T) {
	for _, tc := range []struct {
		name         string
		stored       string
		rebuild      bool
		wantID       string
		wantRequests []string
	}{
		{"first login", "", false, "latest1", []string{"/events/latest", "/labels"}},
		{"resumed", "e7", false, "e7", []string{"/labels"}},
		{"rebuild", "e7", true, "latest1", []string{"/events/latest", "/labels"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			if tc.stored != "" {
				if err := db.SetLastEventID(tc.stored); err != nil {
					t.Fatal(err)
				}
			}
			if tc.rebuild {
				if err := db.SetNeedsRebuild(); err != nil {
					t.Fatal(err)
				}
			}
			var requests []string
			bodies := map[string]string{
				"/events/latest": `{"Code":1000,"EventID":"latest1","More":0}`,
				"/labels":        `{"Code":1000,"Labels":[]}`,
			}
			u := &user{db: db, u: &protonmail.User{Name: "bob"}, syncSem: make(chan struct{}, 1), sendSem: make(chan struct{}, 1)}
			u.c = &protonmail.Client{RootURL: "https://api.test", HTTPClient: &http.Client{
				Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					requests = append(requests, r.URL.Path)
					if r.URL.Path == "/labels" {
						// The ID asked for first must not be stored yet: a
						// failed listing asks for it again.
						if id, ok, err := db.LastEventID(); err != nil || (ok && (tc.rebuild || id != tc.stored)) {
							t.Errorf("while listing, stored event ID = %q, %v, %v", id, ok, err)
						}
					}
					body, ok := bodies[r.URL.Path]
					if !ok {
						t.Errorf("unexpected request %v %v", r.Method, r.URL.Path)
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": {"application/json"}},
						Body:       io.NopCloser(strings.NewReader(body)),
						Request:    r,
					}, nil
				}),
			}}

			id, err := u.initSession()
			if err != nil {
				t.Fatalf("initSession() = %v", err)
			}
			if id != tc.wantID {
				t.Errorf("initSession() = %q, want %q", id, tc.wantID)
			}
			if !slices.Equal(requests, tc.wantRequests) {
				t.Errorf("requests = %v, want %v", requests, tc.wantRequests)
			}
			if got, ok, err := db.LastEventID(); err != nil || !ok || got != tc.wantID {
				t.Errorf("stored event ID = %q, %v, %v, want %q", got, ok, err, tc.wantID)
			}
			if rebuild, err := db.NeedsRebuild(); err != nil || rebuild {
				t.Errorf("NeedsRebuild() = %v, %v after the login; want false", rebuild, err)
			}
		})
	}
}
