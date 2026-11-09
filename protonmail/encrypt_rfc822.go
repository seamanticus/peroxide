// encryptRFC822 and its helpers are ported from EncryptRFC822 in
// go-proton-api (message_encrypt.go, github.com/ProtonMail/go-proton-api),
// from gluon's rfc822 and gopenpgp onto go-message and go-crypto:
//
// The MIT License (MIT)
//
// Copyright (c) 2020 James Houlahan
// Copyright (c) 2022 Proton AG
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package protonmail

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/textproto"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/ianaindex"
)

// encryptRFC822 encrypts literal to key for the import API. It encrypts the
// message part by part, keeping its MIME structure, so that Proton can read
// its text and attachments. A message it can't take apart, one already
// signed or encrypted, or one with more than one text part is encrypted
// whole instead, as PGP/MIME signed by key.
//
// Parts are encrypted as they are read, into one buffer sized for the
// result up front: base64 attachments stay about their size. Encrypted
// whole, the message is armored, a third larger with its line breaks: that
// attempt gets a buffer of its own sized for it, the first one dropped, so
// that neither grows by doubling.
func encryptRFC822(key *openpgp.Entity, literal []byte) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(len(literal) + len(literal)/8 + 4096)
	if err := encryptParts(&buf, key, literal); err != nil {
		buf = bytes.Buffer{} // dropped before the second is made
		var full bytes.Buffer
		full.Grow(len(literal) + len(literal)*3/8 + 8192)
		err = encryptFull(&full, key, literal)
		return full.Bytes(), err
	}
	return buf.Bytes(), nil
}

func encryptParts(w io.Writer, key *openpgp.Entity, literal []byte) error {
	// go-message decodes the transfer encoding; the charset is decoded here
	// (encryptText), as no message.CharsetReader is set.
	e, err := message.Read(bytes.NewReader(literal))
	if err != nil && !message.IsUnknownCharset(err) {
		return err
	}
	texts := 0
	return encryptEntity(w, key, e, &texts)
}

// encryptEntity writes e encrypted to w. texts counts the text parts met.
func encryptEntity(w io.Writer, key *openpgp.Entity, e *message.Entity, texts *int) error {
	mediaType, params, err := e.Header.ContentType()
	if err != nil {
		// gluon reads a Content-Type it can't parse as none: an attachment.
		mediaType, params = "", map[string]string{}
	}
	if e.Header.Has("Content-Disposition") {
		if _, _, err := e.Header.ContentDisposition(); err != nil {
			return fmt.Errorf("cannot read content disposition: %w", err)
		}
	}
	typ, sub, _ := strings.Cut(mediaType, "/")
	switch sub {
	case "encrypted":
		return errors.New("already encrypted")
	case "signed":
		return errors.New("already signed")
	}

	switch typ {
	case "multipart":
		return encryptMultipart(w, key, e, params["boundary"], texts)
	case "text":
		if *texts++; *texts > 1 {
			return errors.New("multiple text parts")
		}
		return encryptText(w, key, e, mediaType, params)
	case "message":
		return encryptText(w, key, e, mediaType, params)
	default:
		return encryptAttachment(w, key, e)
	}
}

// encryptMultipart writes e's header as is, then each part encrypted,
// between the same boundaries.
func encryptMultipart(w io.Writer, key *openpgp.Entity, e *message.Entity, boundary string, texts *int) error {
	mr := e.MultipartReader()
	if mr == nil || boundary == "" {
		return errors.New("multipart without a boundary")
	}
	if err := textproto.WriteHeader(w, e.Header.Header); err != nil {
		return err
	}
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil && !message.IsUnknownCharset(err) {
			return err
		}
		if _, err := fmt.Fprintf(w, "--%v\r\n", boundary); err != nil {
			return err
		}
		if err := encryptEntity(w, key, p, texts); err != nil {
			return err
		}
		if _, err := io.WriteString(w, "\r\n"); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "--%v--\r\n", boundary)
	return err
}

// encryptText writes a text part as an armored PGP message, decoded from its
// transfer encoding and, if it names one, its charset: it is UTF-8 after.
func encryptText(w io.Writer, key *openpgp.Entity, e *message.Entity, mediaType string, params map[string]string) error {
	var body io.Reader = e.Body
	h := e.Header.Copy()
	h.Del("Content-Transfer-Encoding")
	if charset, ok := params["charset"]; ok {
		enc, err := charsetEncoding(charset)
		if err != nil {
			return err
		}
		body = enc.NewDecoder().Reader(body)
		params["charset"] = "utf-8"
		h.Set("Content-Type", mime.FormatMediaType(mediaType, params))
	}
	if err := textproto.WriteHeader(w, h.Header); err != nil {
		return err
	}
	return encryptArmored(w, key, body, nil)
}

// encryptAttachment writes a part that isn't text as a binary PGP message,
// base64-encoded.
func encryptAttachment(w io.Writer, key *openpgp.Entity, e *message.Entity) error {
	h := e.Header.Copy()
	h.Set("Content-Transfer-Encoding", "base64")
	if err := textproto.WriteHeader(w, h.Header); err != nil {
		return err
	}
	bw := base64.NewEncoder(base64.StdEncoding, w)
	if err := encryptTo(bw, key, e.Body, nil); err != nil {
		return err
	}
	return bw.Close()
}

// encryptFull writes literal whole to w as a PGP/MIME message, signed by
// key, under a header holding the fields Proton lists messages by.
func encryptFull(w io.Writer, key *openpgp.Entity, literal []byte) error {
	orig, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(literal)))
	if err != nil {
		return err
	}

	// From the message, not random as in go-proton-api: the same APPEND sends
	// the same bytes. "--import-" can't start a line of the armored text.
	sum := sha256.Sum256(literal)
	boundary := "import-" + hex.EncodeToString(sum[:16])

	var h message.Header
	if v := orig.Get("Message-Id"); v != "" {
		h.Set("Message-Id", v)
	}
	h.Set("Mime-Version", "1.0")
	h.SetContentType("multipart/encrypted", map[string]string{
		"boundary": boundary,
		"protocol": "application/pgp-encrypted",
	})
	for _, k := range []string{"From", "To", "Subject", "Date", "Received", "Cc"} {
		if v := orig.Get(k); v != "" {
			h.Set(k, v)
		}
	}

	var control, attachment textproto.Header
	control.Set("Content-Description", "PGP/MIME version identification")
	control.Set("Content-Type", "application/pgp-encrypted")
	attachment.Set("Content-Description", "OpenPGP encrypted message")
	attachment.Set("Content-Disposition", "inline; filename=encrypted.asc")
	attachment.Set("Content-Type", mime.FormatMediaType("application/octet-stream", map[string]string{"name": "encrypted.asc"}))

	if err := textproto.WriteHeader(w, h.Header); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "--%v\r\n", boundary); err != nil {
		return err
	}
	if err := textproto.WriteHeader(w, control); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Version: 1\r\n--%v\r\n", boundary); err != nil {
		return err
	}
	if err := textproto.WriteHeader(w, attachment); err != nil {
		return err
	}
	if err := encryptArmored(w, key, bytes.NewReader(literal), key); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "\r\n--%v--\r\n", boundary)
	return err
}

// encryptArmored writes data encrypted to key, and signed by signer if it
// isn't nil, to w as an armored PGP message.
func encryptArmored(w io.Writer, key *openpgp.Entity, data io.Reader, signer *openpgp.Entity) error {
	aw, err := armor.Encode(w, "PGP MESSAGE", nil)
	if err != nil {
		return err
	}
	if err := encryptTo(aw, key, data, signer); err != nil {
		return err
	}
	return aw.Close()
}

func encryptTo(w io.Writer, key *openpgp.Entity, data io.Reader, signer *openpgp.Entity) error {
	pw, err := openpgp.Encrypt(w, []*openpgp.Entity{key}, signer, nil, nil)
	if err != nil {
		return err
	}
	// In bounded writes: go-crypto buffers each Write whole before it splits
	// it into packets (packet.partialLengthWriter), and its cipher allocates
	// an output of the Write's size, so a bytes.Reader's one WriteTo of the
	// whole message held two more copies of it. The struct hides WriteTo.
	if _, err := io.CopyBuffer(pw, struct{ io.Reader }{data}, make([]byte, 64<<10)); err != nil {
		return err
	}
	return pw.Close()
}

// charsetEncoding looks a charset up as go-proton-api does: by its IANA
// name, by its "cs" alias, by its HTML label.
func charsetEncoding(charset string) (encoding.Encoding, error) {
	name := strings.ToLower(charset)
	if e, err := ianaindex.MIME.Encoding(name); e != nil && err == nil {
		return e, nil
	} else if e, err := ianaindex.MIME.Encoding("cs" + name); e != nil && err == nil {
		return e, nil
	} else if e, err := htmlindex.Get(name); err == nil {
		return e, nil
	}
	return nil, fmt.Errorf("unknown charset: %s", charset)
}
