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
// sent otherwise; replied if \Answered; unread unless \Seen. Not for Drafts,
// whose messages are drafts (createMessage). The address is the caller's.
func importMetadata(labelID string, flags []string, h textproto.Header) *protonmail.ImportMetadata {
	meta := &protonmail.ImportMetadata{
		LabelIDs: []string{labelID},
		Unread:   !slices.Contains(flags, imap.SeenFlag),
	}
	if slices.Contains(flags, imap.FlaggedFlag) {
		meta.LabelIDs = append(meta.LabelIDs, protonmail.LabelStarred)
	}
	switch {
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
		if strings.HasPrefix(mediaType, "text/") {
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
	ctx := context.Background()

	if mbox.label == protonmail.LabelDraft {
		if _, err := createMessage(mbox.u.c, mbox.u.u, mbox.u.privateKeys, mbox.u.addresses(), bytes.NewReader(literal)); err != nil {
			return err
		}
		return mbox.Poll()
	}

	h, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(literal)))
	if err != nil {
		return fmt.Errorf("cannot read the message header: %w", err)
	}
	if done, err := mbox.appendSent(ctx, literal, h); done || err != nil {
		return err
	}

	addr, ok := importAddress(mbox.u.addresses(), messageSender(h))
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

// appendSent handles an APPEND of a message peroxide's SMTP server sent in
// the last sendrecord.Expiry, by the same account to the same recipients:
// Proton filed it in Sent already, so it is added to this mailbox, as a COPY
// of it would, instead of imported again (proton-bridge does the same, and
// gluon adds the existing message to the mailbox). done is false for any
// other message: it is to be imported.
func (mbox *mailbox) appendSent(ctx context.Context, literal []byte, h textproto.Header) (done bool, err error) {
	if mbox.u.backend == nil || mbox.u.backend.sends == nil {
		return false, nil
	}
	hash, err := sendrecord.Hash(literal)
	if err != nil {
		// Not a message the SMTP server could have sent: it hashes them all.
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, sentWait)
	defer cancel()
	apiID, ok := mbox.u.backend.sends.SentMessage(ctx, mbox.u.username, hash, recipients(h))
	if !ok {
		return false, nil
	}
	log.Printf("APPEND of message %v, just sent: added to %v, not imported again", apiID, mbox.name)
	if mbox.label != protonmail.LabelSent {
		failed, err := mbox.u.c.LabelMessages(mbox.label, []string{apiID})
		if err != nil {
			return true, err
		}
		if err := mbox.u.applyAction([]string{apiID}, failed, labelUpdate(mbox.label, true)); err != nil {
			return true, err
		}
	}
	return true, mbox.Poll()
}
