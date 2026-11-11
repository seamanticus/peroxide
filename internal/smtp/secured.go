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
	"slices"
	"strings"

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
// headers included. The client's signature is not kept: Proton delivers
// only its own, by the address key (docs/smtp.md), so the part is sent as
// the message, unwrapped (unwrapSigned).
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

// unwrapSigned returns the message raw would be unsigned: its header, but
// for the multipart/signed Content-* fields, with the signed part's own
// header fields (Content-Type and the like), then the signed part's body.
func unwrapSigned(raw []byte, s *signedParts) ([]byte, error) {
	outer, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		return nil, fmt.Errorf("cannot read the message header: %w", err)
	}
	br := bufio.NewReader(bytes.NewReader(s.content))
	inner, err := textproto.ReadHeader(br)
	if err != nil {
		return nil, fmt.Errorf("cannot read the signed part's header: %w", err)
	}
	for _, k := range []string{"Content-Type", "Content-Transfer-Encoding", "Content-Disposition", "Content-Description", "Content-Id"} {
		outer.Del(k)
	}
	// Add puts a field first in the written header: added in reverse, the
	// part's fields keep their order.
	var fields [][2]string
	for f := inner.Fields(); f.Next(); {
		fields = append(fields, [2]string{f.Key(), f.Value()})
	}
	for _, kv := range slices.Backward(fields) {
		outer.Add(kv[0], kv[1])
	}
	if !outer.Has("Content-Type") {
		// RFC 2045 section 5.2: a part without one is plain US-ASCII text.
		outer.Set("Content-Type", "text/plain; charset=us-ascii")
	}
	var b bytes.Buffer
	if err := textproto.WriteHeader(&b, outer); err != nil {
		return nil, err
	}
	if _, err := io.Copy(&b, br); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
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

// recipientKey is a recipient of a client-encrypted message and their
// Proton key, nil for an external recipient.
type recipientKey struct {
	addr string
	key  *openpgp.Entity
}

// assignment is the one key packet an external recipient gets in their
// PGPMIME package.
type assignment struct {
	addr   string
	packet []byte
}

// lookupRecipients fetches each recipient's Proton key, if they have one.
func lookupRecipients(c *protonmail.Client, recipients []*mail.Address) ([]recipientKey, error) {
	var out []recipientKey
	for _, rcpt := range recipients {
		resp, err := c.GetPublicKeys(context.Background(), rcpt.Address)
		if err != nil {
			return nil, fmt.Errorf("cannot get public key for address %q: %v", rcpt.Address, err)
		}
		key, err := resp.SendingKey()
		if err != nil {
			return nil, fmt.Errorf("cannot send to %q: %w", rcpt.Address, err)
		}
		rk := recipientKey{addr: rcpt.Address}
		if key != nil {
			if rk.key, err = key.Entity(); err != nil {
				return nil, fmt.Errorf("cannot read the public key of %q: %w", rcpt.Address, err)
			}
		}
		out = append(out, rk)
	}
	return out, nil
}

// assignPackets gives the recipient the one key packet Proton takes for
// them (2001 Multiple packets present otherwise). Proton users are refused:
// Proton delivers to them only text/plain or text/html internal packages
// (2001 Invalid package type for multipart/mixed, Wrong type of package
// for PGPMIME). Packets to the account's own keys (own) are left out: the
// Sent copy keeps them. An external recipient's key is unknown to Proton
// and peroxide, so one external recipient gets the one packet left; any
// other case can't be told apart and is refused.
func assignPackets(ps []keyPacket, rcpts []recipientKey, own openpgp.EntityList) ([]assignment, error) {
	var external, internal []string
	for _, r := range rcpts {
		if r.key != nil {
			internal = append(internal, r.addr)
		} else {
			external = append(external, r.addr)
		}
	}
	if len(internal) > 0 {
		return nil, fmt.Errorf("cannot send a client-encrypted message to Proton users (%s): Proton can't deliver PGP/MIME to them; send it without client encryption, and peroxide encrypts it to their Proton key end to end", strings.Join(internal, ", "))
	}
	if len(external) == 0 {
		return nil, errors.New("cannot send a client-encrypted message without recipients")
	}
	if len(external) > 1 {
		return nil, fmt.Errorf("cannot send an encrypted message to several recipients without a Proton key (%s) at once: Proton takes one key packet per recipient, and which is whose can't be told; send to each apart", strings.Join(external, ", "))
	}
	used := make([]bool, len(ps))
	for i, p := range ps {
		for _, e := range own {
			if hasKeyID(e, p.id) {
				used[i] = true
			}
		}
	}
	var left []int
	var ids []string
	for i, p := range ps {
		if !used[i] {
			left = append(left, i)
			ids = append(ids, fmt.Sprintf("%X", p.id))
		}
	}
	switch len(left) {
	case 1:
		return []assignment{{addr: external[0], packet: ps[left[0]].raw}}, nil
	case 0:
		return nil, fmt.Errorf("cannot send to %q: the message is encrypted only to your own keys, not to theirs", external[0])
	default:
		return nil, fmt.Errorf("cannot send to %q: the message is encrypted to %d keys Proton doesn't know (%s) and Proton takes only the recipient's; don't encrypt to yourself, or do with your Proton key (docs/smtp.md)", external[0], len(left), strings.Join(ids, ", "))
	}
}

// planSecured checks, before any draft exists, that the client-encrypted
// message sec can go to recipients, and picks each one's key packet.
func planSecured(c *protonmail.Client, own openpgp.EntityList, sec *secured, recipients []*mail.Address) ([]assignment, error) {
	ps, err := splitKeyPackets(sec.encrypted.keyPackets)
	if err != nil {
		return nil, err
	}
	rcpts, err := lookupRecipients(c, recipients)
	if err != nil {
		return nil, err
	}
	return assignPackets(ps, rcpts, own)
}

// sendSecured sends draft msg, already created, encrypted by the client as
// PGP/MIME, with the client's data packet as the body and the recipient's
// key packet (planSecured) in a PGPMIME package, which Proton reassembles
// into the multipart/encrypted the client built. The draft, and so the Sent
// copy, holds the client's message as text: Proton takes only text/plain
// and text/html draft bodies (2011 for multipart/mixed).
func sendSecured(c *protonmail.Client, msg *protonmail.Message, privateKey *openpgp.Entity, sec *secured, raw []byte, plan []assignment) error {
	if sec.encrypted == nil {
		panic("invariant: sendSecured of a message the client didn't encrypt")
	}
	entity, err := mimeBody(raw)
	if err != nil {
		return err
	}
	log.Println("uploading message body (client-encrypted PGP/MIME, kept whole)")
	draft, err := uploadDraftBody(c, msg, privateKey, "text/plain", entity)
	if err != nil {
		return fmt.Errorf("cannot update draft message: %v", err)
	}

	set := protonmail.NewMessagePackageSet(nil)
	set.MIMEType = "multipart/mixed"
	set.Body = base64.StdEncoding.EncodeToString(sec.encrypted.data)
	for _, a := range plan {
		set.AddPGPMIME(a.addr, a.packet)
	}

	log.Println("sending message (client-encrypted)")
	outgoing := &protonmail.OutgoingMessage{ID: draft.ID, Packages: []*protonmail.MessagePackageSet{set}}
	if _, _, err := c.SendMessage(outgoing); err != nil {
		return fmt.Errorf("cannot send message: %v", err)
	}
	return nil
}

// uploadDraftBody sets draft msg's body to body, of type mimeType,
// encrypted and signed with the address key.
func uploadDraftBody(c *protonmail.Client, msg *protonmail.Message, privateKey *openpgp.Entity, mimeType string, body []byte) (*protonmail.Message, error) {
	msg.MIMEType = mimeType
	plaintext, err := msg.Encrypt([]*openpgp.Entity{privateKey}, privateKey)
	if err != nil {
		return nil, err
	}
	if _, err := plaintext.Write(body); err != nil {
		plaintext.Close()
		return nil, err
	}
	if err := plaintext.Close(); err != nil {
		return nil, err
	}
	return c.UpdateDraftMessage(msg)
}

// keyPacket is one public-key encrypted session key packet of the client's
// ciphertext: the key ID it names (0 for a hidden recipient, RFC 9580
// 5.1) and its bytes as the client wrote them.
type keyPacket struct {
	id  uint64
	raw []byte
}

// splitKeyPackets splits b, the client's key packets, into its public-key
// encrypted session key packets, cut by reader position like
// splitEncrypted. Symmetric-key ones (passphrase) name no key and are left
// out.
func splitKeyPackets(b []byte) ([]keyPacket, error) {
	var out []keyPacket
	src := bytes.NewReader(b)
	ops := packet.NewOpaqueReader(src)
	for {
		start := len(b) - src.Len()
		op, err := ops.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("key packets: %w", err)
		}
		raw := b[start : len(b)-src.Len()]
		p, err := op.Parse()
		if err != nil {
			return nil, fmt.Errorf("key packet: %w", err)
		}
		if ek, ok := p.(*packet.EncryptedKey); ok {
			out = append(out, keyPacket{id: ek.KeyId, raw: raw})
		}
	}
}

// hasKeyID reports whether id is the ID of e's primary key or a subkey.
func hasKeyID(e *openpgp.Entity, id uint64) bool {
	if id == e.PrimaryKey.KeyId {
		return true
	}
	for _, sub := range e.Subkeys {
		if id == sub.PublicKey.KeyId {
			return true
		}
	}
	return false
}
