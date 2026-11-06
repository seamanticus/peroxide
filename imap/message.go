package imap

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/emersion/go-imap"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"

	"github.com/seamanticus/peroxide/protonmail"
)

func messageID(msg *protonmail.Message) string {
	if msg.ExternalID != "" {
		return msg.ExternalID
	} else {
		return msg.ID + "@protonmail.com"
	}
}

func formatHeader(h mail.Header) string {
	var b bytes.Buffer
	fields := h.Fields()
	for fields.Next() {
		b.WriteString(fmt.Sprintf("%s: %s\r\n", fields.Key(), fields.Value()))
	}
	return b.String()
}

func protonmailAddressList(addresses []*mail.Address) []*protonmail.MessageAddress {
	l := make([]*protonmail.MessageAddress, len(addresses))
	for i, addr := range addresses {
		l[i] = &protonmail.MessageAddress{
			Name:    addr.Name,
			Address: addr.Address,
		}
	}
	return l
}

func imapAddress(addr *protonmail.MessageAddress) *imap.Address {
	parts := strings.SplitN(addr.Address, "@", 2)
	if len(parts) < 2 {
		parts = append(parts, "")
	}

	return &imap.Address{
		PersonalName: addr.Name,
		MailboxName:  parts[0],
		HostName:     parts[1],
	}
}

func imapAddressList(addresses []*protonmail.MessageAddress) []*imap.Address {
	l := make([]*imap.Address, len(addresses))
	for i, addr := range addresses {
		l[i] = imapAddress(addr)
	}
	return l
}

func fetchEnvelope(msg *protonmail.Message) *imap.Envelope {
	env := &imap.Envelope{
		Date:    msg.Time.Time(),
		Subject: msg.Subject,
		// TODO: Sender
		To:      imapAddressList(msg.ToList),
		Cc:      imapAddressList(msg.CCList),
		Bcc:     imapAddressList(msg.BCCList),
		ReplyTo: imapAddressList(msg.ReplyTos),
		// TODO: InReplyTo
		MessageId: messageID(msg),
	}
	// Sender is decoded from the API and may be missing: From is then NIL.
	if msg.Sender != nil {
		env.From = []*imap.Address{imapAddress(msg.Sender)}
	}
	return env
}

func msgBoundary(msg *protonmail.Message) string {
	h := sha1.Sum([]byte(msg.ID))
	return hex.EncodeToString(h[:])
}

func hasLabel(msg *protonmail.Message, labelID string) bool {
	for _, id := range msg.LabelIDs {
		if labelID == id {
			return true
		}
	}
	return false
}

func splitMIMEType(t string) (string, string) {
	parts := strings.SplitN(t, "/", 2)
	if len(parts) < 2 {
		return "text", "plain"
	}
	return parts[0], parts[1]
}

func (mbox *mailbox) fetchBodyStructure(msg *protonmail.Message, extended bool) (*imap.BodyStructure, error) {
	// The local database keeps neither the body nor its MIME type: guessing
	// them made a text/plain body a text/html part of size 0.
	msg, err := mbox.u.fullMessage(msg.ID)
	if err != nil {
		return nil, err
	}

	if mbox.passThrough(msg) {
		return mbox.passThroughBodyStructure(msg, extended)
	}

	inlineHeader, body, err := mbox.encodedInlinePart(msg)
	if err != nil {
		return nil, err
	}
	// Described from the header the part is sent with, which is the
	// decrypted body's or, when that fails, the armored fallback's.
	inlineType, inlineSubType := "text", "plain"
	var params map[string]string
	if t, p, err := inlineHeader.ContentType(); err == nil {
		inlineType, inlineSubType = splitMIMEType(t)
		params = p
	}
	inline := &imap.BodyStructure{
		MIMEType:    inlineType,
		MIMESubType: inlineSubType,
		Params:      params,
		Encoding:    inlineHeader.Get("Content-Transfer-Encoding"),
		Extended:    extended,
		Disposition: "inline",
	}
	// Size and Lines are of the part as sent, after its encoding.
	inline.Size = uint32(len(body))
	if inlineType == "text" {
		inline.Lines = uint32(bytes.Count(body, []byte("\n")))
		if len(body) > 0 && body[len(body)-1] != '\n' {
			inline.Lines++
		}
	}
	parts := []*imap.BodyStructure{inline}

	for _, att := range msg.Attachments {
		attType, attSubType := splitMIMEType(att.MIMEType)
		name := att.Name
		if !att.Decryptable(mbox.u.privateKeys) {
			attType, attSubType, name = "application", "octet-stream", armoredAttachmentName(att)
		}
		parts = append(parts, &imap.BodyStructure{
			MIMEType:          attType,
			MIMESubType:       attSubType,
			Id:                att.ContentID,
			Encoding:          "base64",
			Size:              uint32(att.Size),
			Extended:          extended,
			Disposition:       "attachment",
			DispositionParams: map[string]string{"filename": name},
		})
	}

	return &imap.BodyStructure{
		MIMEType:    "multipart",
		MIMESubType: "mixed",
		Params:      map[string]string{"boundary": msgBoundary(msg)},
		// TODO: Size
		Parts:    parts,
		Extended: extended,
	}, nil
}

// inlinePart returns the header and the body, decrypted, of the inline part
// of msg (not passed through). It is read whole, so that a failure anywhere
// in it, the integrity check at its end included, is known before anything
// is sent: the body is then served as bridge does, still armored, with a
// comment saying why (writeCustomTextPart). A FETCH thus never fails on one
// message, and the client may still decrypt it.
func (mbox *mailbox) inlinePart(msg *protonmail.Message) (message.Header, []byte) {
	body, err := mbox.decryptBody(msg)
	if err == nil {
		return inlineHeader(msg), body
	}
	if errors.Is(err, pgperrors.ErrKeyIncorrect) {
		if toAccount, _ := mbox.encryptedToAccount(msg); toAccount {
			err = fmt.Errorf("encrypted to a key of the account that peroxide hasn't unlocked (one added since it started?): %w", err)
		} else {
			// Not passed through as PGP/MIME as it has attachments.
			err = fmt.Errorf("encrypted end to end to a key Proton doesn't have: %w", err)
		}
	}
	log.Printf("Message %v served encrypted: %v", msg.ID, err)

	// Armor is text whatever the body was: a multipart type would make
	// clients parse it as parts.
	contentType := "text/plain"
	if strings.HasPrefix(msg.MIMEType, "text/") {
		contentType = msg.MIMEType
	}
	var h mail.InlineHeader
	h.SetContentType(contentType, nil)
	h.Set("Content-Transfer-Encoding", "7bit")
	armored, armorErr := rearmor(strings.NewReader(msg.Body), "This message could not be decrypted: "+err.Error())
	if armorErr != nil {
		// Not armor at all: what Proton stored is all there is.
		log.Printf("cannot armor message %v again: %v", msg.ID, armorErr)
		return h.Header, []byte(msg.Body)
	}
	return h.Header, armored
}

func (mbox *mailbox) decryptBody(msg *protonmail.Message) ([]byte, error) {
	md, err := msg.Read(mbox.u.privateKeys, nil)
	if err != nil {
		return nil, err
	}
	// TODO: check signature
	return io.ReadAll(md.UnverifiedBody)
}

// rearmor returns the armored PGP message r, decoded and armored again with
// comment as its Comment header (RFC 9580 section 6.2).
func rearmor(r io.Reader, comment string) ([]byte, error) {
	block, err := armor.Decode(r)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := writeArmored(&b, block.Body, comment); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// writeArmored writes the binary PGP message r to w, armored, with comment
// on one line as its Comment header.
func writeArmored(w io.Writer, r io.Reader, comment string) error {
	comment = strings.Join(strings.Fields(comment), " ")
	aw, err := armor.Encode(w, "PGP MESSAGE", map[string]string{"Comment": comment})
	if err != nil {
		return err
	}
	if _, err := io.Copy(aw, r); err != nil {
		return err
	}
	return aw.Close()
}

// encodedInlinePart returns the header of the inline part of msg and its body
// as written in a FETCH, encoded as the header says.
func (mbox *mailbox) encodedInlinePart(msg *protonmail.Message) (message.Header, []byte, error) {
	h, body := mbox.inlinePart(msg)
	var b bytes.Buffer
	w, err := message.CreateWriter(&b, h)
	if err != nil {
		return h, nil, err
	}
	b.Reset() // keep the body only
	if _, err := w.Write(body); err != nil {
		return h, nil, err
	}
	if err := w.Close(); err != nil {
		return h, nil, err
	}
	return h, b.Bytes(), nil
}

// attachmentPartHeader returns the header of the part of att. One no unlocked
// key of the account opens is served as bridge does, still encrypted, as an
// armored <name>.pgp file whose comment says why (writeCustomAttachmentPart),
// so the client may decrypt it. Only att's key packets are read for that.
func (mbox *mailbox) attachmentPartHeader(att *protonmail.Attachment) message.Header {
	if att.Decryptable(mbox.u.privateKeys) {
		return attachmentHeader(att)
	}
	var h mail.AttachmentHeader
	h.SetContentType("application/octet-stream", nil)
	h.Set("Content-Transfer-Encoding", "base64")
	h.SetFilename(armoredAttachmentName(att))
	return h.Header
}

// attachmentBody downloads att and returns the body of its part, decrypted
// or armored as attachmentPartHeader says.
func (mbox *mailbox) attachmentBody(att *protonmail.Attachment) (io.Reader, error) {
	rc, err := mbox.u.c.GetAttachment(att.ID)
	if err != nil {
		return nil, err
	}

	if att.Decryptable(mbox.u.privateKeys) {
		md, err := att.Read(rc, mbox.u.privateKeys, nil)
		if err != nil {
			return nil, err
		}
		// TODO: check signature
		return md.UnverifiedBody, nil
	}

	defer rc.Close()
	log.Printf("Attachment %v of message %v served encrypted: no unlocked key of the account opens it", att.ID, att.MessageID)
	var b bytes.Buffer
	if err := writeArmored(&b, att.PGPMessage(rc), "This attachment could not be decrypted: no key peroxide has unlocked opens it"); err != nil {
		return nil, fmt.Errorf("cannot armor attachment %v: %w", att.ID, err)
	}
	return &b, nil
}

func armoredAttachmentName(att *protonmail.Attachment) string {
	return att.Name + ".pgp"
}

func inlineHeader(msg *protonmail.Message) message.Header {
	var h mail.InlineHeader
	if msg.MIMEType != "" {
		h.SetContentType(msg.MIMEType, map[string]string{"charset": "utf-8"})
	} else {
		log.Println("Sending an inline header without its proper MIME type")
	}
	h.Set("Content-Transfer-Encoding", "quoted-printable")
	return h.Header
}

func attachmentHeader(att *protonmail.Attachment) message.Header {
	var h mail.AttachmentHeader
	h.SetContentType(att.MIMEType, nil)
	h.Set("Content-Transfer-Encoding", "base64")
	h.SetFilename(att.Name)
	if att.ContentID != "" {
		h.Set("Content-Id", att.ContentID)
	}
	return h.Header
}

func mailAddress(addr *protonmail.MessageAddress) *mail.Address {
	return &mail.Address{
		Name:    addr.Name,
		Address: addr.Address,
	}
}

func mailAddressList(addresses []*protonmail.MessageAddress) []*mail.Address {
	l := make([]*mail.Address, len(addresses))
	for i, addr := range addresses {
		l[i] = mailAddress(addr)
	}
	return l
}

func messageHeader(msg *protonmail.Message) message.Header {
	typeParams := map[string]string{"boundary": msgBoundary(msg)}

	var h mail.Header
	h.SetContentType("multipart/mixed", typeParams)
	h.SetDate(msg.Time.Time())
	h.SetSubject(msg.Subject)
	if msg.Sender != nil {
		h.SetAddressList("From", []*mail.Address{mailAddress(msg.Sender)})
	}
	if len(msg.ReplyTos) > 0 {
		h.SetAddressList("Reply-To", mailAddressList(msg.ReplyTos))
	}
	if len(msg.ToList) > 0 {
		h.SetAddressList("To", mailAddressList(msg.ToList))
	}
	if len(msg.CCList) > 0 {
		h.SetAddressList("Cc", mailAddressList(msg.CCList))
	}
	if len(msg.BCCList) > 0 {
		h.SetAddressList("Bcc", mailAddressList(msg.BCCList))
	}
	// TODO: In-Reply-To
	h.Set("Message-Id", fmt.Sprintf("<%s>", messageID(msg)))
	return h.Header
}

func (mbox *mailbox) fetchBodySection(msg *protonmail.Message, section *imap.BodySectionName) (imap.Literal, error) {
	// Only the encrypted body says whether the message is passed through, and
	// then its header too differs (its Content-Type), so every section of a
	// PGP message needs the full message. The cache keeps the cost to one
	// request per message.
	full, err := mbox.u.fullMessage(msg.ID)
	if err != nil {
		return nil, err
	}
	if mbox.passThrough(full) {
		return mbox.passThroughBodySection(full, section)
	}

	b := new(bytes.Buffer)

	if len(section.Path) == 0 {
		w, err := message.CreateWriter(b, messageHeader(full))
		if err != nil {
			return nil, err
		}

		if section.Specifier == imap.TextSpecifier {
			b.Reset()
		}

		switch section.Specifier {
		case imap.EntireSpecifier, imap.TextSpecifier:
			ih, body := mbox.inlinePart(full)
			pw, err := w.CreatePart(ih)
			if err != nil {
				return nil, err
			}
			if _, err := pw.Write(body); err != nil {
				return nil, err
			}
			pw.Close()

			for _, att := range full.Attachments {
				pw, err := w.CreatePart(mbox.attachmentPartHeader(att))
				if err != nil {
					return nil, err
				}
				pr, err := mbox.attachmentBody(att)
				if err != nil {
					return nil, err
				}
				if _, err := io.Copy(pw, pr); err != nil {
					return nil, err
				}
				pw.Close()
			}
		}

		w.Close()
	} else {
		if len(section.Path) > 1 {
			return nil, errors.New("invalid body section path length")
		}

		var h message.Header
		var getBody func() (io.Reader, error)
		if part := section.Path[0]; part == 1 {
			ih, body := mbox.inlinePart(full)
			h = ih
			getBody = func() (io.Reader, error) {
				return bytes.NewReader(body), nil
			}
		} else {
			i := part - 2
			if i < 0 || i >= len(full.Attachments) {
				return nil, errors.New("invalid attachment section path")
			}

			att := full.Attachments[i]
			h = mbox.attachmentPartHeader(att)
			getBody = func() (io.Reader, error) {
				return mbox.attachmentBody(att)
			}
		}

		w, err := message.CreateWriter(b, h)
		if err != nil {
			return nil, err
		}

		switch section.Specifier {
		case imap.TextSpecifier:
			// The header hasn't been requested. Discard it.
			b.Reset()
		case imap.EntireSpecifier:
			if len(section.Path) > 0 {
				// When selecting a specific part by index, IMAP servers
				// return only the text, not the associated MIME header.
				b.Reset()
			}
		}

		// Write the body, if requested
		switch section.Specifier {
		case imap.EntireSpecifier, imap.TextSpecifier:
			r, err := getBody()
			if err != nil {
				return nil, err
			}

			if _, err := io.Copy(w, r); err != nil {
				return nil, err
			}
		}

		w.Close()
	}

	var l imap.Literal = b
	if section.Partial != nil {
		l = bytes.NewReader(section.ExtractPartial(b.Bytes()))
	}

	return l, nil
}

func createMessage(c *protonmail.Client, u *protonmail.User, privateKeys openpgp.EntityList, addrs []*protonmail.Address, r io.Reader) (*protonmail.Message, error) {
	// Parse the incoming MIME message header
	mr, err := mail.CreateReader(r)
	if err != nil {
		return nil, err
	}

	subject, _ := mr.Header.Subject()
	fromList, _ := mr.Header.AddressList("From")
	toList, _ := mr.Header.AddressList("To")
	ccList, _ := mr.Header.AddressList("Cc")
	bccList, _ := mr.Header.AddressList("Bcc")

	if len(fromList) != 1 {
		return nil, errors.New("the From field must contain exactly one address")
	}
	if len(toList) == 0 && len(ccList) == 0 && len(bccList) == 0 {
		return nil, errors.New("no recipient specified")
	}

	fromAddrStr := fromList[0].Address
	var fromAddr *protonmail.Address
	for _, addr := range addrs {
		if strings.EqualFold(addr.Email, fromAddrStr) {
			fromAddr = addr
			break
		}
	}
	if fromAddr == nil {
		return nil, errors.New("unknown sender address")
	}
	if len(fromAddr.Keys) == 0 {
		return nil, errors.New("sender address has no private key")
	}

	// TODO: get appropriate private key
	encryptedPrivateKey, err := fromAddr.Keys[0].Entity()
	if err != nil {
		return nil, fmt.Errorf("cannot parse sender private key: %v", err)
	}

	var privateKey *openpgp.Entity
	for _, e := range privateKeys {
		if e.PrimaryKey.KeyId == encryptedPrivateKey.PrimaryKey.KeyId {
			privateKey = e
			break
		}
	}
	if privateKey == nil {
		return nil, errors.New("sender address key hasn't been decrypted")
	}

	msg := &protonmail.Message{
		ToList:    protonmailAddressList(toList),
		CCList:    protonmailAddressList(ccList),
		BCCList:   protonmailAddressList(bccList),
		Subject:   subject,
		Header:    formatHeader(mr.Header),
		AddressID: fromAddr.ID,
		Sender: &protonmail.MessageAddress{
			Address: fromAddrStr,
			Name:    fromList[0].Name,
		},
	}

	// Create an empty draft
	plaintext, err := msg.Encrypt([]*openpgp.Entity{privateKey}, privateKey)
	if err != nil {
		return nil, err
	}
	if err := plaintext.Close(); err != nil {
		return nil, err
	}

	// TODO: parentID from In-Reply-To
	msg, err = c.CreateDraftMessage(msg, "")
	if err != nil {
		return nil, fmt.Errorf("cannot create draft message: %v", err)
	}

	var body *bytes.Buffer
	var bodyType string

	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}

		switch h := p.Header.(type) {
		case *mail.InlineHeader:
			t, _, err := h.ContentType()
			if err != nil {
				break
			}

			if body != nil && t != "text/html" {
				break
			}

			body = &bytes.Buffer{}
			bodyType = t
			if _, err := io.Copy(body, p.Body); err != nil {
				return nil, err
			}
		case *mail.AttachmentHeader:
			t, _, err := h.ContentType()
			if err != nil {
				break
			}

			filename, err := h.Filename()
			if err != nil {
				break
			}

			att := &protonmail.Attachment{
				MessageID: msg.ID,
				Name:      filename,
				MIMEType:  t,
				ContentID: h.Get("Content-Id"),
				// TODO: Header
			}

			_, err = att.GenerateKey([]*openpgp.Entity{privateKey})
			if err != nil {
				return nil, fmt.Errorf("cannot generate attachment key: %v", err)
			}

			pr, pw := io.Pipe()

			go func() {
				cleartext, err := att.Encrypt(pw, privateKey)
				if err != nil {
					pw.CloseWithError(err)
					return
				}
				if _, err := io.Copy(cleartext, p.Body); err != nil {
					pw.CloseWithError(err)
					return
				}
				pw.CloseWithError(cleartext.Close())
			}()

			att, err = c.CreateAttachment(att, pr)
			if err != nil {
				return nil, fmt.Errorf("cannot upload attachment: %v", err)
			}

			msg.Attachments = append(msg.Attachments, att)
		}
	}

	if body == nil {
		return nil, errors.New("message doesn't contain a body part")
	}

	// Encrypt the body and update the draft
	msg.MIMEType = bodyType
	plaintext, err = msg.Encrypt([]*openpgp.Entity{privateKey}, privateKey)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(plaintext, body); err != nil {
		return nil, err
	}
	if err := plaintext.Close(); err != nil {
		return nil, err
	}

	if _, err := c.UpdateDraftMessage(msg); err != nil {
		return nil, fmt.Errorf("cannot update draft message: %v", err)
	}

	return msg, nil
}
