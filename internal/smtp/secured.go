package smtp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"

	"github.com/seamanticus/peroxide/internal/protonmail"
)

// secured is a PGP/MIME message the client signed or encrypted itself
// (RFC 3156), split into what Proton's send packages carry. Exactly one of
// signed and encrypted is set.
type secured struct {
	signed    *signedParts
	encrypted *encryptedParts
}

// signedParts is a multipart/signed message: the first part's exact bytes,
// headers included. The client's signature is not kept: Proton signs again
// with the address key and delivers only its own (see sendSecured).
type signedParts struct {
	content []byte
}

// encryptedParts is a multipart/encrypted message's ciphertext split in its
// key packets (one per key the client encrypted to) and its data packet.
type encryptedParts struct {
	keyPackets []byte
	data       []byte
}

// parseSecured reports whether raw is a PGP/MIME message the client
// secured, and splits it. A message that says it is one but can't be split
// is an error: taking it apart as plain mail would break it.
func parseSecured(raw []byte) (*secured, bool, error) {
	src := bytes.NewReader(raw)
	br := bufio.NewReader(src)
	h, err := textproto.ReadHeader(br)
	if err != nil {
		return nil, false, nil
	}
	t, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil {
		return nil, false, nil
	}
	// What neither src nor br's buffer still holds is the header.
	body := raw[len(raw)-src.Len()-br.Buffered():]
	switch {
	case t == "multipart/signed" && params["protocol"] == "application/pgp-signature":
		s, err := splitSigned(body, params["boundary"])
		if err != nil {
			return nil, false, fmt.Errorf("cannot read the signed message: %w", err)
		}
		return &secured{signed: s}, true, nil
	case t == "multipart/encrypted" && params["protocol"] == "application/pgp-encrypted":
		e, err := splitEncrypted(body, params["boundary"])
		if err != nil {
			return nil, false, fmt.Errorf("cannot read the encrypted message: %w", err)
		}
		return &secured{encrypted: e}, true, nil
	}
	return nil, false, nil
}

// splitSigned cuts the signed part out of body by its delimiters, bytes
// untouched, as RFC 3156 section 5 has the signature cover them: from after
// the first delimiter line's CRLF to before the CRLF of the next one.
func splitSigned(body []byte, boundary string) (*signedParts, error) {
	if boundary == "" {
		return nil, errors.New("no boundary")
	}
	delim := []byte("\r\n--" + boundary)
	padded := append([]byte("\r\n"), body...)
	_, rest, ok := bytes.Cut(padded, delim)
	if !ok {
		return nil, errors.New("no first part")
	}
	_, rest, ok = bytes.Cut(rest, []byte("\r\n"))
	if !ok {
		return nil, errors.New("no first part")
	}
	content, _, ok := bytes.Cut(rest, delim)
	if !ok {
		return nil, errors.New("no second part")
	}
	return &signedParts{content: content}, nil
}

// splitEncrypted reads the ciphertext, the second part, and splits its
// packets: key packets (public-key and symmetric-key encrypted session
// keys) apart from the one data packet.
func splitEncrypted(body []byte, boundary string) (*encryptedParts, error) {
	if boundary == "" {
		return nil, errors.New("no boundary")
	}
	parts := multipart.NewReader(bytes.NewReader(body), boundary)
	if _, err := parts.NextPart(); err != nil {
		return nil, fmt.Errorf("version part: %w", err)
	}
	p, err := parts.NextPart()
	if err != nil {
		return nil, fmt.Errorf("ciphertext part: %w", err)
	}
	ciphertext, err := dearmor(p, "PGP MESSAGE")
	if err != nil {
		return nil, err
	}
	// Packets are cut from ciphertext by reader position, never
	// re-serialized: the client's bytes, length encodings included, go out
	// as they came.
	src := bytes.NewReader(ciphertext)
	ops := packet.NewOpaqueReader(src)
	dataStart := -1
	for {
		start := len(ciphertext) - src.Len()
		op, err := ops.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("ciphertext packets: %w", err)
		}
		// Tags 1 and 3: public-key and symmetric-key encrypted session key.
		isKey := op.Tag == 1 || op.Tag == 3
		switch {
		case dataStart >= 0:
			return nil, fmt.Errorf("packet (tag %d) after the data packet", op.Tag)
		case !isKey:
			dataStart = start
		}
	}
	if dataStart <= 0 {
		return nil, errors.New("ciphertext without key or data packets")
	}
	return &encryptedParts{keyPackets: ciphertext[:dataStart], data: ciphertext[dataStart:]}, nil
}

func dearmor(r io.Reader, blockType string) ([]byte, error) {
	block, err := armor.Decode(r)
	if err != nil {
		return nil, fmt.Errorf("armor: %w", err)
	}
	if block.Type != blockType {
		return nil, fmt.Errorf("armor block %q, want %q", block.Type, blockType)
	}
	b, err := io.ReadAll(block.Body)
	if err != nil {
		return nil, fmt.Errorf("armor: %w", err)
	}
	return b, nil
}

// sendSecured sends draft msg, already created, to recipients without a
// Proton key, with the client's PGP/MIME kept whole where Proton allows it.
// An encrypted message goes as a PGPMIME package of the client's own
// packets, delivered as the client built it. A signed message goes as a
// ClearMIME package of its signed part, which Proton accepts only signed
// by the address key (2001 otherwise) and delivers re-signed by that key
// alone: the client's signature can't reach the recipient, so it isn't
// sent. The draft, and so the Sent copy, holds the message as the client
// sent it, as text.
func sendSecured(c *protonmail.Client, msg *protonmail.Message, privateKey *openpgp.Entity, sec *secured, raw []byte, recipients []*mail.Address) error {
	log.Println("uploading message body (client-secured PGP/MIME, kept whole)")
	entity, err := mimeBody(raw)
	if err != nil {
		return err
	}
	msg.MIMEType = "text/plain"
	plaintext, err := msg.Encrypt([]*openpgp.Entity{privateKey}, privateKey)
	if err != nil {
		return err
	}
	if _, err := plaintext.Write(entity); err != nil {
		plaintext.Close()
		return err
	}
	if err := plaintext.Close(); err != nil {
		return err
	}
	msg, err = c.UpdateDraftMessage(msg)
	if err != nil {
		return fmt.Errorf("cannot update draft message: %v", err)
	}

	var rcpts []string
	for _, rcpt := range recipients {
		resp, err := c.GetPublicKeys(context.Background(), rcpt.Address)
		if err != nil {
			return fmt.Errorf("cannot get public key for address %q: %v", rcpt.Address, err)
		}
		key, err := resp.SendingKey()
		if err != nil {
			return fmt.Errorf("cannot send to %q: %w", rcpt.Address, err)
		}
		if key != nil {
			return fmt.Errorf("cannot send a client-secured message to %q: recipients with a Proton key aren't supported yet", rcpt.Address)
		}
		rcpts = append(rcpts, rcpt.Address)
	}

	set := protonmail.NewMessagePackageSet(nil)
	switch {
	case sec.signed != nil:
		log.Println("sending message (client-signed, ClearMIME, re-signed with the address key)")
		if err := writeBody(set, "multipart/mixed", privateKey, sec.signed.content); err != nil {
			return err
		}
		for _, r := range rcpts {
			if _, err := set.AddClearMIME(r); err != nil {
				return err
			}
		}
	case sec.encrypted != nil:
		log.Println("sending message (client-encrypted, PGPMIME)")
		set.MIMEType = "multipart/mixed"
		set.Body = base64.StdEncoding.EncodeToString(sec.encrypted.data)
		for _, r := range rcpts {
			set.AddPGPMIME(r, sec.encrypted.keyPackets)
		}
	default:
		panic("invariant: secured message neither signed nor encrypted")
	}

	outgoing := &protonmail.OutgoingMessage{ID: msg.ID, Packages: []*protonmail.MessagePackageSet{set}}
	if _, _, err := c.SendMessage(outgoing); err != nil {
		return fmt.Errorf("cannot send message: %v", err)
	}
	return nil
}
