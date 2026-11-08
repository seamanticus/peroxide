package imap

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"strings"

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
