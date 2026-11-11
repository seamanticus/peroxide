package msgbuild

import (
	"bytes"
	"io"
	"log"
	"mime"

	"github.com/seamanticus/peroxide/internal/protonmail"
)

// Proton takes only text/plain and text/html draft bodies (2011), so the
// Sent copy of a message the client encrypted itself (multipart/encrypted,
// sent whole by internal/smtp) is a text/plain draft: this marker line, then
// the client's MIME entity. Proton's apps show it as that text; peroxide
// serves the entity as the message (clientPGPMIME), so the client sees the
// PGP/MIME it sent.
const clientPGPMIMEMarker = "X-Peroxide-Body: the PGP/MIME message below was sent as is by your mail client; peroxide serves it over IMAP as sent"

// ClientPGPMIMEBody returns the draft body that stands for entity, a
// client's multipart/encrypted message: the marker line, then entity.
func ClientPGPMIMEBody(entity []byte) []byte {
	b := make([]byte, 0, len(clientPGPMIMEMarker)+2+len(entity))
	b = append(b, clientPGPMIMEMarker...)
	b = append(b, "\r\n"...)
	return append(b, entity...)
}

// clientPGPMIME returns full as the client's PGP/MIME message, if its body
// is one ClientPGPMIMEBody wrote: a text/plain draft or sent message whose
// body starts with the marker, signed by one of the account's own keys,
// that holds a multipart/encrypted entity. Only the account signs with its
// keys, so a marker in mail from anyone else, or in an unsigned body, is
// just text. Any check failing leaves full to be built as usual.
func (mb *Builder) clientPGPMIME(full *protonmail.Message) ([]byte, bool) {
	if full.Type != protonmail.MessageDraft && full.Type != protonmail.MessageSent {
		return nil, false
	}
	if full.MIMEType != "text/plain" {
		return nil, false
	}
	md, err := full.Read(mb.Keys, nil)
	if err != nil || !md.IsSigned {
		return nil, false
	}
	body, err := io.ReadAll(md.UnverifiedBody)
	// The signature is checked once the body is read to its end.
	if err != nil || md.SignatureError != nil || !signedByAccount(md, mb.Keys) {
		return nil, false
	}
	entity, ok := cutMarker(body)
	if !ok {
		return nil, false
	}
	bodyHeader, bodyData := SplitEntity(entity)
	mt, params, err := mime.ParseMediaType(bodyHeader.Get("Content-Type"))
	if err != nil || mt != "multipart/encrypted" || params["protocol"] != "application/pgp-encrypted" {
		log.Printf("Message %v starts with peroxide's marker but holds no PGP/MIME: built as text", full.ID)
		return nil, false
	}
	h := MessageHeader(full)
	h.Set("MIME-Version", "1.0")
	literal, err := joinEntity(h, bodyHeader, bodyData)
	if err != nil {
		log.Printf("Message %v: cannot write the client's PGP/MIME (%v): built as text", full.ID, err)
		return nil, false
	}
	return literal, true
}

// cutMarker returns what follows the marker line at the start of body. The
// line may end in LF alone: Proton's apps may store text with bare LF.
func cutMarker(body []byte) ([]byte, bool) {
	rest, ok := bytes.CutPrefix(body, []byte(clientPGPMIMEMarker))
	if !ok {
		return nil, false
	}
	if rest, ok := bytes.CutPrefix(rest, []byte("\r\n")); ok {
		return rest, true
	}
	return bytes.CutPrefix(rest, []byte("\n"))
}
