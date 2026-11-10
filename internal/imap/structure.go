package imap

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend/backendutil"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"

	"github.com/seamanticus/peroxide/internal/imap/database"
	"github.com/seamanticus/peroxide/internal/protonmail"
)

// What IMAP derives from a built message: its BODYSTRUCTURE and ENVELOPE,
// computed once and stored, and the sections FETCH serves from its bytes.

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
