package imap

import (
	"bufio"
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
	"github.com/emersion/go-imap"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"

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

func msgBoundary(msg *protonmail.Message) string {
	h := sha1.Sum([]byte(msg.ID))
	return hex.EncodeToString(h[:])
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

// attachmentHeader returns the header of the part of att. An attached message
// (message/rfc822) has no transfer encoding, as in bridge
// (getAttachmentPartHeader): RFC 2046 section 5.2.1 allows only 7bit, 8bit
// or binary for it, and its structure can then be described in
// BODYSTRUCTURE (RFC 3501 section 7.4.2). Any other is base64.
func attachmentHeader(att *protonmail.Attachment) message.Header {
	var h mail.AttachmentHeader
	h.SetContentType(att.MIMEType, nil)
	if !strings.EqualFold(att.MIMEType, "message/rfc822") {
		h.Set("Content-Transfer-Encoding", "base64")
	}
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

// messageHeader returns msg's header, with peroxide's multipart/mixed
// Content-Type. A full message (GetMessage) carries the header Proton
// received: it is kept, as proton-bridge does, so threading fields
// (In-Reply-To, References) and the sent Date survive, with the address and
// subject fields set from the metadata. Message metadata carries no header:
// it is built from the metadata alone.
func messageHeader(msg *protonmail.Message) message.Header {
	typeParams := map[string]string{"boundary": msgBoundary(msg)}

	var h mail.Header
	if msg.Header != "" {
		orig, err := originalHeader(msg.Header)
		if err != nil {
			log.Printf("Message %v: ignoring its header from Proton: %v", msg.ID, err)
		} else {
			h = mail.Header{Header: message.Header{Header: orig}}
			// The body is peroxide's multipart, not the one sent.
			h.Del("Content-Transfer-Encoding")
			// Only peroxide writes its verdict: one the sender wrote
			// would vouch for the message.
			h.Del(SignatureHeader)
		}
	}
	h.SetContentType("multipart/mixed", typeParams)
	if _, err := h.Date(); err != nil || !h.Has("Date") {
		h.SetDate(msg.Time.Time())
	}
	h.SetSubject(msg.Subject)
	if msg.Sender != nil {
		h.SetAddressList("From", []*mail.Address{mailAddress(msg.Sender)})
	}
	setAddresses(&h, "Reply-To", msg.ReplyTos)
	setAddresses(&h, "To", msg.ToList)
	setAddresses(&h, "Cc", msg.CCList)
	setAddresses(&h, "Bcc", msg.BCCList)
	if !h.Has("Message-Id") {
		h.Set("Message-Id", fmt.Sprintf("<%s>", messageID(msg)))
	}
	return h.Header
}

// setAddresses sets field k to addresses, unless there are none: then a field
// the original header has is kept, as proton-bridge does.
func setAddresses(h *mail.Header, k string, addresses []*protonmail.MessageAddress) {
	if len(addresses) > 0 {
		h.SetAddressList(k, mailAddressList(addresses))
	}
}

// originalHeader parses the header Proton keeps for a message. Proton sends
// it without the blank line that ends a header.
func originalHeader(s string) (textproto.Header, error) {
	h, err := textproto.ReadHeader(bufio.NewReader(strings.NewReader(s + "\r\n")))
	if err != nil {
		return textproto.Header{}, err
	}
	return h, nil
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
