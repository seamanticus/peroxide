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
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/emersion/go-imap"
	imapbackend "github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/backend/backendutil"
	"github.com/emersion/go-imap/responses"
	imapserver "github.com/emersion/go-imap/server"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"

	"github.com/seamanticus/peroxide/imap/database"
	"github.com/seamanticus/peroxide/protonmail"
)

// fakeAPI answers message actions with a result per message: codes holds the
// code of each message, 1000 when missing. gets answers GET requests by path,
// with a status and a body. labelIDs gives the labels of each message for the
// metadata request (POST /mail/v4/messages overridden as GET). It records the
// requests made.
type fakeAPI struct {
	t        *testing.T
	codes    map[string]int
	gets     map[string]fakeResponse
	labelIDs map[string][]string
	// actionCodes and actionStatus override codes for one action, keyed by
	// path and label ID, e.g. "/messages/unlabel 0" (the label ID is empty
	// for actions without one): the code per message, or an HTTP status that
	// fails the whole request.
	actionCodes  map[string]map[string]int
	actionStatus map[string]int
	// metadataStatus, if set, fails the metadata request with that status.
	metadataStatus int

	mu       sync.Mutex
	requests []string
}

type fakeResponse struct {
	status      int
	contentType string
	body        string
}

func (api *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	api.mu.Lock()
	api.requests = append(api.requests, r.Method+" "+r.URL.Path)
	api.mu.Unlock()

	if res, ok := api.gets[r.URL.Path]; ok && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", res.contentType)
		w.WriteHeader(res.status)
		io.WriteString(w, res.body)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/mail/v4/messages" {
		api.serveMetadata(w, r)
		return
	}
	if r.Method != http.MethodPut || !strings.HasPrefix(r.URL.Path, "/messages/") {
		api.t.Errorf("unexpected request %v %v", r.Method, r.URL.Path)
		http.NotFound(w, r)
		return
	}
	var req struct {
		IDs     []string
		LabelID string
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.t.Errorf("decode request: %v", err)
	}
	action := r.URL.Path + " " + req.LabelID
	if status, ok := api.actionStatus[action]; ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, `{"Code":2001,"Error":"failed"}`)
		return
	}

	var results []string
	for _, id := range req.IDs {
		code, ok := api.actionCodes[action][id]
		if !ok {
			code, ok = api.codes[id]
		}
		if !ok {
			code = 1000
		}
		res := fmt.Sprintf(`{"Code":%v}`, code)
		if code != 1000 {
			res = fmt.Sprintf(`{"Code":%v,"Details":[],"Error":"failed"}`, code)
		}
		results = append(results, fmt.Sprintf(`{"ID":%q,"Response":%v}`, id, res))
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"Code":1001,"Responses":[`+strings.Join(results, ",")+`]}`)
}

func (api *fakeAPI) madeRequests() []string {
	api.mu.Lock()
	defer api.mu.Unlock()
	return append([]string(nil), api.requests...)
}

// serveMetadata answers the metadata of the messages asked for by ID, those
// in labelIDs.
func (api *fakeAPI) serveMetadata(w http.ResponseWriter, r *http.Request) {
	if api.metadataStatus != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(api.metadataStatus)
		io.WriteString(w, `{"Code":2001,"Error":"failed"}`)
		return
	}
	if got := r.Header.Get("X-HTTP-Method-Override"); got != http.MethodGet {
		api.t.Errorf("metadata request overridden as %q, want GET", got)
	}
	var req struct{ ID []string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.t.Errorf("decode metadata request: %v", err)
	}
	var msgs []*protonmail.Message
	for _, id := range req.ID {
		if labelIDs, ok := api.labelIDs[id]; ok {
			msgs = append(msgs, &protonmail.Message{ID: id, LabelIDs: labelIDs})
		}
	}
	b, err := json.Marshal(struct {
		Code     int
		Messages []*protonmail.Message
	}{1000, msgs})
	if err != nil {
		api.t.Errorf("encode metadata response: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

// An update made before its folder is renamed in Proton names the mailbox as
// it is when go-imap delivers it, which is when go-imap matches it to the
// clients that have it selected: the EXISTS isn't lost to the rename.
func TestUpdateFollowsRename(t *testing.T) {
	mbox, _ := newTestMailbox(t, nil)
	mbox.setName("Work")
	up := newMailboxUpdate("bob", mbox)
	mbox.setName("Projects")
	if got := up.Mailbox(); got != "Projects" {
		t.Errorf("Mailbox() = %q after the rename, want Projects", got)
	}
	if got := up.Username(); got != "bob" {
		t.Errorf("Username() = %q, want bob", got)
	}
}

// newActionTestMailbox returns INBOX holding msgs, whose user talks to api,
// and the updates sent to clients. Updates are acknowledged as the IMAP server
// does once they are written. The buckets of extraLabels are made first, so
// that msgs labelled with them are filed there too.
func newActionTestMailbox(t *testing.T, api *fakeAPI, msgs []*protonmail.Message, extraLabels ...string) (*mailbox, func() []imapbackend.Update) {
	t.Helper()
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	mbox, _ := newTestMailbox(t, msgs, extraLabels...)
	u := mbox.u
	u.c = &protonmail.Client{RootURL: srv.URL}
	u.mailboxes = map[string]*mailbox{protonmail.LabelInbox: mbox}
	u.backend = &backend{updates: make(chan imapbackend.Update)}

	var mu sync.Mutex
	var sent []imapbackend.Update
	done := make(chan struct{})
	go func() {
		defer close(done)
		for update := range u.backend.updates {
			mu.Lock()
			sent = append(sent, update)
			mu.Unlock()
			close(update.Done())
		}
	}()
	t.Cleanup(func() {
		close(u.backend.updates)
		<-done
	})

	return mbox, func() []imapbackend.Update {
		mu.Lock()
		defer mu.Unlock()
		return append([]imapbackend.Update(nil), sent...)
	}
}

func uidSet(uids ...uint32) *imap.SeqSet {
	s := new(imap.SeqSet)
	s.AddNum(uids...)
	return s
}

// A read toggle is applied without waiting for Proton's event, and clients are
// told with a FETCH bearing the UID.
func TestStoreSeenAppliedWithoutEvent(t *testing.T) {
	api := &fakeAPI{t: t}
	mbox, sent := newActionTestMailbox(t, api, []*protonmail.Message{inboxMessage("a", 0), inboxMessage("b", 1)})

	if err := mbox.UpdateMessagesFlags(true, uidSet(2), imap.AddFlags, []string{imap.SeenFlag}); err != nil {
		t.Fatalf("UpdateMessagesFlags() = %v", err)
	}

	checkStatus(t, mbox, 2, 0)
	if got, want := api.madeRequests(), []string{"PUT /messages/read"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("requests = %v, want %v", got, want)
	}

	updates := sent()
	if len(updates) != 1 {
		t.Fatalf("sent %v updates, want 1", len(updates))
	}
	up, ok := updates[0].(*imapbackend.MessageUpdate)
	if !ok {
		t.Fatalf("sent %T, want a message update", updates[0])
	}
	if up.Message.SeqNum != 2 || up.Message.Uid != 2 || len(up.Message.Flags) != 1 || up.Message.Flags[0] != imap.SeenFlag {
		t.Errorf("sent FETCH %v UID %v FLAGS %v, want 2 UID 2 FLAGS [\\Seen]", up.Message.SeqNum, up.Message.Uid, up.Message.Flags)
	}
	if _, ok := up.Message.Items[imap.FetchUid]; !ok {
		t.Error("sent FETCH doesn't hold the UID item")
	}
}

// A flag change on a message is told to every mailbox holding it: All Mail
// holds it too once stored, so nothing is logged (TODO.md section 8 logged
// "message not found" for a mailbox no client had opened).
func TestStoreTellsEveryMailbox(t *testing.T) {
	msgs := []*protonmail.Message{
		{ID: "a", Unread: 1, LabelIDs: []string{protonmail.LabelInbox, protonmail.LabelAllMail}},
	}
	mbox, sent := newActionTestMailbox(t, &fakeAPI{t: t}, msgs, protonmail.LabelAllMail)
	u := mbox.u
	allMailDB, err := u.db.Mailbox(protonmail.LabelAllMail)
	if err != nil {
		t.Fatalf("db.Mailbox() = %v", err)
	}
	u.mailboxes[protonmail.LabelAllMail] = &mailbox{name: "All Mail", label: protonmail.LabelAllMail, u: u, db: allMailDB, deleted: make(map[string]struct{})}

	var logs bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldWriter)

	if err := mbox.UpdateMessagesFlags(true, uidSet(1), imap.AddFlags, []string{imap.SeenFlag}); err != nil {
		t.Fatalf("UpdateMessagesFlags() = %v", err)
	}

	if logs.Len() != 0 {
		t.Errorf("logged %q, want nothing", logs.String())
	}
	var names []string
	for _, up := range sent() {
		if _, ok := up.(*imapbackend.MessageUpdate); !ok {
			t.Errorf("sent %T, want message updates only", up)
		}
		names = append(names, up.Mailbox())
	}
	slices.Sort(names)
	if want := []string{"All Mail", imap.InboxName}; !slices.Equal(names, want) {
		t.Errorf("sent message updates for %v, want %v", names, want)
	}
}

// LIST shows All Mail only with ShowAllMail, as proton-bridge does. Hidden, it
// is still a mailbox: SELECT and STATUS find it by name.
// evidence: witnesses
func TestListShowAllMail(t *testing.T) {
	for _, show := range []bool{false, true} {
		t.Run(fmt.Sprint(show), func(t *testing.T) {
			inbox, _ := newTestMailbox(t, nil)
			u := inbox.u
			u.backend = &backend{showAllMail: show}
			allMail := &mailbox{name: "All Mail", label: protonmail.LabelAllMail, u: u}
			u.mailboxes = map[string]*mailbox{protonmail.LabelInbox: inbox, protonmail.LabelAllMail: allMail}

			list, err := u.ListMailboxes(false)
			if err != nil {
				t.Fatalf("ListMailboxes() = %v", err)
			}
			var names []string
			for _, mbox := range list {
				names = append(names, mbox.Name())
			}
			slices.Sort(names)
			want := []string{imap.InboxName}
			if show {
				want = []string{"All Mail", imap.InboxName}
			}
			if !slices.Equal(names, want) {
				t.Errorf("LIST = %v, want %v", names, want)
			}

			if got, err := u.GetMailbox("All Mail"); err != nil || got != allMail {
				t.Errorf("GetMailbox(All Mail) = %v, %v; want the All Mail mailbox", got, err)
			}
		})
	}
}

// STORE \Deleted without .SILENT must answer with the new flags (RFC 3501
// section 6.4.6): go-imap only sends that FETCH itself for backends without
// updates. \Deleted is peroxide's own, so no API request is made.
// evidence: witnesses
func TestStoreDeletedSendsFlags(t *testing.T) {
	tests := []struct {
		name      string
		op        imap.FlagsOp
		wantFlags []string
	}{
		{"+FLAGS", imap.AddFlags, []string{imap.SeenFlag, imap.DeletedFlag}},
		{"-FLAGS", imap.RemoveFlags, []string{imap.SeenFlag}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeAPI{t: t}
			mbox, sent := newActionTestMailbox(t, api, []*protonmail.Message{inboxMessage("a", 1), inboxMessage("b", 0)})
			if tt.op == imap.RemoveFlags {
				mbox.deleted["b"] = struct{}{}
			}

			if err := mbox.UpdateMessagesFlags(true, uidSet(2), tt.op, []string{imap.DeletedFlag}); err != nil {
				t.Fatalf("UpdateMessagesFlags() = %v", err)
			}
			if got := api.madeRequests(); len(got) != 0 {
				t.Errorf("made requests %q, want none", got)
			}
			updates := sent()
			if len(updates) != 1 {
				t.Fatalf("sent %v updates, want 1", len(updates))
			}
			up, ok := updates[0].(*imapbackend.MessageUpdate)
			if !ok {
				t.Fatalf("sent %T, want a message update", updates[0])
			}
			if up.Message.SeqNum != 2 || up.Message.Uid != 2 || !slices.Equal(up.Message.Flags, tt.wantFlags) {
				t.Errorf("sent FETCH %v UID %v FLAGS %v, want 2 UID 2 FLAGS %v", up.Message.SeqNum, up.Message.Uid, up.Message.Flags, tt.wantFlags)
			}
			if _, ok := up.Message.Items[imap.FetchUid]; !ok {
				t.Error("sent FETCH doesn't hold the UID item")
			}
			if up.Mailbox() != imap.InboxName {
				t.Errorf("sent update for mailbox %q, want %q", up.Mailbox(), imap.InboxName)
			}
		})
	}
}

// STORE FLAGS replaces the flags (RFC 3501 section 6.4.6): flags it doesn't
// list are removed, from the messages holding them only; flags it lists are
// added; flags with no Proton meaning are left alone.
// evidence: witnesses
func TestStoreSetFlagsReplaces(t *testing.T) {
	const work = "label-work"
	msgs := []*protonmail.Message{
		{ID: "a", Unread: 0, LabelIDs: []string{protonmail.LabelInbox, work}},
		{ID: "b", Unread: 1, LabelIDs: []string{protonmail.LabelInbox}},
	}
	tests := []struct {
		name         string
		flags        []string
		wantRequests []string
		wantFlags    [][]string // by UID, from 1
	}{
		{"empty clears", nil,
			[]string{"PUT /messages/unlabel", "PUT /messages/unread"},
			[][]string{nil, nil}},
		// go-imap lowercases a label flag: it must still count as listed.
		{"keeps listed label", []string{"work"},
			[]string{"PUT /messages/label", "PUT /messages/unread"},
			[][]string{{"Work"}, {"Work"}}},
		{"sets seen and deleted", []string{imap.SeenFlag, imap.DeletedFlag},
			[]string{"PUT /messages/read", "PUT /messages/unlabel"},
			[][]string{{imap.SeenFlag, imap.DeletedFlag}, {imap.SeenFlag, imap.DeletedFlag}}},
		{"answered and draft alone change nothing in Proton", []string{imap.AnsweredFlag, imap.DraftFlag},
			[]string{"PUT /messages/unlabel", "PUT /messages/unread"},
			[][]string{nil, nil}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeAPI{t: t}
			mbox, _ := newActionTestMailbox(t, api, msgs)
			mbox.u.flags = map[string]string{work: "Work"}
			mbox.deleted["b"] = struct{}{}

			if err := mbox.UpdateMessagesFlags(true, uidSet(1, 2), imap.SetFlags, tt.flags); err != nil {
				t.Fatalf("UpdateMessagesFlags() = %v", err)
			}
			if got := api.madeRequests(); !slices.Equal(got, tt.wantRequests) {
				t.Errorf("made requests %q, want %q", got, tt.wantRequests)
			}
			for i, want := range tt.wantFlags {
				uid := uint32(i + 1)
				apiIDs, err := mbox.fromSeqSet(true, uidSet(uid))
				if err != nil || len(apiIDs) != 1 {
					t.Fatalf("fromSeqSet(%v) = %v, %v", uid, apiIDs, err)
				}
				msg, err := mbox.u.db.Message(apiIDs[0])
				if err != nil {
					t.Fatalf("Message(%v) = %v", apiIDs[0], err)
				}
				got := mbox.fetchFlags(msg)
				slices.Sort(got)
				want = slices.Sorted(slices.Values(want))
				if !slices.Equal(got, want) {
					t.Errorf("UID %v flags = %v, want %v", uid, got, want)
				}
			}
		})
	}
}

// Proton reports a message already as asked as "does not exist": clients must
// still learn its flags, or a client toggling flags keeps asking the same.
func TestStoreSeenNothingToDo(t *testing.T) {
	api := &fakeAPI{t: t, codes: map[string]int{"a": protonmail.CodeMessageDoesNotExist}}
	mbox, sent := newActionTestMailbox(t, api, []*protonmail.Message{inboxMessage("a", 1)})

	if err := mbox.UpdateMessagesFlags(true, uidSet(1), imap.RemoveFlags, []string{imap.SeenFlag}); err != nil {
		t.Fatalf("UpdateMessagesFlags() = %v", err)
	}
	checkStatus(t, mbox, 1, 1)
	if len(sent()) != 1 {
		t.Errorf("sent %v updates, want 1", len(sent()))
	}
}

// Messages the action failed for are left alone, and the command fails.
func TestStoreSeenFailure(t *testing.T) {
	api := &fakeAPI{t: t, codes: map[string]int{"b": 2011}}
	mbox, _ := newActionTestMailbox(t, api, []*protonmail.Message{inboxMessage("a", 1), inboxMessage("b", 1)})

	if err := mbox.UpdateMessagesFlags(true, uidSet(1, 2), imap.AddFlags, []string{imap.SeenFlag}); err == nil {
		t.Error("UpdateMessagesFlags() = nil, want an error")
	}
	// a was marked read, b wasn't.
	checkStatus(t, mbox, 2, 1)
}

// MOVE labels with the destination, then unlabels from the source, as
// proton-bridge does by default, and applies both without waiting for an
// event.
func TestMoveAppliedWithoutEvent(t *testing.T) {
	api := &fakeAPI{t: t}
	mbox, _ := newActionTestMailbox(t, api, []*protonmail.Message{inboxMessage("a", 1), inboxMessage("b", 0)})
	archive := addTestMailbox(t, mbox.u, "Archive", protonmail.LabelArchive, nil)

	if err := mbox.MoveMessages(true, uidSet(1), "Archive"); err != nil {
		t.Fatalf("MoveMessages() = %v", err)
	}
	checkStatus(t, mbox, 1, 0)
	checkStatus(t, archive, 1, 1)
	if got, want := api.madeRequests(), []string{"PUT /messages/label", "PUT /messages/unlabel"}; !slices.Equal(got, want) {
		t.Errorf("made requests %q, want %q", got, want)
	}
}

// MOVE refuses, before any request, moves into or out of All Mail and
// Starred, views of messages kept in their folders, and between INBOX and
// Sent. A MOVE into the mailbox itself does nothing.
// evidence: witnesses
func TestMoveRefused(t *testing.T) {
	name := map[string]string{
		protonmail.LabelInbox:   imap.InboxName,
		protonmail.LabelAllMail: "All Mail",
		protonmail.LabelStarred: "Starred",
		protonmail.LabelSent:    "Sent",
		protonmail.LabelArchive: "Archive",
	}
	for _, tc := range []struct {
		name     string
		from, to string
		wantErr  bool
	}{
		{"out of All Mail", protonmail.LabelAllMail, protonmail.LabelArchive, true},
		{"into All Mail", protonmail.LabelArchive, protonmail.LabelAllMail, true},
		{"out of Starred", protonmail.LabelStarred, protonmail.LabelArchive, true},
		{"into Starred", protonmail.LabelInbox, protonmail.LabelStarred, true},
		{"Starred to All Mail", protonmail.LabelStarred, protonmail.LabelAllMail, true},
		{"All Mail to Starred", protonmail.LabelAllMail, protonmail.LabelStarred, true},
		{"INBOX to Sent", protonmail.LabelInbox, protonmail.LabelSent, true},
		{"Sent to INBOX", protonmail.LabelSent, protonmail.LabelInbox, true},
		{"into itself", protonmail.LabelArchive, protonmail.LabelArchive, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeAPI{t: t}
			inbox, _ := newActionTestMailbox(t, api, nil)
			msg := &protonmail.Message{ID: "a", LabelIDs: []string{tc.from, protonmail.LabelAllMail}}
			from := addTestMailbox(t, inbox.u, name[tc.from], tc.from, []*protonmail.Message{msg})
			if tc.to != tc.from {
				addTestMailbox(t, inbox.u, name[tc.to], tc.to, nil)
			}

			err := from.MoveMessages(true, uidSet(1), name[tc.to])
			if (err != nil) != tc.wantErr {
				t.Errorf("MoveMessages() = %v, want error %v", err, tc.wantErr)
			}
			if got := api.madeRequests(); len(got) != 0 {
				t.Errorf("made requests %q, want none", got)
			}
			if !inMailbox(from, "a") {
				t.Error("message left the source")
			}
		})
	}
}

// inMailbox reports whether mbox's local database holds apiID.
func inMailbox(mbox *mailbox, apiID string) bool {
	_, _, err := mbox.db.FromApiID(apiID)
	return err == nil
}

// A MOVE that fails partway leaves no message in both mailboxes nor in
// neither (RFC 6851 section 3.3). A message the label failed for isn't
// unlabelled. One the unlabel answers "does not exist" for (labelling into a
// folder took it out of the source already) is moved. One the unlabel failed
// for is looked up: if Proton shows it out of the source, it is moved;
// otherwise it is taken out of the destination again.
// evidence: witnesses
func TestMovePartialFailure(t *testing.T) {
	var (
		labelArchive   = "/messages/label " + protonmail.LabelArchive
		unlabelInbox   = "/messages/unlabel " + protonmail.LabelInbox
		unlabelArchive = "/messages/unlabel " + protonmail.LabelArchive
		inBoth         = []string{protonmail.LabelInbox, protonmail.LabelArchive, protonmail.LabelAllMail}
	)
	const (
		label    = "PUT /messages/label"
		unlabel  = "PUT /messages/unlabel"
		metadata = "POST /mail/v4/messages"
	)
	for _, tc := range []struct {
		name         string
		api          *fakeAPI
		wantErr      bool
		wantRequests []string
		wantSource   bool
		wantArchive  bool
	}{
		{
			name:         "moved",
			api:          &fakeAPI{},
			wantRequests: []string{label, unlabel},
			wantArchive:  true,
		},
		{
			name:         "label fails",
			api:          &fakeAPI{actionCodes: map[string]map[string]int{labelArchive: {"a": 2011}}},
			wantErr:      true,
			wantRequests: []string{label},
			wantSource:   true,
		},
		{
			name:         "label request fails",
			api:          &fakeAPI{actionStatus: map[string]int{labelArchive: http.StatusUnprocessableEntity}},
			wantErr:      true,
			wantRequests: []string{label},
			wantSource:   true,
		},
		{
			name:         "already in the destination",
			api:          &fakeAPI{actionCodes: map[string]map[string]int{labelArchive: {"a": protonmail.CodeMessageDoesNotExist}}},
			wantRequests: []string{label, unlabel},
			wantArchive:  true,
		},
		{
			name:         "label moved it out of the source",
			api:          &fakeAPI{actionCodes: map[string]map[string]int{unlabelInbox: {"a": protonmail.CodeMessageDoesNotExist}}},
			wantRequests: []string{label, unlabel},
			wantArchive:  true,
		},
		{
			name: "unlabel fails, still in source",
			api: &fakeAPI{
				actionCodes: map[string]map[string]int{unlabelInbox: {"a": 2011}},
				labelIDs:    map[string][]string{"a": inBoth},
			},
			wantErr:      true,
			wantRequests: []string{label, unlabel, metadata, unlabel},
			wantSource:   true,
		},
		{
			name: "unlabel request fails, still in source",
			api: &fakeAPI{
				actionStatus: map[string]int{unlabelInbox: http.StatusUnprocessableEntity},
				labelIDs:     map[string][]string{"a": inBoth},
			},
			wantErr:      true,
			wantRequests: []string{label, unlabel, metadata, unlabel},
			wantSource:   true,
		},
		{
			name: "unlabel fails, out of source anyway",
			api: &fakeAPI{
				actionCodes: map[string]map[string]int{unlabelInbox: {"a": 2011}},
				labelIDs:    map[string][]string{"a": {protonmail.LabelArchive, protonmail.LabelAllMail}},
			},
			wantRequests: []string{label, unlabel, metadata},
			wantArchive:  true,
		},
		{
			name: "undo fails",
			api: &fakeAPI{
				actionCodes: map[string]map[string]int{unlabelInbox: {"a": 2011}, unlabelArchive: {"a": 2011}},
				labelIDs:    map[string][]string{"a": inBoth},
			},
			wantErr:      true,
			wantRequests: []string{label, unlabel, metadata, unlabel},
			// In both, locally as in Proton.
			wantSource:  true,
			wantArchive: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := tc.api
			api.t = t
			source, _ := newActionTestMailbox(t, api, []*protonmail.Message{inboxMessage("a", 0)})
			archive := addTestMailbox(t, source.u, "Archive", protonmail.LabelArchive, nil)

			err := source.MoveMessages(true, uidSet(1), "Archive")
			if (err != nil) != tc.wantErr {
				t.Errorf("MoveMessages() = %v, want error %v", err, tc.wantErr)
			}
			if got := api.madeRequests(); !slices.Equal(got, tc.wantRequests) {
				t.Errorf("made requests %q, want %q", got, tc.wantRequests)
			}
			if got := inMailbox(source, "a"); got != tc.wantSource {
				t.Errorf("in %v = %v, want %v", source.Name(), got, tc.wantSource)
			}
			if got := inMailbox(archive, "a"); got != tc.wantArchive {
				t.Errorf("in Archive = %v, want %v", got, tc.wantArchive)
			}
		})
	}
}

// COPY labels the messages with the destination and keeps them in the source.
// One the label failed for is left out, and the command fails; one already
// labelled ("does not exist") counts as copied.
// evidence: witnesses
func TestCopyMessages(t *testing.T) {
	for _, tc := range []struct {
		name        string
		codes       map[string]int
		wantErr     bool
		wantArchive []string
	}{
		{"copied", nil, false, []string{"a", "b"}},
		{"already there", map[string]int{"b": protonmail.CodeMessageDoesNotExist}, false, []string{"a", "b"}},
		{"one fails", map[string]int{"b": 2011}, true, []string{"a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeAPI{t: t, actionCodes: map[string]map[string]int{"/messages/label " + protonmail.LabelArchive: tc.codes}}
			inbox, sent := newActionTestMailbox(t, api, []*protonmail.Message{inboxMessage("a", 1), inboxMessage("b", 0)})
			archive := addTestMailbox(t, inbox.u, "Archive", protonmail.LabelArchive, nil)

			err := inbox.CopyMessages(true, uidSet(1, 2), "Archive")
			if (err != nil) != tc.wantErr {
				t.Errorf("CopyMessages() = %v, want error %v", err, tc.wantErr)
			}
			if got, want := api.madeRequests(), []string{"PUT /messages/label"}; !slices.Equal(got, want) {
				t.Errorf("made requests %q, want %q", got, want)
			}
			checkStatus(t, inbox, 2, 1)
			for _, id := range []string{"a", "b"} {
				if got, want := inMailbox(archive, id), slices.Contains(tc.wantArchive, id); got != want {
					t.Errorf("%v in Archive = %v, want %v", id, got, want)
				}
			}
			var exists int
			for _, up := range sent() {
				if mu, ok := up.(*imapbackend.MailboxUpdate); ok && mu.Mailbox() == "Archive" {
					exists++
				}
			}
			if exists != len(tc.wantArchive) {
				t.Errorf("sent %v EXISTS for Archive, want %v", exists, len(tc.wantArchive))
			}
		})
	}
}

// From Trash, a message no other mailbox shows is deleted before anything
// else. If the delete, or the lookup of what else holds it, fails, nothing is
// expunged: the message stays in Trash, still marked \Deleted, EXPUNGE
// answers NO, and the next EXPUNGE tries again. It never ends in All Mail
// alone, which clients may not show.
// evidence: witnesses
func TestExpungeFromTrashDeleteFails(t *testing.T) {
	const (
		metadata = "POST /mail/v4/messages"
		del      = "PUT /messages/delete"
	)
	inTrash := map[string][]string{"a": {protonmail.LabelTrash, protonmail.LabelAllMail}}
	for _, tc := range []struct {
		name         string
		api          *fakeAPI
		wantRequests []string
	}{
		{"refused for the message", &fakeAPI{
			actionCodes: map[string]map[string]int{"/messages/delete ": {"a": 2011}},
			labelIDs:    inTrash,
		}, []string{metadata, del}},
		{"request fails", &fakeAPI{
			actionStatus: map[string]int{"/messages/delete ": http.StatusUnprocessableEntity},
			labelIDs:     inTrash,
		}, []string{metadata, del}},
		{"labels unknown", &fakeAPI{metadataStatus: http.StatusBadGateway}, []string{metadata}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := tc.api
			api.t = t
			inbox, sent := newActionTestMailbox(t, api, nil)
			trash := addTestMailbox(t, inbox.u, "Trash", protonmail.LabelTrash, []*protonmail.Message{trashMessage("a")})

			if err := trash.UpdateMessagesFlags(true, uidSet(1), imap.AddFlags, []string{imap.DeletedFlag}); err != nil {
				t.Fatalf("UpdateMessagesFlags() = %v", err)
			}
			stored := len(sent())
			if err := trash.Expunge(); err == nil {
				t.Error("Expunge() = nil, want an error: the message is still in Trash")
			}
			if got := api.madeRequests(); !slices.Equal(got, tc.wantRequests) {
				t.Errorf("made requests %q, want %q", got, tc.wantRequests)
			}
			checkStatus(t, trash, 1, 0)
			if updates := sent()[stored:]; len(updates) != 0 {
				t.Errorf("sent %v updates, want none", len(updates))
			}
			if _, err := inbox.u.db.Message("a"); err != nil {
				t.Errorf("db.Message() = %v, want the message kept", err)
			}
			if flags := trash.fetchFlags(trashMessage("a")); !slices.Contains(flags, imap.DeletedFlag) {
				t.Errorf("flags = %v, want \\Deleted kept for the next EXPUNGE", flags)
			}
		})
	}
}

// Of two messages expunged from Trash, the one whose delete fails stays, and
// the other is gone: EXPUNGE answers NO for the first only.
func TestExpungeFromTrashPartialDelete(t *testing.T) {
	api := &fakeAPI{
		t:           t,
		actionCodes: map[string]map[string]int{"/messages/delete ": {"b": 2011}},
		labelIDs: map[string][]string{
			"a": {protonmail.LabelTrash, protonmail.LabelAllMail},
			"b": {protonmail.LabelTrash, protonmail.LabelAllMail},
		},
	}
	inbox, _ := newActionTestMailbox(t, api, nil)
	trash := addTestMailbox(t, inbox.u, "Trash", protonmail.LabelTrash, []*protonmail.Message{trashMessage("a"), trashMessage("b")})
	if err := trash.UpdateMessagesFlags(true, uidSet(1, 2), imap.AddFlags, []string{imap.DeletedFlag}); err != nil {
		t.Fatalf("UpdateMessagesFlags() = %v", err)
	}

	if err := trash.Expunge(); err == nil {
		t.Error("Expunge() = nil, want an error for b")
	}
	if inMailbox(trash, "a") || !inMailbox(trash, "b") {
		t.Errorf("in Trash: a %v, b %v; want only b", inMailbox(trash, "a"), inMailbox(trash, "b"))
	}
	if _, err := inbox.u.db.Message("a"); !errors.Is(err, database.ErrNotFound) {
		t.Errorf("db.Message(a) = %v, want %v: deleted for good", err, database.ErrNotFound)
	}
}

// EXPUNGE deletes the messages marked deleted once: they are gone after.
func TestExpungeForgetsDeletedMessages(t *testing.T) {
	api := &fakeAPI{t: t}
	mbox, sent := newActionTestMailbox(t, api, []*protonmail.Message{inboxMessage("a", 0), inboxMessage("b", 1)})

	if err := mbox.UpdateMessagesFlags(true, uidSet(1), imap.AddFlags, []string{imap.DeletedFlag}); err != nil {
		t.Fatalf("UpdateMessagesFlags() = %v", err)
	}
	// The STORE sends a FETCH of the new flags: count what EXPUNGE sends.
	stored := len(sent())
	if err := mbox.Expunge(); err != nil {
		t.Fatalf("Expunge() = %v", err)
	}
	checkStatus(t, mbox, 1, 1)
	if updates := sent()[stored:]; len(updates) != 1 {
		t.Errorf("sent %v updates, want 1 EXPUNGE", len(updates))
	} else if up, ok := updates[0].(*imapbackend.ExpungeUpdate); !ok || up.SeqNum != 1 {
		t.Errorf("sent %#v, want EXPUNGE 1", updates[0])
	}

	if err := mbox.Expunge(); err != nil {
		t.Fatalf("second Expunge() = %v", err)
	}
	if got := len(api.madeRequests()); got != 1 {
		t.Errorf("made %v requests, want 1: the second EXPUNGE has nothing to delete", got)
	}
}

// trashMessage is a read message in Trash.
func trashMessage(id string) *protonmail.Message {
	return &protonmail.Message{ID: id, LabelIDs: []string{protonmail.LabelTrash, protonmail.LabelAllMail}}
}

// addTestMailbox gives u a mailbox for labelID, synced with msgs.
func addTestMailbox(t *testing.T, u *user, name, labelID string, msgs []*protonmail.Message) *mailbox {
	t.Helper()
	mboxDB := syncTestMailbox(t, u.db, labelID, msgs)
	mbox := &mailbox{name: name, label: labelID, u: u, db: mboxDB, deleted: make(map[string]struct{})}
	u.mailboxes[labelID] = mbox
	return mbox
}

// EXPUNGE in Trash deletes for good, first, the messages no other mailbox or
// label of the account holds, and only unlabels the others from Trash.
func TestExpungeFromTrash(t *testing.T) {
	const (
		unlabel  = "PUT /messages/unlabel"
		metadata = "POST /mail/v4/messages"
		labels   = "GET /labels"
		del      = "PUT /messages/delete"
	)
	for _, tc := range []struct {
		name string
		// labelIDs are the message's labels in Proton; nil when Proton
		// doesn't have it.
		labelIDs     []string
		listLabels   string // GET /labels body; empty when not expected
		wantRequests []string
		wantDeleted  bool
	}{
		{
			name:         "only in Trash",
			labelIDs:     []string{protonmail.LabelTrash, protonmail.LabelAllMail, protonmail.LabelAllSent, protonmail.LabelAllDraft},
			wantRequests: []string{metadata, del},
			wantDeleted:  true,
		},
		{
			name:         "in Inbox",
			labelIDs:     []string{protonmail.LabelTrash, protonmail.LabelAllMail, protonmail.LabelInbox},
			wantRequests: []string{metadata, unlabel},
		},
		{
			name:         "in a label not known yet",
			labelIDs:     []string{protonmail.LabelTrash, protonmail.LabelAllMail, "x"},
			listLabels:   `{"Code":1000,"Labels":[{"ID":"x","Name":"x","Type":1}]}`,
			wantRequests: []string{metadata, labels, unlabel},
		},
		{
			name:         "in a label not listed",
			labelIDs:     []string{protonmail.LabelTrash, protonmail.LabelAllMail, "x"},
			listLabels:   `{"Code":1000,"Labels":[]}`,
			wantRequests: []string{metadata, labels, del},
			wantDeleted:  true,
		},
		{
			name:         "gone from Proton",
			wantRequests: []string{metadata, unlabel},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeAPI{t: t, labelIDs: map[string][]string{}}
			if tc.labelIDs != nil {
				api.labelIDs["a"] = tc.labelIDs
			}
			if tc.listLabels != "" {
				api.gets = map[string]fakeResponse{"/labels": {http.StatusOK, "application/json", tc.listLabels}}
			}
			inbox, sent := newActionTestMailbox(t, api, nil)
			trash := addTestMailbox(t, inbox.u, "Trash", protonmail.LabelTrash, []*protonmail.Message{trashMessage("a")})

			if err := trash.UpdateMessagesFlags(true, uidSet(1), imap.AddFlags, []string{imap.DeletedFlag}); err != nil {
				t.Fatalf("UpdateMessagesFlags() = %v", err)
			}
			// The STORE sends a FETCH of the new flags: count what EXPUNGE sends.
			stored := len(sent())
			if err := trash.Expunge(); err != nil {
				t.Fatalf("Expunge() = %v", err)
			}
			checkStatus(t, trash, 0, 0)
			if got := api.madeRequests(); !slices.Equal(got, tc.wantRequests) {
				t.Errorf("made requests %q, want %q", got, tc.wantRequests)
			}
			if updates := sent()[stored:]; len(updates) != 1 {
				t.Errorf("sent %v updates, want 1 EXPUNGE", len(updates))
			} else if up, ok := updates[0].(*imapbackend.ExpungeUpdate); !ok || up.SeqNum != 1 {
				t.Errorf("sent %#v, want EXPUNGE 1", updates[0])
			}

			_, err := inbox.u.db.Message("a")
			if tc.wantDeleted && !errors.Is(err, database.ErrNotFound) {
				t.Errorf("db.Message() = %v, want %v: deleted for good", err, database.ErrNotFound)
			} else if !tc.wantDeleted && err != nil {
				t.Errorf("db.Message() = %v, want the message kept", err)
			}
		})
	}
}

// Every message is in All Mail, so EXPUNGE there could only delete them for
// good: it is refused.
func TestExpungeAllMailRefused(t *testing.T) {
	api := &fakeAPI{t: t}
	inbox, _ := newActionTestMailbox(t, api, nil)
	all := addTestMailbox(t, inbox.u, "All Mail", protonmail.LabelAllMail, []*protonmail.Message{trashMessage("a")})

	if err := all.UpdateMessagesFlags(true, uidSet(1), imap.AddFlags, []string{imap.DeletedFlag}); err != nil {
		t.Fatalf("UpdateMessagesFlags() = %v", err)
	}
	if err := all.Expunge(); err == nil {
		t.Error("Expunge() = nil, want an error")
	}
	checkStatus(t, all, 1, 0)
	if got := api.madeRequests(); len(got) != 0 {
		t.Errorf("made requests %q, want none", got)
	}
}

func TestSearchMessages(t *testing.T) {
	// An action test mailbox: STORE \Deleted tells clients through the backend.
	mbox, _ := newActionTestMailbox(t, &fakeAPI{t: t}, []*protonmail.Message{inboxMessage("a", 1), inboxMessage("b", 0), inboxMessage("c", 1)})
	if err := mbox.UpdateMessagesFlags(true, uidSet(3), imap.AddFlags, []string{imap.DeletedFlag}); err != nil {
		t.Fatalf("UpdateMessagesFlags() = %v", err)
	}

	for _, tc := range []struct {
		name     string
		isUID    bool
		criteria *imap.SearchCriteria
		want     []uint32
	}{
		{"all", false, &imap.SearchCriteria{}, []uint32{1, 2, 3}},
		{"unseen", false, &imap.SearchCriteria{WithoutFlags: []string{imap.SeenFlag}}, []uint32{1, 3}},
		{"deleted by UID", true, &imap.SearchCriteria{WithFlags: []string{imap.DeletedFlag}}, []uint32{3}},
		{"unseen, not deleted", false, &imap.SearchCriteria{
			WithoutFlags: []string{imap.SeenFlag},
			Not:          []*imap.SearchCriteria{{WithFlags: []string{imap.DeletedFlag}}},
		}, []uint32{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mbox.SearchMessages(tc.isUID, tc.criteria)
			if err != nil {
				t.Fatalf("SearchMessages() = %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("SearchMessages() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A message the API returns without a Sender has no From, rather than
// crashing the connection that fetches or searches it.
// evidence: witnesses
func TestNoSender(t *testing.T) {
	msg := inboxMessage("a", 0)
	if env := fetchEnvelope(msg); env.From != nil {
		t.Errorf("fetchEnvelope().From = %v, want nil", env.From)
	}
	if h := messageHeader(msg); h.Has("From") {
		t.Errorf("messageHeader() has From %q, want none", h.Get("From"))
	}
}

// RFC 3501 section 7.4.2: Sender and Reply-To default to From, and the
// envelope's Message-Id is the header's, brackets included.
// evidence: witnesses
func TestEnvelopeDefaults(t *testing.T) {
	from := &protonmail.MessageAddress{Name: "Bob", Address: "bob@example.org"}
	replyTo := &protonmail.MessageAddress{Address: "list@example.org"}
	tests := []struct {
		name        string
		replyTos    []*protonmail.MessageAddress
		wantReplyTo string
	}{
		{"no Reply-To", nil, "bob@example.org"},
		{"Reply-To", []*protonmail.MessageAddress{replyTo}, "list@example.org"},
	}
	for _, tt := range tests {
		msg := inboxMessage("a", 0)
		msg.Sender = from
		msg.ReplyTos = tt.replyTos
		msg.ExternalID = "abc@example.org"
		env := fetchEnvelope(msg)
		if len(env.Sender) != 1 || env.Sender[0].Address() != "bob@example.org" {
			t.Errorf("%v: Sender = %v, want From", tt.name, env.Sender)
		}
		if len(env.ReplyTo) != 1 || env.ReplyTo[0].Address() != tt.wantReplyTo {
			t.Errorf("%v: ReplyTo = %v, want %v", tt.name, env.ReplyTo, tt.wantReplyTo)
		}
		if h := messageHeader(msg); env.MessageId != h.Get("Message-Id") || env.MessageId != "<abc@example.org>" {
			t.Errorf("%v: envelope Message-Id %q, header %q, want <abc@example.org>", tt.name, env.MessageId, h.Get("Message-Id"))
		}
	}
}

// A full message's header is Proton's original one, with threading fields
// and the sent Date kept, the metadata's addresses and subject over it, and
// peroxide's own Content-Type. One that can't be parsed is ignored.
// evidence: witnesses
func TestMessageHeaderFromOriginal(t *testing.T) {
	const original = "Return-Path: <bob@example.org>\r\n" +
		"Date: Mon, 02 Nov 2026 10:00:00 +1300\r\n" +
		"From: Old Name <bob@example.org>\r\n" +
		"To: bob@protonmail.com\r\n" +
		"Subject: original subject\r\n" +
		"Message-Id: <orig@example.org>\r\n" +
		"In-Reply-To: <parent@example.org>\r\n" +
		"References: <root@example.org> <parent@example.org>\r\n" +
		"Mime-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n"

	msg := fetchTestMessage("a", 0)
	msg.Sender = &protonmail.MessageAddress{Name: "Bob", Address: "bob@example.org"}
	msg.Subject = "decrypted subject"
	msg.Header = original
	h := messageHeader(msg)
	for k, want := range map[string]string{
		"Return-Path": "<bob@example.org>",
		"Date":        "Mon, 02 Nov 2026 10:00:00 +1300",
		"Message-Id":  "<orig@example.org>",
		"In-Reply-To": "<parent@example.org>",
		"References":  "<root@example.org> <parent@example.org>",
		"Subject":     "decrypted subject",
		"To":          "bob@protonmail.com",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%v = %q, want %q", k, got, want)
		}
	}
	if from, err := (&mail.Header{Header: h}).AddressList("From"); err != nil || len(from) != 1 ||
		from[0].Name != "Bob" || from[0].Address != "bob@example.org" {
		t.Errorf("From = %v, %v, want the metadata's Bob <bob@example.org>", from, err)
	}
	if mt, _, err := h.ContentType(); err != nil || mt != "multipart/mixed" {
		t.Errorf("Content-Type = %q, %v, want multipart/mixed", mt, err)
	}
	if h.Has("Content-Transfer-Encoding") {
		t.Errorf("Content-Transfer-Encoding = %q, want none", h.Get("Content-Transfer-Encoding"))
	}

	msg.Header = " starts with a space\r\n"
	if h := messageHeader(msg); h.Get("Message-Id") != "<"+messageID(msg)+">" || h.Get("Subject") != "decrypted subject" {
		t.Errorf("with a malformed header: Message-Id %q, Subject %q, want the metadata's", h.Get("Message-Id"), h.Get("Subject"))
	}
}

// fetchTestMessage is an INBOX message with a plain body, whose header needs
// no API request.
func fetchTestMessage(id string, unread int) *protonmail.Message {
	msg := inboxMessage(id, unread)
	msg.Sender = &protonmail.MessageAddress{Address: "from@example.org"}
	msg.MIMEType = "text/plain"
	msg.Body = "Hello\r\n"
	return msg
}

// BODY[HEADER.FIELDS (...)] sends only the fields named, and
// HEADER.FIELDS.NOT all but those, matched without case (RFC 3501 section
// 6.4.5). Each equals go-imap's filter, which pass-through uses, applied to
// BODY[HEADER].
// evidence: witnesses
func TestFetchHeaderFields(t *testing.T) {
	api := &fakeAPI{t: t}
	msg := fetchTestMessage("a", 0)
	msg.Subject = "hello"
	msg.Header = "Date: Mon, 02 Nov 2026 10:00:00 +1300\r\n" +
		"Message-Id: <orig@example.org>\r\n" +
		"X-Mailer: test\r\n" +
		"Received: from a\r\n" +
		"Received: from b\r\n"
	mbox, _ := newActionTestMailbox(t, api, []*protonmail.Message{msg})
	cacheFull(mbox, msg)

	const whole = "BODY.PEEK[HEADER]"
	wholeHeader := sectionBytes(t, fetchItems(t, mbox, whole), whole)
	parsed, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(wholeHeader)))
	if err != nil {
		t.Fatalf("read %v: %v", whole, err)
	}

	for _, tc := range []struct {
		item       imap.FetchItem
		wantKeys   []string
		unwantKeys []string
	}{
		{"BODY.PEEK[HEADER.FIELDS (SUBJECT message-id)]", []string{"Subject", "Message-Id"}, []string{"Date", "X-Mailer", "Content-Type"}},
		{"BODY.PEEK[HEADER.FIELDS (RECEIVED)]", []string{"Received"}, []string{"Subject"}},
		{"BODY.PEEK[HEADER.FIELDS (X-NONE)]", nil, []string{"Subject", "Date"}},
		{"BODY.PEEK[HEADER.FIELDS.NOT (subject X-MAILER)]", []string{"Date", "Message-Id", "Content-Type"}, []string{"Subject", "X-Mailer"}},
	} {
		got := sectionBytes(t, fetchItems(t, mbox, tc.item), tc.item)
		h, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(got)))
		if err != nil {
			t.Fatalf("%v: read header: %v", tc.item, err)
		}
		for _, k := range tc.wantKeys {
			if !h.Has(k) {
				t.Errorf("%v: no %v in %q", tc.item, k, got)
			}
		}
		for _, k := range tc.unwantKeys {
			if h.Has(k) {
				t.Errorf("%v: has %v in %q", tc.item, k, got)
			}
		}
		if n := len(h.Values("Received")); slices.Contains(tc.wantKeys, "Received") && n != 2 {
			t.Errorf("%v: %v Received fields, want 2", tc.item, n)
		}

		section, err := imap.ParseBodySectionName(tc.item)
		if err != nil {
			t.Fatal(err)
		}
		l, err := backendutil.FetchBodySection(parsed, strings.NewReader(""), section)
		if err != nil {
			t.Fatalf("%v: backendutil: %v", tc.item, err)
		}
		want, err := io.ReadAll(l)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%v = %q, go-imap's filter gives %q", tc.item, got, want)
		}
	}
}

// cacheFull puts msgs in mbox's full-message cache, as if fetched already, so
// fetching their bodies makes no API request.
func cacheFull(mbox *mailbox, msgs ...*protonmail.Message) {
	for _, msg := range msgs {
		mbox.u.messages.put(msg)
	}
}

// fetchConn is the part of a server connection FETCH uses: its context and
// WriteResp, which drains the FETCH responses into msgs. The embedded nil
// Conn makes the type a Conn; other methods aren't called.
type fetchConn struct {
	imapserver.Conn
	ctx  *imapserver.Context
	msgs []*imap.Message
}

func (c *fetchConn) Context() *imapserver.Context { return c.ctx }

func (c *fetchConn) WriteResp(res imap.WriterTo) error {
	fetch, ok := res.(*responses.Fetch)
	if !ok {
		return fmt.Errorf("unexpected response %T", res)
	}
	for msg := range fetch.Messages {
		c.msgs = append(c.msgs, msg)
	}
	return nil
}

// fetchWith runs FETCH 1:* items through Extension's FETCH, as go-imap's
// server does, on mbox opened read-only or not, and returns what it sent.
func fetchWith(t *testing.T, mbox *mailbox, readOnly bool, items ...imap.FetchItem) ([]*imap.Message, error) {
	t.Helper()
	seqSet, err := imap.ParseSeqSet("1:*")
	if err != nil {
		t.Fatal(err)
	}
	cmd, ok := Extension{}.Command("FETCH")().(*fetchCommand)
	if !ok {
		t.Fatal("Extension doesn't handle FETCH")
	}
	cmd.SeqSet, cmd.Items = seqSet, items
	conn := &fetchConn{ctx: &imapserver.Context{Mailbox: mbox, MailboxReadOnly: readOnly}}
	err = cmd.Handle(conn)
	return conn.msgs, err
}

// fetchAll runs FETCH 1:* with items on mbox opened read-write.
func fetchAll(t *testing.T, mbox *mailbox, items ...imap.FetchItem) ([]*imap.Message, error) {
	t.Helper()
	return fetchWith(t, mbox, false, items...)
}

// In a mailbox opened read-only (EXAMINE), and through go-imap's own FETCH
// (ListMessages), which can't tell, fetching a body leaves it unread
// (RFC 3501 section 6.3.2).
func TestFetchBodyReadOnlyLeavesUnread(t *testing.T) {
	api := &fakeAPI{t: t}
	msg := fetchTestMessage("a", 1)
	mbox, sent := newActionTestMailbox(t, api, []*protonmail.Message{msg})
	cacheFull(mbox, msg)

	if _, err := fetchWith(t, mbox, true, "BODY[HEADER]"); err != nil {
		t.Fatalf("FETCH in a read-only mailbox: %v", err)
	}

	seqSet, _ := imap.ParseSeqSet("1:*")
	ch := make(chan *imap.Message, 10)
	if err := mbox.ListMessages(false, seqSet, []imap.FetchItem{"BODY[HEADER]"}, ch); err != nil {
		t.Fatalf("ListMessages() = %v", err)
	}

	if got := api.madeRequests(); len(got) != 0 {
		t.Errorf("made requests %v, want none", got)
	}
	if updates := sent(); len(updates) != 0 {
		t.Errorf("sent %v updates, want none", len(updates))
	}
	checkStatus(t, mbox, 1, 1)
}

// UID FETCH answers with the UID even when the client didn't ask for it
// (RFC 3501 section 6.4.8).
func TestUidFetchAddsUid(t *testing.T) {
	api := &fakeAPI{t: t}
	msg := fetchTestMessage("a", 0)
	mbox, _ := newActionTestMailbox(t, api, []*protonmail.Message{msg})

	seqSet, _ := imap.ParseSeqSet("1:*")
	cmd := &fetchCommand{}
	cmd.SeqSet, cmd.Items = seqSet, []imap.FetchItem{imap.FetchFlags}
	conn := &fetchConn{ctx: &imapserver.Context{Mailbox: mbox}}
	if err := cmd.UidHandle(conn); err != nil {
		t.Fatalf("UID FETCH: %v", err)
	}
	if len(conn.msgs) != 1 || conn.msgs[0].Uid != 1 {
		t.Errorf("sent %+v, want message 1 with UID 1", conn.msgs)
	}
}

// Fetching a body section without .PEEK sets \Seen (RFC 3501 section 6.4.5),
// in Proton too, and tells clients with a FETCH of the flags.
// evidence: witnesses
func TestFetchBodySetsSeen(t *testing.T) {
	tests := []struct {
		name     string
		item     imap.FetchItem
		unread   int
		wantSeen bool
	}{
		{"BODY", "BODY[HEADER]", 1, true},
		{"BODY.PEEK", "BODY.PEEK[HEADER]", 1, false},
		{"RFC822.HEADER", imap.FetchRFC822Header, 1, false},
		{"already read", "BODY[HEADER]", 0, false},
		{"no section", imap.FetchFlags, 1, false},
	}
	for _, tt := range tests {
		api := &fakeAPI{t: t}
		msg := fetchTestMessage("a", tt.unread)
		mbox, sent := newActionTestMailbox(t, api, []*protonmail.Message{msg})
		cacheFull(mbox, msg)

		msgs, err := fetchAll(t, mbox, tt.item)
		if err != nil {
			t.Fatalf("%v: ListMessages() = %v", tt.name, err)
		}
		if len(msgs) != 1 {
			t.Fatalf("%v: sent %v messages, want 1", tt.name, len(msgs))
		}

		var wantRequests []string
		wantUnseen := uint32(tt.unread)
		if tt.wantSeen {
			wantRequests = []string{"PUT /messages/read"}
			wantUnseen = 0
		}
		if got := api.madeRequests(); !slices.Equal(got, wantRequests) {
			t.Errorf("%v: made requests %v, want %v", tt.name, got, wantRequests)
		}
		checkStatus(t, mbox, 1, wantUnseen)

		updates := sent()
		if !tt.wantSeen {
			if len(updates) != 0 {
				t.Errorf("%v: sent %v updates, want none", tt.name, len(updates))
			}
			continue
		}
		if len(updates) != 1 {
			t.Fatalf("%v: sent %v updates, want 1", tt.name, len(updates))
		}
		if up, ok := updates[0].(*imapbackend.MessageUpdate); !ok || !slices.Contains(up.Message.Flags, imap.SeenFlag) {
			t.Errorf("%v: sent %#v, want a FETCH with \\Seen", tt.name, updates[0])
		}
	}
}

// An item that is neither a known name nor a body section fails FETCH before
// any message is sent, rather than being left out of the answer.
func TestFetchUnknownItem(t *testing.T) {
	for _, item := range []imap.FetchItem{"X-UNKNOWN", "BODY[", "BINARY[1]"} {
		api := &fakeAPI{t: t}
		mbox, _ := newActionTestMailbox(t, api, []*protonmail.Message{fetchTestMessage("a", 1), fetchTestMessage("b", 1)})

		msgs, err := fetchAll(t, mbox, imap.FetchFlags, item)
		if err == nil {
			t.Errorf("FETCH %v: ListMessages() succeeded, want an error", item)
		}
		if len(msgs) != 0 {
			t.Errorf("FETCH %v: sent %v messages, want none", item, len(msgs))
		}
		if got := api.madeRequests(); len(got) != 0 {
			t.Errorf("FETCH %v: made requests %v, want none", item, got)
		}
	}
}

// The macros stand for their items (RFC 3501 section 6.4.5), whoever parsed
// the command.
// evidence: witnesses
func TestFetchMacros(t *testing.T) {
	tests := []struct {
		macro imap.FetchItem
		want  []imap.FetchItem
	}{
		{imap.FetchFast, []imap.FetchItem{imap.FetchFlags, imap.FetchInternalDate, imap.FetchRFC822Size}},
		{imap.FetchAll, []imap.FetchItem{imap.FetchFlags, imap.FetchInternalDate, imap.FetchRFC822Size, imap.FetchEnvelope}},
		{imap.FetchFull, []imap.FetchItem{imap.FetchFlags, imap.FetchInternalDate, imap.FetchRFC822Size, imap.FetchEnvelope, imap.FetchBody}},
	}
	for _, tt := range tests {
		api := &fakeAPI{t: t}
		msg := fetchTestMessage("a", 0)
		mbox, _ := newActionTestMailbox(t, api, []*protonmail.Message{msg})
		cacheFull(mbox, msg)

		msgs, err := fetchAll(t, mbox, tt.macro)
		if err != nil {
			t.Fatalf("FETCH %v: ListMessages() = %v", tt.macro, err)
		}
		if len(msgs) != 1 {
			t.Fatalf("FETCH %v: sent %v messages, want 1", tt.macro, len(msgs))
		}
		got := slices.Sorted(maps.Keys(msgs[0].Items))
		want := slices.Sorted(slices.Values(tt.want))
		if !slices.Equal(got, want) {
			t.Errorf("FETCH %v: sent items %v, want %v", tt.macro, got, want)
		}
	}
}
