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
func encryptRFC822(key *openpgp.Entity, literal []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := encryptParts(&buf, key, literal); err != nil {
		return encryptFull(key, literal)
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
	body, err := io.ReadAll(e.Body)
	if err != nil {
		return err
	}
	h := e.Header.Copy()
	h.Del("Content-Transfer-Encoding")
	if charset, ok := params["charset"]; ok {
		if body, err = decodeCharset(body, charset); err != nil {
			return err
		}
		params["charset"] = "utf-8"
		h.Set("Content-Type", mime.FormatMediaType(mediaType, params))
	}
	armored, err := encryptArmored(key, body, nil)
	if err != nil {
		return err
	}
	if err := textproto.WriteHeader(w, h.Header); err != nil {
		return err
	}
	_, err = w.Write(armored)
	return err
}

// encryptAttachment writes a part that isn't text as a binary PGP message,
// base64-encoded.
func encryptAttachment(w io.Writer, key *openpgp.Entity, e *message.Entity) error {
	body, err := io.ReadAll(e.Body)
	if err != nil {
		return err
	}
	h := e.Header.Copy()
	h.Set("Content-Transfer-Encoding", "base64")
	var enc bytes.Buffer
	if err := encryptTo(&enc, key, body, nil); err != nil {
		return err
	}
	if err := textproto.WriteHeader(w, h.Header); err != nil {
		return err
	}
	bw := base64.NewEncoder(base64.StdEncoding, w)
	if _, err := bw.Write(enc.Bytes()); err != nil {
		return err
	}
	return bw.Close()
}

// encryptFull writes literal whole as a PGP/MIME message, signed by key,
// under a header holding the fields Proton lists messages by.
func encryptFull(key *openpgp.Entity, literal []byte) ([]byte, error) {
	armored, err := encryptArmored(key, literal, key)
	if err != nil {
		return nil, err
	}
	orig, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(literal)))
	if err != nil {
		return nil, err
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

	var b bytes.Buffer
	if err := textproto.WriteHeader(&b, h.Header); err != nil {
		return nil, err
	}
	for _, part := range []struct {
		h    textproto.Header
		body []byte
	}{{control, []byte("Version: 1")}, {attachment, armored}} {
		fmt.Fprintf(&b, "--%v\r\n", boundary)
		if err := textproto.WriteHeader(&b, part.h); err != nil {
			return nil, err
		}
		b.Write(part.body)
		b.WriteString("\r\n")
	}
	fmt.Fprintf(&b, "--%v--\r\n", boundary)
	return b.Bytes(), nil
}

// encryptArmored returns data encrypted to key, and signed by signer if it
// isn't nil, as an armored PGP message.
func encryptArmored(key *openpgp.Entity, data []byte, signer *openpgp.Entity) ([]byte, error) {
	var b bytes.Buffer
	aw, err := armor.Encode(&b, "PGP MESSAGE", nil)
	if err != nil {
		return nil, err
	}
	if err := encryptTo(aw, key, data, signer); err != nil {
		return nil, err
	}
	if err := aw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func encryptTo(w io.Writer, key *openpgp.Entity, data []byte, signer *openpgp.Entity) error {
	pw, err := openpgp.Encrypt(w, []*openpgp.Entity{key}, signer, nil, nil)
	if err != nil {
		return err
	}
	if _, err := pw.Write(data); err != nil {
		return err
	}
	return pw.Close()
}

// decodeCharset decodes body from charset to UTF-8, looking the charset up
// as go-proton-api does: by its IANA name, by its "cs" alias, by its HTML
// label.
func decodeCharset(body []byte, charset string) ([]byte, error) {
	name := strings.ToLower(charset)
	var enc encoding.Encoding
	if e, err := ianaindex.MIME.Encoding(name); e != nil && err == nil {
		enc = e
	} else if e, err := ianaindex.MIME.Encoding("cs" + name); e != nil && err == nil {
		enc = e
	} else if e, err := htmlindex.Get(name); err == nil {
		enc = e
	} else {
		return nil, fmt.Errorf("unknown charset: %s", charset)
	}
	return io.ReadAll(enc.NewDecoder().Reader(bytes.NewReader(body)))
}
