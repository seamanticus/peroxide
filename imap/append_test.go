package imap

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/emersion/go-imap"
	imapbackend "github.com/emersion/go-imap/backend"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/textproto"

	"github.com/seamanticus/peroxide/protonmail"
	"github.com/seamanticus/peroxide/sendrecord"
)

const appendLiteral = "From: Bob <bob@example.org>\r\n" +
	"To: ann@example.org\r\n" +
	"Subject: hello\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"hello ann\r\n"

// newAppendTestMailbox is newActionTestMailbox with an address bob@example.org
// whose key is unlocked, and a poll that returns at once (APPEND waits on
// one).
func newAppendTestMailbox(t *testing.T, api *fakeAPI, msgs []*protonmail.Message) (*mailbox, *openpgp.Entity) {
	t.Helper()
	inbox, _ := newActionTestMailbox(t, api, msgs)
	key := newKey(t, "bob")
	u := inbox.u
	u.username = "bob"
	u.privateKeys = openpgp.EntityList{key}
	u.addrs = []*protonmail.Address{{ID: "a1", Email: "bob@example.org", Send: protonmail.AddressSendPrimary, Keys: []*protonmail.PrivateKey{publicKey(t, "k1", key)}}}
	u.eventsReceiver = &fakeRewinder{}
	u.done = make(chan struct{})
	close(u.done)
	return inbox, key
}

// appendTo APPENDs literal with flags into mbox.
func appendTo(mbox *mailbox, flags []string, literal string) error {
	return mbox.CreateMessage(flags, time.Time{}, bytes.NewBufferString(literal))
}

// An APPEND outside Drafts imports the message into the mailbox, in the
// sender's address, encrypted to its key, with the flags and labels
// proton-bridge sends: the mailbox, and Starred if \Flagged.
// evidence: witnesses
func TestAppendImports(t *testing.T) {
	api := &fakeAPI{t: t}
	inbox, key := newAppendTestMailbox(t, api, nil)
	archive := addTestMailbox(t, inbox.u, "Archive", protonmail.LabelArchive, nil)

	if err := appendTo(archive, []string{imap.FlaggedFlag, imap.SeenFlag}, appendLiteral); err != nil {
		t.Fatalf("CreateMessage() = %v", err)
	}
	if len(api.madeImports()) != 1 {
		t.Fatalf("%v imports, want 1", len(api.madeImports()))
	}
	imp := api.madeImports()[0]
	if imp.meta.AddressID != "a1" || !slices.Equal(imp.meta.LabelIDs, []string{protonmail.LabelArchive, protonmail.LabelStarred}) ||
		imp.meta.Unread != 0 || imp.meta.Flags != protonmail.MessageFlagSent {
		t.Errorf("import metadata %+v", imp.meta)
	}

	// The text is encrypted to the address key, the header kept.
	e, err := message.Read(bytes.NewReader(imp.message))
	if err != nil {
		t.Fatalf("read the imported message: %v", err)
	}
	if got := e.Header.Get("Subject"); got != "hello" {
		t.Errorf("Subject = %q", got)
	}
	block, err := armor.Decode(e.Body)
	if err != nil {
		t.Fatalf("the body isn't armored: %v", err)
	}
	md, err := openpgp.ReadMessage(block.Body, openpgp.EntityList{key}, nil, nil)
	if err != nil {
		t.Fatalf("the body isn't encrypted to the address key: %v", err)
	}
	if plain, _ := io.ReadAll(md.UnverifiedBody); string(plain) != "hello ann\r\n" {
		t.Errorf("body = %q", plain)
	}
}

// importMetadata gives the flags proton-bridge's CreateMessage gives: INBOX
// received, Sent sent, elsewhere received only with a Received field; replied
// with \Answered; unread without \Seen; Starred with \Flagged.
// evidence: witnesses
func TestImportMetadata(t *testing.T) {
	var plain, received textproto.Header
	received.Set("Received", "from mx by mx")
	const (
		recv    = protonmail.MessageFlagReceived
		sent    = protonmail.MessageFlagSent
		replied = protonmail.MessageFlagReplied
	)
	custom := "custom-folder"
	for _, tt := range []struct {
		name       string
		label      string
		flags      []string
		h          textproto.Header
		wantFlags  protonmail.MessageFlags
		wantUnread bool
		wantLabels []string
	}{
		{"INBOX", protonmail.LabelInbox, nil, plain, recv, true, []string{protonmail.LabelInbox}},
		{"INBOX without Received", protonmail.LabelInbox, nil, plain, recv, true, []string{protonmail.LabelInbox}},
		{"Sent with Received", protonmail.LabelSent, nil, received, sent, true, []string{protonmail.LabelSent}},
		{"folder, received", custom, nil, received, recv, true, []string{custom}},
		{"folder, sent", custom, nil, plain, sent, true, []string{custom}},
		{"seen", custom, []string{imap.SeenFlag}, plain, sent, false, []string{custom}},
		{"answered", protonmail.LabelInbox, []string{imap.AnsweredFlag}, plain, recv | replied, true, []string{protonmail.LabelInbox}},
		{"flagged", protonmail.LabelArchive, []string{imap.FlaggedFlag}, plain, sent, true, []string{protonmail.LabelArchive, protonmail.LabelStarred}},
		{"draft flag ignored", custom, []string{imap.DraftFlag}, plain, sent, true, []string{custom}},
		{"Drafts", protonmail.LabelDraft, nil, received, 0, false, []string{protonmail.LabelDraft}},
		{"Drafts answered", protonmail.LabelDraft, []string{imap.AnsweredFlag}, plain, replied, false, []string{protonmail.LabelDraft}},
	} {
		meta := importMetadata(tt.label, tt.flags, tt.h)
		if meta.Flags != tt.wantFlags || meta.Unread != tt.wantUnread || !slices.Equal(meta.LabelIDs, tt.wantLabels) {
			t.Errorf("%v: importMetadata() = %+v, want flags %v, unread %v, labels %v", tt.name, meta, tt.wantFlags, tt.wantUnread, tt.wantLabels)
		}
	}
}

// An APPEND into Drafts imports the message whole, neither received nor
// sent, in the primary address when the sender isn't the account's.
// evidence: witnesses
func TestAppendDraftsImports(t *testing.T) {
	api := &fakeAPI{t: t}
	inbox, _ := newAppendTestMailbox(t, api, nil)
	inbox.u.addrs[0].Status = protonmail.AddressEnabled
	drafts := addTestMailbox(t, inbox.u, "Drafts", protonmail.LabelDraft, nil)

	literal := strings.Replace(appendLiteral, "Bob <bob@example.org>", "GitHub <notifications@github.com>", 1)
	if err := appendTo(drafts, nil, literal); err != nil {
		t.Fatalf("CreateMessage() = %v", err)
	}
	imports := api.madeImports()
	if len(imports) != 1 {
		t.Fatalf("%v imports, want 1", len(imports))
	}
	if m := imports[0].meta; m.AddressID != "a1" || m.Flags != 0 || m.Unread != 0 || !slices.Equal(m.LabelIDs, []string{protonmail.LabelDraft}) {
		t.Errorf("import metadata %+v", m)
	}
	if !bytes.Contains(imports[0].message, []byte("From: GitHub <notifications@github.com>")) {
		t.Error("the imported draft lost its From field")
	}
}

// literalOfLen is a literal of any length holding nothing: for the limit,
// checked before the message is read.
type literalOfLen int

func (l literalOfLen) Len() int                   { return int(l) }
func (l literalOfLen) Read(p []byte) (int, error) { return 0, io.EOF }

// APPEND into All Mail or Starred, or of a message over APPENDLIMIT, is
// refused before any request, the latter with TOOBIG.
// evidence: witnesses
func TestAppendRefused(t *testing.T) {
	api := &fakeAPI{t: t}
	inbox, _ := newAppendTestMailbox(t, api, nil)
	allMail := addTestMailbox(t, inbox.u, "All Mail", protonmail.LabelAllMail, nil)
	starred := addTestMailbox(t, inbox.u, "Starred", protonmail.LabelStarred, nil)

	for _, mbox := range []*mailbox{allMail, starred} {
		if err := appendTo(mbox, nil, appendLiteral); err == nil {
			t.Errorf("APPEND to %v succeeded", mbox.name)
		}
	}
	if err := inbox.CreateMessage(nil, time.Time{}, literalOfLen(appendLimit+1)); !errors.Is(err, imapbackend.ErrTooBig) {
		t.Errorf("APPEND over the limit = %v, want ErrTooBig", err)
	}
	if got := api.madeRequests(); len(got) != 0 {
		t.Errorf("made requests %q, want none", got)
	}
}

// importAddress takes the sender's address of the account, a +alias
// ignored, unless it is another provider's that can't send; the primary
// address (lowest Order) otherwise.
// evidence: witnesses
func TestImportAddress(t *testing.T) {
	addrs := []*protonmail.Address{
		{ID: "second", Email: "two@example.org", Order: 2},
		{ID: "primary", Email: "one@example.org", Order: 1},
		{ID: "external", Email: "me@other.org", Order: 3, Type: protonmail.AddressExternal, Send: protonmail.AddressSendDisabled},
		{ID: "byoe", Email: "me@byoe.org", Order: 4, Type: protonmail.AddressExternal, Send: protonmail.AddressSendSecondary},
	}
	for _, tt := range []struct {
		sender, want string
	}{
		{"two@example.org", "second"},
		{"TWO@Example.org", "second"},
		{"two+news@example.org", "second"},
		{"stranger@example.net", "primary"},
		{"", "primary"},
		{"me@other.org", "primary"},
		{"me@byoe.org", "byoe"},
	} {
		got, ok := importAddress(addrs, tt.sender)
		if !ok || got.ID != tt.want {
			t.Errorf("importAddress(%q) = %v, %v, want %v", tt.sender, got, ok, tt.want)
		}
	}
	if _, ok := importAddress(nil, "one@example.org"); ok {
		t.Error("importAddress() found an address in none")
	}
}

// draftAddress is importAddress, but a sender address that can't send or
// isn't enabled gives the primary address.
// evidence: witnesses
func TestDraftAddress(t *testing.T) {
	enabled, send := protonmail.AddressEnabled, protonmail.AddressSendSecondary
	addrs := []*protonmail.Address{
		{ID: "primary", Email: "one@example.org", Order: 1, Status: enabled, Send: protonmail.AddressSendPrimary},
		{ID: "second", Email: "two@example.org", Order: 2, Status: enabled, Send: send},
		{ID: "nosend", Email: "three@example.org", Order: 3, Status: enabled, Send: protonmail.AddressSendDisabled},
		{ID: "disabled", Email: "four@example.org", Order: 4, Status: protonmail.AddressDisabled, Send: send},
	}
	for _, tt := range []struct {
		sender, want string
	}{
		{"two@example.org", "second"},
		{"three@example.org", "primary"},
		{"four@example.org", "primary"},
		{"mail@mailserver.com", "primary"},
	} {
		if got, ok := draftAddress(addrs, tt.sender); !ok || got.ID != tt.want {
			t.Errorf("draftAddress(%q) = %v, %v, want %v", tt.sender, got, ok, tt.want)
		}
	}
}

// withTextPart adds an empty text part to a message without one, as
// proton-bridge does: last in a multipart/mixed message, in one wrapping any
// other; a message with a text part is left as is.
// evidence: witnesses
func TestWithTextPart(t *testing.T) {
	head := "From: a@example.org\r\nSubject: s\r\n"
	for _, tt := range []struct {
		name, literal string
		// wantParts are the media types of the parts after, nil if the
		// message is left as is.
		wantParts []string
	}{
		{"text", head + "Content-Type: text/html\r\n\r\n<p>x</p>\r\n", nil},
		{"no content type", head + "\r\nx\r\n", nil},
		{"PGP/MIME encrypted", head + "Content-Type: multipart/encrypted; boundary=b; protocol=\"application/pgp-encrypted\"\r\n\r\n--b\r\nContent-Type: application/pgp-encrypted\r\n\r\nVersion: 1\r\n--b\r\nContent-Type: application/octet-stream\r\n\r\nx\r\n--b--\r\n", nil},
		{"PGP/MIME signed", head + "Content-Type: multipart/signed; boundary=b; protocol=\"application/pgp-signature\"\r\n\r\n--b\r\nContent-Type: application/pdf\r\n\r\nx\r\n--b\r\nContent-Type: application/pgp-signature\r\n\r\ns\r\n--b--\r\n", nil},
		{"mixed with text", head + "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nx\r\n--b--\r\n", nil},
		{"mixed with an attachment only", head + "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=a.txt\r\n\r\nx\r\n--b--\r\n",
			[]string{"text/plain", "text/plain"}},
		{"attachment only", head + "Content-Type: application/pdf\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERg==\r\n",
			[]string{"application/pdf", "text/plain"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := withTextPart([]byte(tt.literal))
			if err != nil {
				t.Fatalf("withTextPart() = %v", err)
			}
			if tt.wantParts == nil {
				if string(got) != tt.literal {
					t.Errorf("withTextPart() = %q, want the message as is", got)
				}
				return
			}
			e, err := message.Read(bytes.NewReader(got))
			if err != nil {
				t.Fatalf("read %q: %v", got, err)
			}
			if mt, _, _ := e.Header.ContentType(); mt != "multipart/mixed" {
				t.Fatalf("Content-Type %v, want multipart/mixed", mt)
			}
			if e.Header.Get("Subject") != "s" {
				t.Errorf("the header lost its Subject: %q", got)
			}
			var parts []string
			var first []byte
			mr := e.MultipartReader()
			for {
				p, err := mr.NextPart()
				if errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					t.Fatalf("read the parts of %q: %v", got, err)
				}
				mt, _, _ := p.Header.ContentType()
				parts = append(parts, mt)
				if first == nil {
					first, _ = io.ReadAll(p.Body)
				}
			}
			if !slices.Equal(parts, tt.wantParts) {
				t.Errorf("parts %v, want %v, in %q", parts, tt.wantParts, got)
			}
			if tt.name == "attachment only" && string(first) != "%PDF" {
				t.Errorf("the wrapped part is %q, want its decoded bytes", first)
			}
			again, _ := withTextPart([]byte(tt.literal))
			if !bytes.Equal(got, again) {
				t.Error("withTextPart() of one message gave two results")
			}
		})
	}
}

// statusResp returns the tagged response err gives APPEND, as go-imap's
// server sends it: an *imap.ErrStatusResp as is, nil as OK, another error
// as NO with its text.
func statusResp(err error) *imap.StatusResp {
	if err == nil {
		return &imap.StatusResp{Type: imap.StatusRespOk}
	}
	if s, ok := errors.AsType[*imap.ErrStatusResp](err); ok {
		return s.Resp
	}
	return &imap.StatusResp{Type: imap.StatusRespNo, Info: err.Error()}
}

// An APPEND of a message the account has already isn't imported: Proton
// would file the one it has in the mailbox, moving it out of its folder.
// Found by its Message-Id, or as the message the SMTP server just sent to
// the same recipients, it is OK [ALERT] if it is in the mailbox already, NO
// [ALERT] naming where it is otherwise; nothing changes in Proton.
// evidence: witnesses
func TestAppendExisting(t *testing.T) {
	withID := "Message-Id: <m1@example.org>\r\n" + appendLiteral
	hash, err := sendrecord.Hash([]byte(appendLiteral))
	if err != nil {
		t.Fatal(err)
	}
	const metadata = "POST /mail/v4/messages"
	for _, tt := range []struct {
		name    string
		literal string
		// labels of message "e" in Proton; its Message-Id is m1@example.org.
		labels []string
		// sentTo, if set, records "e" as sent over SMTP to them.
		sentTo       []string
		dest         string
		wantType     imap.StatusRespType
		wantIn       string // in the text
		wantRequests []string
	}{
		{"new message", withID, nil, nil, protonmail.LabelArchive, imap.StatusRespOk, "",
			[]string{metadata, "POST /mail/v4/messages/import"}},
		{"in another folder", withID, []string{protonmail.LabelInbox, protonmail.LabelAllMail}, nil, protonmail.LabelSpam, imap.StatusRespNo, "in INBOX",
			[]string{metadata}},
		{"in the folder", withID, []string{protonmail.LabelSpam, protonmail.LabelAllMail}, nil, protonmail.LabelSpam, imap.StatusRespOk, "already in Dest",
			[]string{metadata}},
		{"only in All Mail", withID, []string{protonmail.LabelAllMail, protonmail.LabelStarred}, nil, protonmail.LabelTrash, imap.StatusRespNo, "in All Mail",
			[]string{metadata}},
		{"just sent, into Sent", appendLiteral, []string{protonmail.LabelSent, protonmail.LabelAllMail}, []string{"ann@example.org"}, protonmail.LabelSent, imap.StatusRespOk, "already in Dest",
			[]string{metadata}},
		{"just sent, into Archive", appendLiteral, []string{protonmail.LabelSent, protonmail.LabelAllMail}, []string{"ann@example.org"}, protonmail.LabelArchive, imap.StatusRespNo, "in Sent",
			[]string{metadata}},
		{"sent to others", appendLiteral, []string{protonmail.LabelSent}, []string{"carol@example.org"}, protonmail.LabelSent, imap.StatusRespOk, "",
			[]string{"POST /mail/v4/messages/import"}},
		{"in Sent, into INBOX", withID, []string{protonmail.LabelSent, protonmail.LabelAllMail}, nil, protonmail.LabelInbox, imap.StatusRespOk, "in Dest too",
			[]string{metadata, "PUT /messages/label"}},
		{"in INBOX, into Sent", withID, []string{protonmail.LabelInbox, protonmail.LabelAllMail}, nil, protonmail.LabelSent, imap.StatusRespOk, "already in INBOX",
			[]string{metadata, "PUT /messages/label"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeAPI{t: t, labelIDs: map[string][]string{}, externalIDs: map[string]string{}}
			if tt.labels != nil {
				api.labelIDs["e"] = tt.labels
				api.externalIDs["e"] = "m1@example.org"
			}
			inbox, _ := newAppendTestMailbox(t, api, nil)
			addTestMailbox(t, inbox.u, "Sent", protonmail.LabelSent, nil)
			if tt.sentTo != nil {
				inbox.u.backend.sends.Start("bob", hash, tt.sentTo).Sent("e")
			}
			mbox := addTestMailbox(t, inbox.u, "Dest", tt.dest, nil)

			resp := statusResp(appendTo(mbox, []string{imap.SeenFlag}, tt.literal))
			if resp.Type != tt.wantType || !strings.Contains(resp.Info, tt.wantIn) {
				t.Errorf("response %v %q, want %v with %q", resp.Type, resp.Info, tt.wantType, tt.wantIn)
			}
			if wantAlert := tt.wantIn != ""; (resp.Code == imap.CodeAlert) != wantAlert {
				t.Errorf("response code %q, want ALERT %v", resp.Code, wantAlert)
			}
			if got := api.madeRequests(); !slices.Equal(got, tt.wantRequests) {
				t.Errorf("made requests %q, want %q", got, tt.wantRequests)
			}
		})
	}
}

// A draft saved again under the Message-Id of one in the account is imported:
// the client deletes the old draft once the APPEND answers OK, so an "already
// there" answer would lose the edit. No lookup is made.
// evidence: witnesses
func TestAppendDraftKnownMessageID(t *testing.T) {
	api := &fakeAPI{t: t,
		labelIDs:    map[string][]string{"d": {protonmail.LabelDraft, protonmail.LabelAllMail}},
		externalIDs: map[string]string{"d": "m1@example.org"},
	}
	inbox, _ := newAppendTestMailbox(t, api, nil)
	drafts := addTestMailbox(t, inbox.u, "Drafts", protonmail.LabelDraft, nil)

	if err := appendTo(drafts, []string{imap.DraftFlag}, "Message-Id: <m1@example.org>\r\n"+appendLiteral); err != nil {
		t.Fatalf("CreateMessage() = %v", err)
	}
	if got, want := api.madeRequests(), []string{"POST /mail/v4/messages/import"}; !slices.Equal(got, want) {
		t.Errorf("made requests %q, want %q", got, want)
	}
}

// The answer for a message already there names the mailboxes as they are
// while a rename goes on: run with -race, it reads names under their lock.
// evidence: witnesses
func TestAppendExistingDuringRename(t *testing.T) {
	api := &fakeAPI{t: t,
		labelIDs:    map[string][]string{"e": {"f1", protonmail.LabelAllMail}},
		externalIDs: map[string]string{"e": "m1@example.org"},
	}
	inbox, _ := newAppendTestMailbox(t, api, nil)
	work := addTestMailbox(t, inbox.u, "Work", "f1", nil)
	dest := addTestMailbox(t, inbox.u, "Dest", protonmail.LabelArchive, nil)

	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 100 {
			work.setName(fmt.Sprintf("Work %v", i))
			dest.setName(fmt.Sprintf("Dest %v", i))
		}
	})
	resp := statusResp(appendTo(dest, nil, "Message-Id: <m1@example.org>\r\n"+appendLiteral))
	wg.Wait()
	if resp.Type != imap.StatusRespNo || !strings.Contains(resp.Info, "in Work") {
		t.Errorf("response %v %q, want NO naming Work", resp.Type, resp.Info)
	}
}

// recipients lists To, Cc and Bcc, as the SMTP server's envelope holds them.
// evidence: witnesses
func TestRecipients(t *testing.T) {
	h, err := textproto.ReadHeader(bufio.NewReader(strings.NewReader(
		"To: A <a@x.org>, b@x.org\r\nCc: c@x.org\r\nBcc: d@x.org\r\nReply-To: e@x.org\r\n\r\n")))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := recipients(h), []string{"a@x.org", "b@x.org", "c@x.org", "d@x.org"}; !slices.Equal(got, want) {
		t.Errorf("recipients() = %v, want %v", got, want)
	}
}
