package sendrecord

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"slices"
	"strings"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
)

// Hash returns what identifies a message across the forms a client gives it:
// as sent over SMTP and as saved with APPEND. It is gluon's GetMessageHash
// (rfc822/hash.go), which proton-bridge's sendrecorder uses: SHA-256 over
//   - the Subject, and the addresses of From, To, Cc, Reply-To and
//     In-Reply-To;
//   - for each part that isn't multipart: its media type and parameters but
//     the boundary, its Content-Disposition, and its body decoded from its
//     transfer encoding, without CRs and outer white space.
//
// So the boundaries, the transfer encodings and the other header fields (Bcc,
// Date, Message-Id) may differ. Unlike gluon it decodes every part's body,
// not only text/plain and text/html.
func Hash(literal []byte) (string, error) {
	e, err := message.Read(bytes.NewReader(literal))
	if err != nil && !lenient(err) {
		return "", fmt.Errorf("cannot read the message: %w", err)
	}
	h := sha256.New()
	io.WriteString(h, e.Header.Get("Subject"))
	for _, k := range []string{"From", "To", "Cc", "Reply-To", "In-Reply-To"} {
		io.WriteString(h, addresses(e.Header.Get(k)))
	}
	if err := hashParts(h, e); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}

func hashParts(w io.Writer, e *message.Entity) error {
	if mr := e.MultipartReader(); mr != nil {
		for {
			p, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil && !lenient(err) {
				return fmt.Errorf("cannot read a part of the message: %w", err)
			}
			if err := hashParts(w, p); err != nil {
				return err
			}
		}
	}

	// A Content-Type that can't be parsed is left out, as gluon does.
	mediaType, params, err := mime.ParseMediaType(asciiOnly(cmp.Or(e.Header.Get("Content-Type"), "text/plain")))
	if err == nil {
		io.WriteString(w, mediaType)
		for _, k := range slices.Sorted(maps.Keys(params)) {
			if strings.EqualFold(k, "boundary") {
				continue
			}
			io.WriteString(w, k)
			io.WriteString(w, params[k])
		}
	}
	io.WriteString(w, e.Header.Get("Content-Disposition"))

	body, err := io.ReadAll(e.Body)
	if err != nil {
		return fmt.Errorf("cannot read a part of the message: %w", err)
	}
	_, err = w.Write(bytes.TrimSpace(bytes.ReplaceAll(body, []byte{'\r'}, nil)))
	return err
}

// lenient reports whether go-message's err leaves a part readable as is: an
// unknown charset (no message.CharsetReader is set: bodies keep theirs) or
// transfer encoding.
func lenient(err error) bool {
	return message.IsUnknownCharset(err) || message.IsUnknownEncoding(err)
}

// addresses returns the addresses of a header field, joined; the field as is
// if it isn't an address list.
func addresses(field string) string {
	list, err := mail.ParseAddressList(field)
	if err != nil {
		return field
	}
	var b strings.Builder
	for _, a := range list {
		b.WriteString(a.Address)
	}
	return b.String()
}

func asciiOnly(s string) string {
	return strings.Map(func(r rune) rune {
		if r > 0x7f {
			return -1
		}
		return r
	}, s)
}
