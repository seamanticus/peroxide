package imap

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/emersion/go-imap"
	imapbackend "github.com/emersion/go-imap/backend"

	"github.com/seamanticus/peroxide/protonmail"
)

// newLabelsTestUser is a user with INBOX, a folder Folders/Work (f, in
// nothing, color #abc) holding Folders/Work/Old (o), and a label Labels/Todo
// (l), whose API answers /core/v4/labels as Proton would: a POST makes label
// "n1", "n2"... with the name, type and parent asked for, a PUT echoes the
// change, a DELETE succeeds. Requests made are api.madeRequests().
func newLabelsTestUser(t *testing.T) (*user, *fakeAPI, func() []imapbackend.Update) {
	t.Helper()
	api := &fakeAPI{t: t}
	created := 0
	api.labels = func(method, path string, body map[string]any) (int, string) {
		str := func(k string) string { s, _ := body[k].(string); return s }
		switch method {
		case http.MethodPost:
			created++
			typ, _ := body["Type"].(float64)
			return http.StatusOK, fmt.Sprintf(`{"Code":1000,"Label":{"ID":"n%d","Name":%q,"Color":%q,"Type":%d,"ParentID":%q}}`,
				created, str("Name"), str("Color"), int(typ), str("ParentID"))
		case http.MethodPut:
			id := strings.TrimPrefix(path, "/core/v4/labels/")
			return http.StatusOK, fmt.Sprintf(`{"Code":1000,"Label":{"ID":%q,"Name":%q,"Color":%q,"Type":1,"ParentID":%q}}`,
				id, str("Name"), str("Color"), str("ParentID"))
		default:
			return http.StatusOK, `{"Code":1000}`
		}
	}
	inbox, sent := newActionTestMailbox(t, api, nil)
	u := inbox.u
	u.mailboxes = map[string]*mailbox{protonmail.LabelInbox: inbox}
	if err := u.setMailboxesFrom([]*protonmail.Label{
		{ID: "f", Name: "Work", Color: "#abc", Type: protonmail.LabelMessage, Exclusive: 1},
		{ID: "o", Name: "Old", ParentID: "f", Type: protonmail.LabelMessage, Exclusive: 1},
		{ID: "l", Name: "Todo", Type: protonmail.LabelMessage},
	}); err != nil {
		t.Fatal(err)
	}
	return u, api, sent
}

// setMailboxesFrom adds folder and label mailboxes to those u has, as
// createLabel adds them from events.
func (u *user) setMailboxesFrom(labels []*protonmail.Label) error {
	for _, l := range labels {
		u.createLabel(l)
		if u.getMailboxByLabel(l.ID) == nil {
			return fmt.Errorf("label %v made no mailbox", l.ID)
		}
	}
	return nil
}

// CREATE makes the folders and labels Proton accepts, missing parents first,
// with bridge's color and Type, and lists them before any event; a mailbox
// there is, aerc's CREATE before every APPEND, and a name outside Folders and
// Labels make no request.
// evidence: witnesses
func TestCreateMailbox(t *testing.T) {
	for _, tc := range []struct {
		name         string
		create       string
		wantErr      error  // checked with errors.Is, if set
		wantErrIn    string // or a part of the message, if set
		wantRequests []string
		wantNames    []string // mailboxes then there, beside the setup's
	}{
		{"folder", "Folders/New", nil, "", []string{"POST /core/v4/labels"}, []string{"Folders/New"}},
		{"trailing delimiter", "Folders/New/", nil, "", []string{"POST /core/v4/labels"}, []string{"Folders/New"}},
		{"in an existing folder", "Folders/Work/2026", nil, "", []string{"POST /core/v4/labels"}, []string{"Folders/Work/2026"}},
		{"missing parents", "Folders/A/B", nil, "", []string{"POST /core/v4/labels", "POST /core/v4/labels"}, []string{"Folders/A", "Folders/A/B"}},
		{"slash in a name", "Folders/a\u2215b", nil, "", []string{"POST /core/v4/labels"}, []string{"Folders/a\u2215b"}},
		{"label", "Labels/Later", nil, "", []string{"POST /core/v4/labels"}, []string{"Labels/Later"}},
		{"existing system mailbox", "INBOX", imapbackend.ErrMailboxAlreadyExists, "", nil, nil},
		{"existing folder", "Folders/Work", imapbackend.ErrMailboxAlreadyExists, "", nil, nil},
		{"placeholder", "Labels", imapbackend.ErrMailboxAlreadyExists, "", nil, nil},
		{"outside the prefixes", "Projects", nil, "not in Folders or Labels", nil, nil},
		{"system mailbox child", "Archive/2026", nil, "not in Folders or Labels", nil, nil},
		{"nested label", "Labels/a/b", nil, "can't be nested", nil, nil},
		{"empty level", "Folders//x", nil, "empty level", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, api, _ := newLabelsTestUser(t)
			before := mailboxNameSet(u)
			err := u.CreateMailbox(tc.create)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("CreateMailbox(%q) = %v, want %v", tc.create, err, tc.wantErr)
				}
			case tc.wantErrIn != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErrIn) {
					t.Errorf("CreateMailbox(%q) = %v, want an error with %q", tc.create, err, tc.wantErrIn)
				}
			case err != nil:
				t.Fatalf("CreateMailbox(%q) = %v", tc.create, err)
			}
			if got := api.madeRequests(); !slices.Equal(got, tc.wantRequests) {
				t.Errorf("made requests %q, want %q", got, tc.wantRequests)
			}
			var added []string
			for _, name := range mailboxNameSet(u) {
				if !slices.Contains(before, name) {
					added = append(added, name)
				}
			}
			if !slices.Equal(added, tc.wantNames) {
				t.Errorf("added mailboxes %q, want %q", added, tc.wantNames)
			}
			for _, name := range added {
				mbox := u.getMailbox(name)
				kind, color, _ := u.labelOf(mbox)
				if color != protonmail.NewLabelColor || (kind == kindFolder) != strings.HasPrefix(name, "Folders/") {
					t.Errorf("%v: kind %v, color %q", name, kind, color)
				}
			}
		})
	}
}

// The Labels event that follows a CREATE keeps the mailbox CREATE made, which
// a client may have selected.
// evidence: witnesses
func TestCreateMailboxThenEvent(t *testing.T) {
	u, _, _ := newLabelsTestUser(t)
	if err := u.CreateMailbox("Folders/New"); err != nil {
		t.Fatal(err)
	}
	made := u.getMailbox("Folders/New")
	u.handleLabelEvent(&protonmail.EventLabel{ID: made.label, Action: protonmail.EventCreate,
		Label: &protonmail.Label{ID: made.label, Name: "New", Type: protonmail.LabelMessage, Exclusive: 1}})
	if got := u.getMailbox("Folders/New"); got != made {
		t.Errorf("after the event, Folders/New is %p, want the one CREATE made, %p", got, made)
	}
}

// DELETE deletes folders and labels in Proton and removes their mailboxes at
// once, a folder's subfolders with it, with a BYE for each, for the clients
// that have it selected; system mailboxes, the placeholders and a mailbox
// there isn't make no request.
// evidence: witnesses
func TestDeleteMailbox(t *testing.T) {
	for _, tc := range []struct {
		name, mailbox string
		wantErrIn     string
		wantRequests  []string
		wantByes      []string // also the mailboxes gone
	}{
		{"folder", "Folders/Work/Old", "", []string{"DELETE /core/v4/labels/o"}, []string{"Folders/Work/Old"}},
		// Proton deletes the folders inside too; their events come later.
		{"folder with a subfolder", "Folders/Work", "", []string{"DELETE /core/v4/labels/f"}, []string{"Folders/Work", "Folders/Work/Old"}},
		{"label", "Labels/Todo", "", []string{"DELETE /core/v4/labels/l"}, []string{"Labels/Todo"}},
		{"INBOX", "INBOX", "peroxide's own", nil, nil},
		{"placeholder", "Folders", "peroxide's own", nil, nil},
		{"missing", "Folders/Nope", imapbackend.ErrNoSuchMailbox.Error(), nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, api, sent := newLabelsTestUser(t)
			before := mailboxNameSet(u)
			err := u.DeleteMailbox(tc.mailbox)
			if tc.wantErrIn != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrIn) {
					t.Errorf("DeleteMailbox(%q) = %v, want an error with %q", tc.mailbox, err, tc.wantErrIn)
				}
			} else if err != nil {
				t.Fatalf("DeleteMailbox(%q) = %v", tc.mailbox, err)
			}
			if got := api.madeRequests(); !slices.Equal(got, tc.wantRequests) {
				t.Errorf("made requests %q, want %q", got, tc.wantRequests)
			}
			var gone []string
			for _, name := range before {
				if u.getMailbox(name) == nil {
					gone = append(gone, name)
				}
			}
			if !slices.Equal(gone, tc.wantByes) {
				t.Errorf("mailboxes gone %q, want %q", gone, tc.wantByes)
			}
			var byes []string
			for _, up := range sent() {
				if s, ok := up.(*imapbackend.StatusUpdate); ok && s.Type == imap.StatusRespBye {
					byes = append(byes, s.Mailbox())
				}
			}
			if !slices.Equal(byes, tc.wantByes) {
				t.Errorf("BYE sent for %q, want %q", byes, tc.wantByes)
			}
		})
	}
}

// RENAME sends the new name, the mailbox's own color and, for a folder, its
// new parent, created if missing, or the top; it applies Proton's answer at
// once, the folders inside following; it refuses system mailboxes, a kind
// change, a name taken and a folder moved into itself, before any request.
// evidence: witnesses
func TestRenameMailbox(t *testing.T) {
	for _, tc := range []struct {
		name, from, to string
		wantErrIn      string
		wantRequests   []string
		wantNames      []string // the folders and labels after, sorted
	}{
		{"rename folder", "Folders/Work", "Folders/Job", "",
			[]string{"PUT /core/v4/labels/f"},
			[]string{"Folders/Job", "Folders/Job/Old", "Labels/Todo"}},
		{"rename subfolder", "Folders/Work/Old", "Folders/Work/Older", "",
			[]string{"PUT /core/v4/labels/o"},
			[]string{"Folders/Work", "Folders/Work/Older", "Labels/Todo"}},
		{"into a new folder", "Folders/Work", "Folders/Archive/Work", "",
			[]string{"POST /core/v4/labels", "PUT /core/v4/labels/f"},
			[]string{"Folders/Archive", "Folders/Archive/Work", "Folders/Archive/Work/Old", "Labels/Todo"}},
		{"rename label", "Labels/Todo", "Labels/Done", "",
			[]string{"PUT /core/v4/labels/l"},
			[]string{"Folders/Work", "Folders/Work/Old", "Labels/Done"}},
		{"to the top", "Folders/Work/Old", "Folders/Old", "",
			[]string{"PUT /core/v4/labels/o"},
			[]string{"Folders/Old", "Folders/Work", "Labels/Todo"}},
		{"into itself", "Folders/Work", "Folders/Work/Old/Work", "into itself", nil, nil},
		{"folder to label", "Folders/Work", "Labels/Work", "stays a folder", nil, nil},
		{"taken", "Labels/Todo", "Folders/Work", imapbackend.ErrMailboxAlreadyExists.Error(), nil, nil},
		{"INBOX", "INBOX", "Folders/Inbox", "peroxide's own", nil, nil},
		{"missing", "Folders/Nope", "Folders/Yes", imapbackend.ErrNoSuchMailbox.Error(), nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, api, _ := newLabelsTestUser(t)
			err := u.RenameMailbox(tc.from, tc.to)
			if tc.wantErrIn != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrIn) {
					t.Errorf("RenameMailbox(%q, %q) = %v, want an error with %q", tc.from, tc.to, err, tc.wantErrIn)
				}
			} else if err != nil {
				t.Fatalf("RenameMailbox(%q, %q) = %v", tc.from, tc.to, err)
			}
			if got := api.madeRequests(); !slices.Equal(got, tc.wantRequests) {
				t.Errorf("made requests %q, want %q", got, tc.wantRequests)
			}
			if tc.wantNames == nil {
				return
			}
			var got []string
			for _, name := range mailboxNameSet(u) {
				if name != "INBOX" {
					got = append(got, name)
				}
			}
			if !slices.Equal(got, tc.wantNames) {
				t.Errorf("mailboxes %q, want %q", got, tc.wantNames)
			}
			if kind, color, _ := u.labelOf(u.getMailbox(tc.to)); tc.from == "Folders/Work" && (kind != kindFolder || color != "#abc") {
				t.Errorf("%v: kind %v, color %q, want a folder keeping #abc", tc.to, kind, color)
			}
		})
	}
}

// mailboxNameSet lists u's mailbox names, sorted.
func mailboxNameSet(u *user) []string {
	var names []string
	for _, name := range mailboxNames(u) {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
