package imap

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"

	"github.com/seamanticus/peroxide/imap/database"
	"github.com/seamanticus/peroxide/protonmail"
)

// SEARCH reads what is stored: a message already built and stored is
// searched in what is served (its header, its decoded text, its size), one
// not stored yet in its metadata, as before. Nothing is downloaded for a
// SEARCH, so its results may change as the download stores more messages.

// searchNeeds is what of the stored message a search criteria reads.
type searchNeeds struct {
	record  bool // the size
	content bool // the header and the text
}

func needsOf(c *imap.SearchCriteria) searchNeeds {
	n := searchNeeds{
		record:  c.Larger > 0 || c.Smaller > 0,
		content: len(c.Header) > 0 || len(c.Body) > 0 || len(c.Text) > 0 || !c.SentBefore.IsZero() || !c.SentSince.IsZero(),
	}
	add := func(o searchNeeds) {
		n.record = n.record || o.record
		n.content = n.content || o.content
	}
	for _, or := range c.Or {
		add(needsOf(or[0]))
		add(needsOf(or[1]))
	}
	for _, not := range c.Not {
		add(needsOf(not))
	}
	return n
}

// searched is a message as SEARCH reads it.
type searched struct {
	msg    *protonmail.Message
	size   uint32
	header textproto.Header
	text   string // the decoded text parts; with a stored message only
	stored bool   // header, text and size are the stored message's
}

// loadSearched reads what needs asks for of the message msg.
func (u *user) loadSearched(msg *protonmail.Message, needs searchNeeds) (*searched, error) {
	s := &searched{msg: msg, size: uint32(msg.Size)}
	switch {
	case needs.content:
		rec, literal, err := u.db.Body(msg.ID)
		if errors.Is(err, database.ErrNotFound) {
			break
		} else if err != nil {
			return nil, err
		}
		h, text, err := searchText(literal)
		if err != nil {
			return nil, fmt.Errorf("cannot read stored message %v: %w", msg.ID, err)
		}
		s.size, s.header, s.text, s.stored = rec.Size, h, text, true
	case needs.record:
		rec, err := u.db.BodyRecord(msg.ID)
		if errors.Is(err, database.ErrNotFound) {
			break
		} else if err != nil {
			return nil, err
		}
		s.size = rec.Size
	}
	if !s.stored {
		s.header = messageHeader(msg).Header
	}
	return s, nil
}

// searchText returns the header of a stored message and its text parts,
// decoded and joined: what SEARCH BODY looks in (RFC 3501 section 6.4.4).
// Attachments, armored parts included, aren't text a user wrote: a part with
// a filename is left out. A charset go-message doesn't know is read as is.
func searchText(literal []byte) (textproto.Header, string, error) {
	h, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(literal)))
	if err != nil {
		return textproto.Header{}, "", err
	}
	e, err := message.Read(bytes.NewReader(literal))
	if err != nil && !message.IsUnknownCharset(err) {
		return h, "", err
	}
	var text strings.Builder
	err = e.Walk(func(_ []int, part *message.Entity, err error) error {
		if err != nil && !message.IsUnknownCharset(err) {
			return err
		}
		mt, _, _ := part.Header.ContentType()
		if !strings.HasPrefix(mt, "text/") {
			return nil
		}
		if _, params, _ := part.Header.ContentDisposition(); params["filename"] != "" {
			return nil
		}
		if _, err := io.Copy(&text, part.Body); err != nil {
			return err
		}
		text.WriteString("\n")
		return nil
	})
	if err != nil {
		return h, "", err
	}
	return h, text.String(), nil
}

// sentDate is the date SENTBEFORE and SENTSINCE compare: the Date header of
// a stored message, else Proton's time.
func (s *searched) sentDate() time.Time {
	if s.stored {
		mh := mail.Header{Header: message.Header{Header: s.header}}
		if d, err := mh.Date(); err == nil {
			return d
		}
	}
	return s.msg.Time.Time()
}

func matchString(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

func (mbox *mailbox) matchMessage(seqNum, uid uint32, s *searched, c *imap.SearchCriteria) bool {
	msg := s.msg
	if c.SeqNum != nil && !c.SeqNum.Contains(seqNum) {
		return false
	}
	if c.Uid != nil && !c.Uid.Contains(uid) {
		return false
	}

	flags := make(map[string]bool)
	for _, flag := range mbox.fetchFlags(msg) {
		flags[flag] = true
	}
	for _, f := range c.WithFlags {
		if !flags[f] {
			return false
		}
	}
	for _, f := range c.WithoutFlags {
		if flags[f] {
			return false
		}
	}

	date := msg.Time.Time().Round(24 * time.Hour)
	if !c.Since.IsZero() && !date.After(c.Since) {
		return false
	}
	if !c.Before.IsZero() && !date.Before(c.Before) {
		return false
	}

	sent := s.sentDate().Round(24 * time.Hour)
	if !c.SentBefore.IsZero() && !sent.Before(c.SentBefore) {
		return false
	}
	if !c.SentSince.IsZero() && !sent.After(c.SentSince) {
		return false
	}

	if c.Larger > 0 && s.size < c.Larger {
		return false
	}
	if c.Smaller > 0 && s.size > c.Smaller {
		return false
	}

	for key, wantValues := range c.Header {
		fields := s.header.FieldsByKey(key)
		var values []string
		for fields.Next() {
			values = append(values, fields.Value())
		}

		for _, wantValue := range wantValues {
			if wantValue == "" && len(values) == 0 {
				return false
			}
			if wantValue != "" {
				ok := false
				for _, v := range values {
					if matchString(v, wantValue) {
						ok = true
						break
					}
				}
				if !ok {
					return false
				}
			}
		}
	}

	for _, e := range c.Body {
		if !matchString(s.text, e) {
			return false
		}
	}

	for _, e := range c.Text {
		if !matchString(s.text, e) && !matchHeader(s.header, e) {
			return false
		}
	}

	for _, e := range c.Or {
		if !mbox.matchMessage(seqNum, uid, s, e[0]) && !mbox.matchMessage(seqNum, uid, s, e[1]) {
			return false
		}
	}

	for _, e := range c.Not {
		if mbox.matchMessage(seqNum, uid, s, e) {
			return false
		}
	}

	return true
}

// matchHeader reports whether a field of h holds substr.
func matchHeader(h textproto.Header, substr string) bool {
	for f := h.Fields(); f.Next(); {
		if matchString(f.Value(), substr) {
			return true
		}
	}
	return false
}

func (mbox *mailbox) SearchMessages(isUID bool, c *imap.SearchCriteria) ([]uint32, error) {
	// The entries are collected in one read transaction, and the messages
	// read and matched after it ends. matchMessage takes locks (fetchFlags),
	// and a bolt write that grows the file waits for every open read
	// transaction: matching inside one deadlocked against a writer holding,
	// or waiting for, those locks. A nested read transaction did as well.
	type entry struct {
		seqNum, uid uint32
		apiID       string
	}
	var entries []entry
	err := mbox.db.ForEach(func(seqNum, uid uint32, apiID string) error {
		entries = append(entries, entry{seqNum, uid, apiID})
		return nil
	})
	if err != nil {
		return nil, err
	}

	needs := needsOf(c)
	var results []uint32
	for _, e := range entries {
		msg, err := mbox.u.db.Message(e.apiID)
		if errors.Is(err, database.ErrNotFound) {
			continue // deleted by an event since the entries were read
		} else if err != nil {
			return nil, err
		}
		s, err := mbox.u.loadSearched(msg, needs)
		if err != nil {
			return nil, err
		}

		if mbox.matchMessage(e.seqNum, e.uid, s, c) {
			if isUID {
				results = append(results, e.uid)
			} else {
				results = append(results, e.seqNum)
			}
		}
	}
	return results, nil
}
