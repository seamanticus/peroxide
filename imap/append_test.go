package imap

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
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
	} {
		meta := importMetadata(tt.label, tt.flags, tt.h)
		if meta.Flags != tt.wantFlags || meta.Unread != tt.wantUnread || !slices.Equal(meta.LabelIDs, tt.wantLabels) {
			t.Errorf("%v: importMetadata() = %+v, want flags %v, unread %v, labels %v", tt.name, meta, tt.wantFlags, tt.wantUnread, tt.wantLabels)
		}
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

// An APPEND of a message the SMTP server just sent, to the same recipients,
// isn't imported again: in Sent, where Proton filed it, it is already; in
// another mailbox it is added there. Other recipients make another message.
// evidence: witnesses
func TestAppendSentMessage(t *testing.T) {
	hash, err := sendrecord.Hash([]byte(appendLiteral))
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name         string
		label        string
		to           []string
		wantRequests []string
		wantImports  int
	}{
		{"into Sent", protonmail.LabelSent, []string{"ann@example.org"}, nil, 0},
		{"into Archive", protonmail.LabelArchive, []string{"ann@example.org"}, []string{"PUT /messages/label"}, 0},
		{"to others", protonmail.LabelSent, []string{"carol@example.org"}, []string{"POST /mail/v4/messages/import"}, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeAPI{t: t}
			inbox, _ := newAppendTestMailbox(t, api, []*protonmail.Message{inboxMessage("s1", 0)})
			inbox.u.backend.sends = sendrecord.New()
			inbox.u.backend.sends.Start("bob", hash, tt.to).Sent("s1")
			mbox := addTestMailbox(t, inbox.u, "Dest", tt.label, nil)

			if err := appendTo(mbox, []string{imap.SeenFlag}, appendLiteral); err != nil {
				t.Fatalf("CreateMessage() = %v", err)
			}
			if got := api.madeRequests(); !slices.Equal(got, tt.wantRequests) {
				t.Errorf("made requests %q, want %q", got, tt.wantRequests)
			}
			if len(api.madeImports()) != tt.wantImports {
				t.Errorf("%v imports, want %v", len(api.madeImports()), tt.wantImports)
			}
			if tt.label == protonmail.LabelArchive && !inMailbox(mbox, "s1") {
				t.Error("the sent message isn't in Archive")
			}
		})
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
