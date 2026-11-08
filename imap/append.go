package imap

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/emersion/go-imap"
	imapbackend "github.com/emersion/go-imap/backend"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"

	"github.com/seamanticus/peroxide/protonmail"
	"github.com/seamanticus/peroxide/sendrecord"
)

// sentWait is how long APPEND waits for an SMTP send of the same message
// still under way, as proton-bridge waits (90 s).
const sentWait = 90 * time.Second

// appendAllowed refuses, before any request, APPEND into a mailbox that is a
// view of messages kept in their folders: All Mail, as proton-bridge refuses
// it, and Starred, as moveAllowed does. Star a message with \Flagged instead.
func appendAllowed(labelID string) error {
	switch labelID {
	case protonmail.LabelAllMail:
		return errors.New("cannot append to All Mail: append to the message's own mailbox")
	case protonmail.LabelStarred:
		return errors.New(`cannot append to Starred: append to the message's own mailbox with the \Flagged flag`)
	}
	return nil
}

// importMetadata is what an APPEND of a message whose header is h into
// labelID, with flags, imports it with, as proton-bridge's CreateMessage
// decides: labelled with the mailbox, and Starred if \Flagged; received into
// INBOX, sent into Sent, and elsewhere received if it has a Received field,
// sent otherwise; replied if \Answered; unread unless \Seen. Into Drafts,
// neither received nor sent, which is what makes a message a draft in Proton
// (go-proton-api MessageMetadata.IsDraft), and read, as bridge's drafts are.
// The address is the caller's.
func importMetadata(labelID string, flags []string, h textproto.Header) *protonmail.ImportMetadata {
	meta := &protonmail.ImportMetadata{
		LabelIDs: []string{labelID},
		Unread:   !slices.Contains(flags, imap.SeenFlag),
	}
	if slices.Contains(flags, imap.FlaggedFlag) {
		meta.LabelIDs = append(meta.LabelIDs, protonmail.LabelStarred)
	}
	switch {
	case labelID == protonmail.LabelDraft:
		meta.Unread = false
	case labelID == protonmail.LabelInbox:
		meta.Flags = protonmail.MessageFlagReceived
	case labelID == protonmail.LabelSent:
		meta.Flags = protonmail.MessageFlagSent
	case h.Has("Received"):
		meta.Flags = protonmail.MessageFlagReceived
	default:
		meta.Flags = protonmail.MessageFlagSent
	}
	if slices.Contains(flags, imap.AnsweredFlag) {
		meta.Flags |= protonmail.MessageFlagReplied
	}
	return meta
}

// importAddress returns the address a message from sender is imported into,
// as proton-bridge's getImportAddress chooses it in combined mode, the only
// one peroxide has: the sender's own address if it is one of addrs (by its
// e-mail, a +alias ignored), unless it is another provider's address that
// can't send; the primary address (the first by Order) otherwise.
func importAddress(addrs []*protonmail.Address, sender string) (*protonmail.Address, bool) {
	if len(addrs) == 0 {
		return nil, false
	}
	primary := slices.MinFunc(addrs, func(a, b *protonmail.Address) int { return cmp.Compare(a.Order, b.Order) })
	i := slices.IndexFunc(addrs, func(a *protonmail.Address) bool {
		return strings.EqualFold(stripPlusAlias(a.Email), stripPlusAlias(sender))
	})
	if i < 0 {
		return primary, true
	}
	if a := addrs[i]; a.Type == protonmail.AddressExternal && a.Send == protonmail.AddressSendDisabled {
		return primary, true
	}
	return addrs[i], true
}

// draftAddress is importAddress for a draft: as proton-bridge's
// getImportAddress with isDraft, a sender address that can't send or isn't
// enabled gives the primary address instead.
func draftAddress(addrs []*protonmail.Address, sender string) (*protonmail.Address, bool) {
	addr, ok := importAddress(addrs, sender)
	if !ok {
		return nil, false
	}
	if addr.Send == protonmail.AddressSendDisabled || addr.Status != protonmail.AddressEnabled {
		return importAddress(addrs, "")
	}
	return addr, true
}

// stripPlusAlias returns addr without the +alias of its local part.
func stripPlusAlias(addr string) string {
	local, domain, ok := strings.Cut(addr, "@")
	if !ok {
		return addr
	}
	if base, _, ok := strings.Cut(local, "+"); ok && base != "" {
		return base + "@" + domain
	}
	return addr
}

// messageSender is the address of the From field of h, or of Sender if it
// has no From; "" if neither holds one.
func messageSender(h textproto.Header) string {
	mh := mail.Header{Header: message.Header{Header: h}}
	for _, k := range []string{"From", "Sender"} {
		if list, err := mh.AddressList(k); err == nil && len(list) > 0 {
			return list[0].Address
		}
	}
	return ""
}

// addressKey returns the first active key of addr that is unlocked: the one
// a message imported into addr is encrypted to, as bridge's addrKR.FirstKey
// (its keyring holds the unlocked active keys, in Proton's order: primary
// first).
func addressKey(addr *protonmail.Address, unlocked openpgp.EntityList) (*openpgp.Entity, error) {
	for _, key := range addr.Keys {
		if key.Active != 1 {
			continue
		}
		e, err := key.Entity()
		if err != nil {
			log.Printf("cannot read key %v of <%v>: %v", key.ID, addr.Email, err)
			continue
		}
		if keys := unlocked.KeysById(e.PrimaryKey.KeyId); len(keys) > 0 {
			return keys[0].Entity, nil
		}
	}
	return nil, fmt.Errorf("<%v> has no unlocked active key to import into", addr.Email)
}

// withTextPart returns literal with an empty text part added if it has none,
// as proton-bridge's AttachEmptyTextPartIfNoneExists does before an import:
// Proton reads the text of a message from it. A multipart/mixed message has
// one if a part without Content-Disposition is text; any other message if it
// is text itself. Otherwise a multipart/mixed message gets the part last,
// and any other is wrapped in one, its content fields moved to its part.
func withTextPart(literal []byte) ([]byte, error) {
	h, body := splitEntity(literal)
	mh := message.Header{Header: h}
	mediaType, params, err := mh.ContentType()
	if err != nil {
		mediaType = ""
	}
	if mediaType != "multipart/mixed" {
		// A PGP/MIME message is imported encrypted whole (encryptRFC822), so
		// needs no text part; wrapped, clients would find its encrypted or
		// signed part below a multipart/mixed, which none decrypts or checks.
		if strings.HasPrefix(mediaType, "text/") || mediaType == "multipart/encrypted" || mediaType == "multipart/signed" {
			return literal, nil
		}
		return wrapWithTextPart(h, body)
	}
	e, err := message.New(mh, bytes.NewReader(body))
	if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
		return nil, err
	}
	mr := e.MultipartReader()
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
			return nil, err
		}
		if p.Header.Has("Content-Disposition") {
			continue
		}
		if t, _, err := p.Header.ContentType(); err == nil && strings.HasPrefix(t, "text/") {
			return literal, nil
		}
	}
	boundary := params["boundary"]
	// The closing delimiter, the last in the body: the new part goes before.
	end := bytes.LastIndex(body, []byte("--"+boundary+"--"))
	if boundary == "" || end < 0 {
		return nil, errors.New("multipart/mixed message without a closing boundary")
	}
	var b bytes.Buffer
	b.Write(literal[:len(literal)-len(body)])
	b.Write(body[:end])
	if end > 0 && body[end-1] != '\n' {
		b.WriteString("\r\n")
	}
	fmt.Fprintf(&b, "--%v\r\n%v\r\n", boundary, emptyTextPartHeader)
	b.Write(body[end:])
	return b.Bytes(), nil
}

const emptyTextPartHeader = "Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n"

// wrapWithTextPart writes a message whose header is h and body body as
// multipart/mixed: its content fields with body as the first part, an empty
// text part the second.
func wrapWithTextPart(h textproto.Header, body []byte) ([]byte, error) {
	var part textproto.Header
	for _, k := range []string{"Content-Type", "Content-Disposition", "Content-Transfer-Encoding"} {
		for f := h.FieldsByKey(k); f.Next(); {
			part.Add(k, f.Value())
		}
		h.Del(k)
	}
	// From the message, so that the same APPEND sends the same bytes; a
	// body holding its own hash is not to be expected.
	sum := sha256.Sum256(body)
	boundary := "text-" + hex.EncodeToString(sum[:16])
	mh := message.Header{Header: h}
	mh.SetContentType("multipart/mixed", map[string]string{"boundary": boundary})

	var b bytes.Buffer
	if err := textproto.WriteHeader(&b, mh.Header); err != nil {
		return nil, err
	}
	fmt.Fprintf(&b, "--%v\r\n", boundary)
	if err := textproto.WriteHeader(&b, part); err != nil {
		return nil, err
	}
	b.Write(body)
	fmt.Fprintf(&b, "\r\n--%v\r\n%v\r\n\r\n--%v--\r\n", boundary, emptyTextPartHeader, boundary)
	return b.Bytes(), nil
}

// recipients returns the To, Cc and Bcc addresses of h, as proton-bridge's
// getLiteralToList lists them for the send recorder.
func recipients(h textproto.Header) []string {
	mh := mail.Header{Header: message.Header{Header: h}}
	var to []string
	for _, k := range []string{"To", "Cc", "Bcc"} {
		list, _ := mh.AddressList(k)
		for _, a := range list {
			to = append(to, a.Address)
		}
	}
	return to
}

func (mbox *mailbox) CreateMessage(flags []string, date time.Time, body imap.Literal) error {
	if body.Len() > appendLimit {
		return imapbackend.ErrTooBig
	}
	if err := appendAllowed(mbox.label); err != nil {
		return err
	}
	literal, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("cannot read the message: %w", err)
	}
	// go-imap v1 gives a backend no context: the requests run to their end.
	// The client's date isn't kept, as proton-bridge ignores it too: Proton
	// dates an import by its Date field.
	ctx := context.Background()

	h, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(literal)))
	if err != nil {
		return fmt.Errorf("cannot read the message header: %w", err)
	}
	// A client saving a draft again may keep its Message-Id, and deletes the
	// old draft once the APPEND answers OK: answered "already there", it
	// would delete the only copy. So a draft is always imported.
	if mbox.label != protonmail.LabelDraft {
		existing, err := mbox.existingMessages(ctx, literal, h)
		if err != nil {
			return err
		}
		if len(existing) > 0 {
			return mbox.alreadyThere(existing)
		}
	}

	// Into Drafts the message is imported too, not made a Proton draft
	// (POST /messages), which keeps one body part and none of the header.
	pickAddress := importAddress
	if mbox.label == protonmail.LabelDraft {
		pickAddress = draftAddress
	}
	addr, ok := pickAddress(mbox.u.addresses(), messageSender(h))
	if !ok {
		return errors.New("the account has no address to import the message into")
	}
	key, err := addressKey(addr, mbox.u.privateKeys)
	if err != nil {
		return err
	}
	if literal, err = withTextPart(literal); err != nil {
		return fmt.Errorf("cannot read the message: %w", err)
	}
	meta := importMetadata(mbox.label, flags, h)
	meta.AddressID = addr.ID
	if _, err := mbox.u.c.ImportMessage(ctx, key, meta, literal); err != nil {
		if errors.Is(err, protonmail.ErrImportSizeExceeded) {
			return imapbackend.ErrTooBig
		}
		return fmt.Errorf("cannot import the message: %w", err)
	}
	return mbox.Poll()
}

// existingMessages returns the messages of the account an import of literal
// would end up as, none if it would make a new one. Proton's import doesn't
// make a second message of one it has (seen with the same Message-Id): it
// files the one it has in the mailbox, which for a folder moves it out of its
// own. So, before importing, peroxide looks for
//   - the message its SMTP server sent of literal in the last
//     sendrecord.Expiry, by the same account to the same recipients, waiting
//     up to sentWait for a send under way (proton-bridge's sendrecorder);
//   - the messages whose Message-Id is literal's (MessagesByExternalID).
func (mbox *mailbox) existingMessages(ctx context.Context, literal []byte, h textproto.Header) ([]*protonmail.Message, error) {
	// A message Hash can't read the SMTP server didn't record either.
	if hash, err := sendrecord.Hash(literal); err == nil {
		waitCtx, cancel := context.WithTimeout(ctx, sentWait)
		apiID, ok := mbox.u.backend.sends.SentMessage(waitCtx, mbox.u.username, hash, recipients(h))
		cancel()
		if ok {
			msgs, err := mbox.u.c.MessagesByID(ctx, []string{apiID})
			if err != nil {
				return nil, fmt.Errorf("cannot look up the message just sent: %w", err)
			}
			if len(msgs) > 0 {
				return msgs, nil
			}
		}
	}

	mh := mail.Header{Header: message.Header{Header: h}}
	id, err := mh.MessageID()
	if err != nil || id == "" {
		// Without one, Proton has nothing to know the message by.
		return nil, nil
	}
	msgs, err := mbox.u.c.MessagesByExternalID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("cannot check whether the message is already in your account: %w", err)
	}
	return slices.DeleteFunc(msgs, func(m *protonmail.Message) bool { return m.ExternalID != id }), nil
}

// alreadyThere answers an APPEND of a message the account has as existing:
// nothing is imported or moved. If one of them is in this mailbox, the APPEND
// did what was asked: OK. Otherwise importing would move it here from where
// it is, so NO, and a client moving it from another account keeps its copy.
// Either way under ALERT, which clients must show (RFC 3501 section 7.1.1):
// an OK text alone is shown by none of aerc, Thunderbird and Evolution.
// The APPEND's flags aren't applied to the message found: it is the one
// already there, flags and all.
//
// INBOX and Sent are the exception: Proton keeps a message in both (mail to
// yourself), so adding one to a message in the other moves nothing, as COPY
// does it. That is what a client saving its reply to the parent's mailbox
// asks (aerc's copy-to-replied): the message is labelled, and OK.
func (mbox *mailbox) alreadyThere(existing []*protonmail.Message) error {
	name := mbox.Name()
	if slices.ContainsFunc(existing, func(m *protonmail.Message) bool {
		return slices.Contains(m.LabelIDs, mbox.label)
	}) {
		log.Printf("APPEND to %v of message %v, already there: not imported", name, existing[0].ID)
		return appendAlert(imap.StatusRespOk, fmt.Sprintf("This message is already in %v: it was not copied again", name))
	}
	if partner, ok := inboxSentPartner(mbox.label); ok {
		if i := slices.IndexFunc(existing, func(m *protonmail.Message) bool {
			return slices.Contains(m.LabelIDs, partner)
		}); i >= 0 {
			apiID := existing[i].ID
			failed, err := mbox.u.c.LabelMessages(mbox.label, []string{apiID})
			if err != nil {
				return fmt.Errorf("cannot add the message already in your account to %v: %w", name, err)
			}
			if err := mbox.u.applyAction([]string{apiID}, failed, labelUpdate(mbox.label, true)); err != nil {
				return err
			}
			from := mbox.u.getMailboxByLabel(partner)
			if from == nil {
				panic("invariant: setMailboxes makes INBOX and Sent")
			}
			fromName := from.Name()
			log.Printf("APPEND to %v of message %v, already in %v: labelled, not imported", name, apiID, fromName)
			return appendAlert(imap.StatusRespOk, fmt.Sprintf(
				"This message is already in %v: it is now in %v too, not copied again", fromName, name))
		}
	}
	where := mbox.u.mailboxNames(existing)
	log.Printf("APPEND to %v of message %v, already in %v: refused", name, existing[0].ID, where)
	return appendAlert(imap.StatusRespNo, fmt.Sprintf(
		"This message is already in your Proton account, in %v: it was not copied, as Proton keeps one copy of a message",
		where))
}

// inboxSentPartner returns the folder a message may be in beside labelID:
// Sent for INBOX, INBOX for Sent.
func inboxSentPartner(labelID string) (string, bool) {
	switch labelID {
	case protonmail.LabelInbox:
		return protonmail.LabelSent, true
	case protonmail.LabelSent:
		return protonmail.LabelInbox, true
	}
	return "", false
}

// mailboxNames lists the mailboxes holding msgs, but All Mail and Starred,
// which hold every message and the starred ones: All Mail if none is left.
func (u *user) mailboxNames(msgs []*protonmail.Message) string {
	var names []string
	for _, m := range msgs {
		for _, labelID := range m.LabelIDs {
			if labelID == protonmail.LabelAllMail || labelID == protonmail.LabelStarred {
				continue
			}
			if mbox := u.getMailboxByLabel(labelID); mbox != nil {
				if name := mbox.Name(); !slices.Contains(names, name) {
					names = append(names, name)
				}
			}
		}
	}
	if len(names) == 0 {
		return "All Mail"
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

// appendAlert is APPEND's tagged response of typ with text under ALERT.
// go-imap's server sends an *imap.ErrStatusResp a command returns as its
// response, OK included (server/conn.go handleCommand), as its own SELECT
// ends; its APPEND passes CreateMessage's error on.
func appendAlert(typ imap.StatusRespType, text string) error {
	return &imap.ErrStatusResp{Resp: &imap.StatusResp{Type: typ, Code: imap.CodeAlert, Info: text}}
}
