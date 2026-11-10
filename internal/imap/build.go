package imap

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp/armor"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend/backendutil"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"

	"github.com/seamanticus/peroxide/internal/imap/database"
	"github.com/seamanticus/peroxide/internal/protonmail"
)

// A message is built once, as proton-bridge builds it (pkg/message/build.go,
// BuildRFC822Into), stored, and every FETCH item that depends on its contents
// is served from the stored bytes: they never change under a UID. The build
// never fails on what the message holds: what can't be decrypted is served
// still encrypted, as bridge does, and bytes go-imap can't parse are wrapped
// in a note (safeMessage). It fails only when Proton can't be asked.

// builtMessage is a message as served, and what FETCH derives from it.
type builtMessage struct {
	literal []byte
	info    database.BodyInfo
}

// buildMessage builds full, downloading its attachments.
func (u *user) buildMessage(ctx context.Context, full *protonmail.Message) (builtMessage, error) {
	literal, fallback, err := u.buildLiteral(ctx, full)
	if err != nil {
		return builtMessage{}, err
	}
	info, err := describe(literal)
	if err != nil {
		log.Printf("Message %v can't be served as built (%v): served as an attachment of a note", full.ID, err)
		literal, err = safeMessage(full, literal, err)
		if err != nil {
			return builtMessage{}, fmt.Errorf("cannot build message %v: %w", full.ID, err)
		}
		if info, err = describe(literal); err != nil {
			return builtMessage{}, fmt.Errorf("cannot build message %v, even as a note: %w", full.ID, err)
		}
	}
	info.Fallback = fallback
	return builtMessage{literal: literal, info: info}, nil
}

// buildLiteral returns the RFC 822 bytes of full, and whether part of it is
// served still encrypted. As bridge (BuildRFC822Into): a message with
// attachments is a multipart/mixed of its body and attachments; a body that
// is a MIME message itself (received as PGP/MIME, MIMEType multipart/mixed)
// is that message; a body encrypted to no key of the account is passed
// through as PGP/MIME. Other bodies are a multipart/mixed of one part, as
// peroxide always served them.
func (u *user) buildLiteral(ctx context.Context, full *protonmail.Message) ([]byte, bool, error) {
	switch {
	case len(full.Attachments) > 0:
		return u.buildMultipart(ctx, full)
	case full.MIMEType == "multipart/mixed":
		return u.buildPGPMIME(ctx, full)
	case u.passThrough(full):
		log.Printf("Message %v is encrypted to a key Proton doesn't have: passed through as PGP/MIME", full.ID)
		literal, err := passThroughMessage(full)
		return literal, true, err
	default:
		return u.buildMultipart(ctx, full)
	}
}

func (u *user) buildMultipart(ctx context.Context, full *protonmail.Message) ([]byte, bool, error) {
	// The body and the attachments first: their signatures' verdicts go in
	// the message's header, which is written first.
	ih, body, verdict, fallback, err := u.inlinePart(ctx, full)
	if err != nil {
		return nil, false, err
	}
	type part struct {
		h    message.Header
		body []byte
	}
	parts := make([]part, 0, len(full.Attachments))
	verdicts := make([]sigVerdict, 0, len(full.Attachments))
	for _, att := range full.Attachments {
		h, body, v, attFallback, err := u.attachmentPart(ctx, att, senderAddress(full))
		if err != nil {
			return nil, false, err
		}
		fallback = fallback || attFallback
		parts = append(parts, part{h, body})
		verdicts = append(verdicts, v)
	}

	h := messageHeader(full)
	setVerdict(&h, full, verdict, attachmentsSummary(verdicts))
	var b bytes.Buffer
	w, err := message.CreateWriter(&b, h)
	if err != nil {
		return nil, false, err
	}
	if err := writePart(w, ih, body); err != nil {
		return nil, false, err
	}
	for _, p := range parts {
		if err := writePart(w, p.h, p.body); err != nil {
			return nil, false, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, false, err
	}
	return b.Bytes(), fallback, nil
}

// inlinePart returns the header and the body, decrypted, of the inline part
// of msg, and the verdict of its signature. It is read whole, so that a
// failure anywhere in it, the integrity check at its end included, is known
// before anything is built: the body is then served as bridge does, still
// armored, with a comment saying why (writeCustomTextPart), and fallback is
// set. The error is a failure to fetch the sender's keys that may pass: the
// message isn't built now.
func (u *user) inlinePart(ctx context.Context, msg *protonmail.Message) (h message.Header, body []byte, verdict sigVerdict, fallback bool, err error) {
	body, verdict, _, decryptErr, lookupErr := u.decryptAndVerify(ctx, msg)
	if lookupErr != nil {
		return h, nil, sigVerdict{}, false, lookupErr
	}
	if decryptErr == nil {
		return inlineHeader(msg), body, verdict, false, nil
	}
	err = u.explainDecryptError(msg, decryptErr)
	log.Printf("Message %v served encrypted: %v", msg.ID, err)

	// Armor is text whatever the body was: a multipart type would make
	// clients parse it as parts.
	contentType := "text/plain"
	if strings.HasPrefix(msg.MIMEType, "text/") {
		contentType = msg.MIMEType
	}
	var ih mail.InlineHeader
	ih.SetContentType(contentType, nil)
	ih.Set("Content-Transfer-Encoding", "7bit")
	armored, armorErr := rearmor(strings.NewReader(msg.Body), "This message could not be decrypted: "+err.Error())
	if armorErr != nil {
		// Not armor at all: what Proton stored is all there is.
		log.Printf("cannot armor message %v again: %v", msg.ID, armorErr)
		return ih.Header, []byte(msg.Body), sigVerdict{}, true, nil
	}
	return ih.Header, armored, sigVerdict{}, true, nil
}

// explainDecryptError says why a body encrypted to no unlocked key is so.
func (u *user) explainDecryptError(msg *protonmail.Message, err error) error {
	if !errors.Is(err, pgperrors.ErrKeyIncorrect) {
		return err
	}
	if toAccount, _ := u.encryptedToAccount(msg); toAccount {
		return fmt.Errorf("encrypted to a key of the account that peroxide hasn't unlocked (one added since it started?): %w", err)
	}
	return fmt.Errorf("encrypted end to end to a key Proton doesn't have: %w", err)
}

// setVerdict writes in h, as SignatureHeader, the verdict of the body's
// signature and the summary of the attachments' (attachmentsSummary, ""
// for none). A message whose body isn't signed and whose attachments,
// if any, aren't either has no field; one whose body isn't signed but an
// attachment is gets "none" for the body.
func setVerdict(h *message.Header, full *protonmail.Message, v sigVerdict, attachments string) {
	if v.result == "" && (attachments == "" || attachments == sigNone) {
		return
	}
	if v.result == "" {
		v.result = sigNone
	}
	h.Set(SignatureHeader, v.headerValue(senderAddress(full), attachments))
}

// senderAddress is the address of full's sender, "" if it has none.
func senderAddress(full *protonmail.Message) string {
	if full.Sender == nil {
		return ""
	}
	return full.Sender.Address
}

// attachmentPart downloads att and returns its part: decrypted, or, when no
// unlocked key opens it or it fails to decrypt, as bridge does
// (writeCustomAttachmentPart): still encrypted, as an armored <name>.pgp file
// whose comment says why, so that the client may decrypt it. It is decrypted
// whole first, so that a failure at its end still gives the armored part. An
// attached message (message/rfc822) has no transfer encoding, as in bridge,
// so that its structure can be described.
//
// A decrypted attachment's detached signature is checked against the
// sender's keys (verifyAttachment), and the verdict written in the part's
// own header as SignatureHeader, and returned for the message's summary.
// The errors are a failed download, and a failure to fetch the sender's
// keys that may pass.
func (u *user) attachmentPart(ctx context.Context, att *protonmail.Attachment, sender string) (h message.Header, body []byte, v sigVerdict, fallback bool, err error) {
	rc, err := u.c.GetAttachment(ctx, att.ID)
	if err != nil {
		return h, nil, sigVerdict{}, false, err
	}
	ciphertext, err := io.ReadAll(rc)
	if closeErr := rc.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return h, nil, sigVerdict{}, false, fmt.Errorf("cannot download attachment %v: %w", att.ID, err)
	}

	why := "no key peroxide has unlocked opens it"
	if att.Decryptable(u.privateKeys) {
		md, err := att.Read(bytes.NewReader(ciphertext), u.privateKeys, nil)
		if err == nil {
			body, err = io.ReadAll(md.UnverifiedBody)
		}
		if err == nil {
			v, lookupErr := u.verifyAttachment(ctx, att, body, sender)
			if lookupErr != nil {
				return h, nil, sigVerdict{}, false, lookupErr
			}
			h := attachmentHeader(att)
			if v.result != "" {
				// The signer is the message's sender: header.from is on the
				// message's field.
				h.Set(SignatureHeader, v.headerValue("", ""))
			}
			return h, body, v, false, nil
		}
		why = err.Error()
	}

	log.Printf("Attachment %v of message %v served encrypted: %v", att.ID, att.MessageID, why)
	var b bytes.Buffer
	if err := writeArmored(&b, att.PGPMessage(bytes.NewReader(ciphertext)), "This attachment could not be decrypted: "+why); err != nil {
		return h, nil, sigVerdict{}, false, fmt.Errorf("cannot armor attachment %v: %w", att.ID, err)
	}
	var ah mail.AttachmentHeader
	ah.SetContentType("application/octet-stream", nil)
	ah.Set("Content-Transfer-Encoding", "base64")
	ah.SetFilename(armoredAttachmentName(att))
	// Not decrypted, so not checked: counted as not signed.
	return ah.Header, b.Bytes(), sigVerdict{}, true, nil
}

// buildPGPMIME builds a message whose decrypted body is a MIME message (one
// received as PGP/MIME), as bridge does (buildPGPRFC822): the body's header
// fields join the message's, which has no Content-Type of its own then. A
// body signed by a key not unlocked here, the sender's, becomes
// multipart/signed (RFC 3156 section 5) with the signature as second part. A
// body that can't be decrypted is passed through as PGP/MIME, as bridge does
// (buildPGPMIMEFallbackRFC822). peroxide's verdict on the signature goes in
// the header too, for clients that don't verify themselves.
func (u *user) buildPGPMIME(ctx context.Context, full *protonmail.Message) ([]byte, bool, error) {
	plain, verdict, sigs, decryptErr, lookupErr := u.decryptAndVerify(ctx, full)
	if lookupErr != nil {
		return nil, false, lookupErr
	}
	if decryptErr != nil {
		log.Printf("Message %v passed through as PGP/MIME: %v", full.ID, u.explainDecryptError(full, decryptErr))
		literal, err := passThroughMessage(full)
		return literal, true, err
	}

	h := messageHeader(full)
	setVerdict(&h, full, verdict, "")
	// Written by hand below, not by message.CreateWriter, which adds it.
	h.Set("MIME-Version", "1.0")
	if len(sigs) > 0 {
		literal, err := multipartSigned(full, h, plain, sigs[0])
		return literal, false, err
	}

	bodyHeader, bodyData := splitEntity(plain)
	h.Del("Content-Type")
	for f := bodyHeader.Fields(); f.Next(); {
		if !h.Has(f.Key()) && !strings.EqualFold(f.Key(), SignatureHeader) {
			h.Set(f.Key(), f.Value())
		}
	}
	var b bytes.Buffer
	if err := textproto.WriteHeader(&b, h.Header); err != nil {
		return nil, false, err
	}
	b.Write(bodyData)
	return b.Bytes(), false, nil
}

// splitEntity splits a MIME entity into its header and body. One whose start
// isn't a header is all body, as bridge reads it (readHeaderBody).
func splitEntity(entity []byte) (textproto.Header, []byte) {
	r := bytes.NewReader(entity)
	br := bufio.NewReader(r)
	h, err := textproto.ReadHeader(br)
	if err != nil {
		return textproto.Header{}, entity
	}
	// What br took from r and hasn't handed out yet is body too.
	return h, entity[len(entity)-r.Len()-br.Buffered():]
}

// multipartSigned writes entity, signed by sig, as multipart/signed under h.
// The entity is written byte for byte, as signed, so that the client can
// verify it; bridge writes its header again instead.
func multipartSigned(full *protonmail.Message, h message.Header, entity []byte, sig *packet.Signature) ([]byte, error) {
	var sigBytes bytes.Buffer
	aw, err := armor.Encode(&sigBytes, "PGP SIGNATURE", nil)
	if err != nil {
		return nil, err
	}
	if err := sig.Serialize(aw); err != nil {
		return nil, fmt.Errorf("cannot write the signature of message %v: %w", full.ID, err)
	}
	if err := aw.Close(); err != nil {
		return nil, err
	}

	boundary := "signed-" + msgBoundary(full)
	h.SetContentType("multipart/signed", map[string]string{
		"micalg":   micalg(sig),
		"protocol": "application/pgp-signature",
		"boundary": boundary,
	})
	var sigHeader message.Header
	sigHeader.SetContentType("application/pgp-signature", map[string]string{"name": "OpenPGP_signature.asc"})
	sigHeader.SetContentDisposition("attachment", map[string]string{"filename": "OpenPGP_signature"})
	sigHeader.Set("Content-Description", "OpenPGP digital signature")

	var b bytes.Buffer
	if err := textproto.WriteHeader(&b, h.Header); err != nil {
		return nil, err
	}
	b.WriteString("--" + boundary + "\r\n")
	b.Write(entity)
	b.WriteString("\r\n--" + boundary + "\r\n")
	if err := textproto.WriteHeader(&b, sigHeader.Header); err != nil {
		return nil, err
	}
	b.Write(sigBytes.Bytes())
	b.WriteString("\r\n--" + boundary + "--\r\n")
	return b.Bytes(), nil
}

// micalg names the hash of sig as RFC 3156 section 5 and RFC 9580 section
// 9.5 do: "pgp-" and the lower-case name, e.g. pgp-sha256, pgp-sha3-256.
// bridge writes the hash's Go name ("SHA-256") instead.
func micalg(sig *packet.Signature) string {
	name := strings.ToLower(sig.Hash.String())
	name = strings.Replace(name, "sha-", "sha", 1)
	name = strings.Replace(name, "ripemd-", "ripemd", 1)
	return "pgp-" + name
}

// safeMessage wraps literal, which go-imap can't parse, in a message it can:
// a note saying why, and literal as an attachment, so nothing is lost.
func safeMessage(full *protonmail.Message, literal []byte, cause error) ([]byte, error) {
	h := messageHeader(full)
	h.SetContentType("multipart/mixed", map[string]string{"boundary": "safe-" + msgBoundary(full)})
	var b bytes.Buffer
	w, err := message.CreateWriter(&b, h)
	if err != nil {
		return nil, err
	}
	var note message.Header
	note.SetContentType("text/plain", map[string]string{"charset": "utf-8"})
	note.Set("Content-Transfer-Encoding", "quoted-printable")
	text := "peroxide could not make a readable message of this mail (" + cause.Error() +
		"). It is attached as built, in message.eml.\r\n"
	if err := writePart(w, note, []byte(text)); err != nil {
		return nil, err
	}
	var att mail.AttachmentHeader
	att.SetContentType("application/octet-stream", nil)
	att.Set("Content-Transfer-Encoding", "base64")
	att.SetFilename("message.eml")
	if err := writePart(w, att.Header, literal); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// describe computes what FETCH derives from a built message: its extended
// BODYSTRUCTURE, by go-imap's parser, and its ENVELOPE. An error means the
// bytes can't be served.
func describe(literal []byte) (database.BodyInfo, error) {
	br := bufio.NewReader(bytes.NewReader(literal))
	h, err := textproto.ReadHeader(br)
	if err != nil {
		return database.BodyInfo{}, fmt.Errorf("cannot read its header: %w", err)
	}
	bs, err := backendutil.FetchBodyStructure(h, br, true)
	if err != nil {
		return database.BodyInfo{}, fmt.Errorf("cannot read its structure: %w", err)
	}
	return database.BodyInfo{BodyStructure: bs, Envelope: envelope(h)}, nil
}

// envelope returns the ENVELOPE of a message with header h (RFC 3501
// section 7.4.2): Sender and Reply-To default to From. Fields that don't
// parse are NIL, as in go-imap's backendutil.FetchEnvelope, which reads
// encoded words undecoded.
func envelope(h textproto.Header) *imap.Envelope {
	mh := mail.Header{Header: message.Header{Header: h}}
	env := &imap.Envelope{
		InReplyTo: h.Get("In-Reply-To"),
		MessageId: h.Get("Message-Id"),
	}
	env.Date, _ = mh.Date()
	if s, err := mh.Subject(); err == nil {
		env.Subject = s
	} else {
		env.Subject = h.Get("Subject")
	}
	list := func(k string) []*imap.Address {
		addrs, _ := mh.AddressList(k)
		if len(addrs) == 0 {
			return nil
		}
		l := make([]*imap.Address, len(addrs))
		for i, a := range addrs {
			l[i] = imapAddress(&protonmail.MessageAddress{Name: a.Name, Address: a.Address})
		}
		return l
	}
	env.From = list("From")
	env.Sender = list("Sender")
	if len(env.Sender) == 0 {
		env.Sender = env.From
	}
	env.ReplyTo = list("Reply-To")
	if len(env.ReplyTo) == 0 {
		env.ReplyTo = env.From
	}
	env.To = list("To")
	env.Cc = list("Cc")
	env.Bcc = list("Bcc")
	return env
}

// bodySection returns section of the stored message literal, whose structure
// is bs, as go-imap's backendutil computes it. A part the message doesn't
// have is an empty string, as Dovecot and gluon answer, rather than failing
// the FETCH.
func bodySection(literal []byte, bs *imap.BodyStructure, section *imap.BodySectionName) (imap.Literal, error) {
	if !sectionExists(bs, section.Path) {
		return bytes.NewReader(nil), nil
	}
	br := bufio.NewReader(bytes.NewReader(literal))
	h, err := textproto.ReadHeader(br)
	if err != nil {
		return nil, err
	}
	return backendutil.FetchBodySection(h, br, section)
}

// sectionExists reports whether backendutil.FetchBodySection finds the part
// path names in a message of structure bs: it descends multiparts only (not
// into an attached message/rfc822), and takes part 1 of a message that isn't
// one for the message itself (RFC 3501 section 6.4.5).
func sectionExists(bs *imap.BodyStructure, path []int) bool {
	for _, n := range path {
		if !strings.EqualFold(bs.MIMEType, "multipart") {
			return len(path) == 1 && n == 1
		}
		if n < 1 || n > len(bs.Parts) {
			return false
		}
		bs = bs.Parts[n-1]
	}
	return true
}

// withoutExtension returns a copy of bs without extension data at any
// level, for FETCH BODY; go-imap clears it at the top level only.
func withoutExtension(bs *imap.BodyStructure) *imap.BodyStructure {
	c := *bs
	c.Extended = false
	c.Parts = make([]*imap.BodyStructure, len(bs.Parts))
	for i, p := range bs.Parts {
		c.Parts[i] = withoutExtension(p)
	}
	if bs.BodyStructure != nil {
		c.BodyStructure = withoutExtension(bs.BodyStructure)
	}
	return &c
}
