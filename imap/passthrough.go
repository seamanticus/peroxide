package imap

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"

	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend/backendutil"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/textproto"

	"github.com/seamanticus/peroxide/protonmail"
)

// A message the sender encrypted end to end to a key Proton doesn't hold,
// such as the recipient's own key in GnuPG, can't be decrypted here: Proton
// stored it as received, and only the client can read it. peroxide hands it
// over unchanged as PGP/MIME (RFC 3156), as proton-bridge does for bodies it
// can't decrypt (pkg/message/build.go, buildPGPMIMEFallbackRFC822):
//
//	multipart/encrypted; protocol="application/pgp-encrypted"
//	  1. application/pgp-encrypted: "Version: 1"
//	  2. application/octet-stream: the armored PGP message, byte for byte
//
// Clients (aerc's gpg and internal providers, Thunderbird, Evolution)
// decrypt such a message with their own keys.

// passThrough reports whether the body of msg is a PGP message that none of
// the account's keys opens, and must go to the client as it is. Read gives
// ErrKeyIncorrect only for an armored PGP message, and before it reads any of
// the encrypted data (go-crypto openpgp.ReadMessage), so the body itself is
// left unread. Any other failure to decrypt is an error of its own, reported
// when the body is read.
func (mbox *mailbox) passThrough(msg *protonmail.Message) bool {
	_, err := msg.Read(mbox.u.privateKeys, nil)
	return errors.Is(err, pgperrors.ErrKeyIncorrect)
}

// passThroughBoundary is the boundary of the multipart/encrypted message:
// derived from the message ID, so every FETCH of it gives the same bytes.
func passThroughBoundary(msg *protonmail.Message) string {
	return "pgp-" + msgBoundary(msg)
}

// passThroughMessage writes msg as PGP/MIME: its header, then the two parts.
func passThroughMessage(msg *protonmail.Message) ([]byte, error) {
	h := messageHeader(msg)
	h.SetContentType("multipart/encrypted", map[string]string{
		"boundary": passThroughBoundary(msg),
		"protocol": "application/pgp-encrypted",
	})

	var b bytes.Buffer
	w, err := message.CreateWriter(&b, h)
	if err != nil {
		return nil, err
	}

	var versionHeader message.Header
	versionHeader.SetContentType("application/pgp-encrypted", nil)
	versionHeader.Set("Content-Description", "PGP/MIME version identification")
	if err := writePart(w, versionHeader, "Version: 1\r\n"); err != nil {
		return nil, fmt.Errorf("cannot write PGP/MIME version part: %w", err)
	}

	var dataHeader message.Header
	dataHeader.SetContentType("application/octet-stream", map[string]string{"name": "encrypted.asc"})
	dataHeader.SetContentDisposition("inline", map[string]string{"filename": "encrypted.asc"})
	dataHeader.Set("Content-Description", "OpenPGP encrypted message")
	if err := writePart(w, dataHeader, msg.Body); err != nil {
		return nil, fmt.Errorf("cannot write PGP/MIME data part: %w", err)
	}

	if err := w.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writePart(w *message.Writer, h message.Header, body string) error {
	pw, err := w.CreatePart(h)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(pw, body); err != nil {
		return err
	}
	return pw.Close()
}

// readPassThrough returns the header and body of passThroughMessage(msg),
// for go-imap's helpers, which compute sections and structures from them.
func readPassThrough(msg *protonmail.Message) (textproto.Header, *bytes.Reader, error) {
	raw, err := passThroughMessage(msg)
	if err != nil {
		return textproto.Header{}, nil, err
	}
	br := bufio.NewReader(bytes.NewReader(raw))
	h, err := textproto.ReadHeader(br)
	if err != nil {
		return textproto.Header{}, nil, fmt.Errorf("cannot read PGP/MIME header: %w", err)
	}
	rest, err := io.ReadAll(br)
	if err != nil {
		return textproto.Header{}, nil, err
	}
	return h, bytes.NewReader(rest), nil
}

func (mbox *mailbox) passThroughBodyStructure(msg *protonmail.Message, extended bool) (*imap.BodyStructure, error) {
	log.Printf("Message %v is encrypted to a key Proton doesn't have: passed through as PGP/MIME", msg.ID)
	h, body, err := readPassThrough(msg)
	if err != nil {
		return nil, err
	}
	return backendutil.FetchBodyStructure(h, body, extended)
}

func (mbox *mailbox) passThroughBodySection(msg *protonmail.Message, section *imap.BodySectionName) (imap.Literal, error) {
	h, body, err := readPassThrough(msg)
	if err != nil {
		return nil, err
	}
	return backendutil.FetchBodySection(h, body, section)
}
