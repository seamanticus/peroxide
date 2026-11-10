package imap

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"slices"

	"github.com/ProtonMail/go-crypto/openpgp"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/emersion/go-message"

	"github.com/seamanticus/peroxide/internal/protonmail"
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
//
// Any other part that can't be decrypted is served as bridge does
// (pkg/message/build_custom.go), so that nothing is lost and the client may
// decrypt it: a body as an armored text part, an attachment as an armored
// <name>.pgp file, each with an armor comment saying why.

// passThrough reports whether msg goes to the client as PGP/MIME: its body is
// a PGP message encrypted to no key of the account, unlocked or not, and it
// has no attachments, which multipart/encrypted has no room for (bridge too
// keeps PGP/MIME for messages without them, BuildRFC822Into). Read gives
// ErrKeyIncorrect only for an armored PGP message, and before it reads any of
// the encrypted data (go-crypto openpgp.ReadMessage). A body encrypted to an
// account key that isn't unlocked isn't passed through: the user's own
// client couldn't open it either.
func (u *user) passThrough(msg *protonmail.Message) bool {
	if len(msg.Attachments) > 0 {
		return false
	}
	_, err := msg.Read(u.privateKeys, nil)
	if !errors.Is(err, pgperrors.ErrKeyIncorrect) {
		return false
	}
	toAccount, err := u.encryptedToAccount(msg)
	if err != nil {
		log.Printf("cannot read the keys message %v is encrypted to: %v", msg.ID, err)
		return false
	}
	return !toAccount
}

// encryptedToAccount reports whether the body of msg is encrypted to a key of
// the account, unlocked or not. A hidden recipient (key ID 0) names no key.
func (u *user) encryptedToAccount(msg *protonmail.Message) (bool, error) {
	keyIDs, err := msg.EncryptedTo()
	if err != nil {
		return false, err
	}
	account := u.accountKeys()
	for _, id := range keyIDs {
		if id != 0 && len(account.KeysById(id)) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// accountKeys returns every active key of the account and its addresses,
// unlocked or not, as the API lists them: to tell mail encrypted to a key the
// account has from mail encrypted to a key it doesn't.
func (u *user) accountKeys() openpgp.EntityList {
	keys := slices.Clone(u.u.Keys)
	for _, addr := range u.addresses() {
		keys = append(keys, addr.Keys...)
	}
	var entities openpgp.EntityList
	for _, key := range keys {
		if key.Active != 1 {
			continue
		}
		e, err := key.Entity()
		if err != nil {
			log.Printf("cannot read key %v: %v", key.ID, err)
			continue
		}
		entities = append(entities, e)
	}
	return entities
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
	if err := writePart(w, versionHeader, []byte("Version: 1\r\n")); err != nil {
		return nil, fmt.Errorf("cannot write PGP/MIME version part: %w", err)
	}

	var dataHeader message.Header
	dataHeader.SetContentType("application/octet-stream", map[string]string{"name": "encrypted.asc"})
	dataHeader.SetContentDisposition("inline", map[string]string{"filename": "encrypted.asc"})
	dataHeader.Set("Content-Description", "OpenPGP encrypted message")
	if err := writePart(w, dataHeader, []byte(msg.Body)); err != nil {
		return nil, fmt.Errorf("cannot write PGP/MIME data part: %w", err)
	}

	if err := w.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// writePart writes a part of header h and body to w, encoded as h says.
func writePart(w *message.Writer, h message.Header, body []byte) error {
	pw, err := w.CreatePart(h)
	if err != nil {
		return err
	}
	if _, err := pw.Write(body); err != nil {
		return err
	}
	return pw.Close()
}
