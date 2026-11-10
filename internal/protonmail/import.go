package protonmail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// MaxImportSize is the largest encrypted message the import API takes, as
// go-proton-api checks it (MaxImportSize, raised to 70 MiB in 2025-11): about
// 50 MB of attachments once base64-encoded.
const MaxImportSize = 70 << 20

// ErrImportSizeExceeded is ImportMessage's error for a message that is over
// MaxImportSize once encrypted. No request is made.
var ErrImportSizeExceeded = errors.New("message exceeds the import size limit")

// MessageFlags is the Flags bit set of a message (go-proton-api MessageFlag).
// Only the bits peroxide sets are named.
type MessageFlags int64

const (
	MessageFlagReceived MessageFlags = 1 << 0
	MessageFlagSent     MessageFlags = 1 << 1
	MessageFlagReplied  MessageFlags = 1 << 5
)

// ImportMetadata is what Proton files an imported message with.
type ImportMetadata struct {
	AddressID string
	LabelIDs  []string
	Unread    bool
	Flags     MessageFlags
}

// importMetadataJSON is ImportMetadata as the API reads it: Unread is 0 or 1.
type importMetadataJSON struct {
	AddressID string
	LabelIDs  []string
	Unread    int
	Flags     MessageFlags
}

// importName names the one message of a request, in its form field and in
// the response.
const importName = "0"

// ImportMessage encrypts literal, an RFC 822 message, to key, and imports it
// into the account with meta (POST /mail/v4/messages/import), returning the
// ID of the new message. The request holds the message, then the metadata
// keyed by its name, as go-proton-api sends them.
func (c *Client) ImportMessage(ctx context.Context, key *openpgp.Entity, meta *ImportMetadata, literal []byte) (string, error) {
	encrypted, err := encryptRFC822(key, literal)
	if err != nil {
		return "", fmt.Errorf("cannot encrypt the message: %w", err)
	}
	if len(encrypted) > MaxImportSize {
		return "", ErrImportSizeExceeded
	}

	// The request is the encrypted message between a head (the boundary and
	// the part's header) and a tail (the metadata part), sent from the three
	// without copying the message into one body: it is the largest thing
	// peroxide holds.
	var head, tail bytes.Buffer
	out := &switchWriter{&head}
	mw := multipart.NewWriter(out)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{
		"name":     importName,
		"filename": importName + ".eml",
	}))
	h.Set("Content-Type", "message/rfc822")
	part, err := mw.CreatePart(h)
	if err != nil {
		return "", err
	}
	out.w = &tail
	if _, err := part.Write([]byte("\r\n")); err != nil {
		return "", err
	}

	unread := 0
	if meta.Unread {
		unread = 1
	}
	metadata, err := json.Marshal(map[string]importMetadataJSON{importName: {
		AddressID: meta.AddressID,
		LabelIDs:  meta.LabelIDs,
		Unread:    unread,
		Flags:     meta.Flags,
	}})
	if err != nil {
		return "", err
	}
	h = make(textproto.MIMEHeader)
	h.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "Metadata"}))
	h.Set("Content-Type", "application/json")
	part, err = mw.CreatePart(h)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(metadata); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	req, err := c.newRequest(http.MethodPost, "/mail/v4/messages/import", nil)
	if err != nil {
		return "", err
	}
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	// GetBody makes the request one that can be sent again after a
	// re-authentication (Client.do).
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(io.MultiReader(bytes.NewReader(head.Bytes()), bytes.NewReader(encrypted), bytes.NewReader(tail.Bytes()))), nil
	}
	req.Body, _ = req.GetBody()
	req.ContentLength = int64(head.Len() + len(encrypted) + tail.Len())

	var respData struct {
		resp
		Responses []struct {
			Name     string
			Response struct {
				resp
				MessageID string
			}
		}
	}
	if err := c.doJSON(req, &respData); err != nil {
		return "", err
	}
	for _, res := range respData.Responses {
		if res.Name != importName {
			continue
		}
		if res.Response.Code != codeSuccess {
			msg := ""
			if res.Response.RawAPIError != nil {
				msg = res.Response.Message
			}
			return "", &APIError{Code: res.Response.Code, Message: msg}
		}
		if res.Response.MessageID == "" {
			return "", errors.New("the import response holds no message ID")
		}
		return res.Response.MessageID, nil
	}
	return "", errors.New("the import response holds no result for the message")
}

// switchWriter writes to w, which can be changed between writes.
type switchWriter struct{ w io.Writer }

func (s *switchWriter) Write(p []byte) (int, error) { return s.w.Write(p) }
