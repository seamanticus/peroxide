package protonmail

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

type MessageType int

const (
	MessageInbox MessageType = iota
	MessageDraft
	MessageSent
	MessageInboxAndSent
)

type MessageEncryption int

const (
	MessageUnencrypted MessageEncryption = iota
	MessageEncryptedInternal
	MessageEncryptedExternal
	MessageEncryptedOutside
	_
	_
	_
	MessageEncryptedInlinePGP
	MessageEncryptedPGPMIME
	_
	_
)

type MessageAction int

const (
	MessageReply MessageAction = iota
	MessageReplyAll
	MessageForward
)

type MessageAddress struct {
	Address string
	Name    string
}

type Message struct {
	ID             string `json:",omitempty"`
	Order          int64
	ConversationID string `json:",omitempty"`
	Subject        string
	Unread         int
	Type           MessageType
	Sender         *MessageAddress
	ToList         []*MessageAddress
	Time           Timestamp
	Size           int64
	NumAttachments int
	IsEncrypted    MessageEncryption
	ExpirationTime Timestamp
	IsReplied      int
	IsRepliedAll   int
	IsForwarded    int
	SpamScore      int
	AddressID      string
	Body           string
	MIMEType       string `json:",omitempty"`
	CCList         []*MessageAddress
	BCCList        []*MessageAddress
	ReplyTos       []*MessageAddress
	Header         string `json:",omitempty"`
	Attachments    []*Attachment
	LabelIDs       []string
	ExternalID     string `json:",omitempty"`
}

// Read decrypts the body of msg with keyring. Proton stores bodies as armored
// PGP messages whatever IsEncrypted says about how they were sent, so the
// body itself decides: one that doesn't start with a PGP message is returned
// as is. A body encrypted to no key of keyring gives
// openpgp/errors.ErrKeyIncorrect, as when the sender encrypted it end to end
// to a key Proton doesn't have.
func (msg *Message) Read(keyring openpgp.KeyRing, prompt openpgp.PromptFunction) (*openpgp.MessageDetails, error) {
	if !isPGPMessage(msg.Body) {
		return &openpgp.MessageDetails{
			IsEncrypted:    false,
			IsSigned:       false,
			UnverifiedBody: strings.NewReader(msg.Body),
		}, nil
	}

	block, err := armor.Decode(strings.NewReader(msg.Body))
	if err != nil {
		return nil, fmt.Errorf("cannot read armored body: %w", err)
	}
	return openpgp.ReadMessage(block.Body, keyring, prompt, nil)
}

func isPGPMessage(body string) bool {
	return strings.HasPrefix(strings.TrimLeft(body, " \t\r\n"), "-----BEGIN PGP MESSAGE-----")
}

// EncryptedTo reads the key packets of the body of msg, and nothing after
// them, and returns the IDs of the keys it is encrypted to, 0 for a hidden
// recipient (RFC 9580 section 5.1). A body that isn't a PGP message has none.
func (msg *Message) EncryptedTo() ([]uint64, error) {
	if !isPGPMessage(msg.Body) {
		return nil, nil
	}
	block, err := armor.Decode(strings.NewReader(msg.Body))
	if err != nil {
		return nil, fmt.Errorf("cannot read armored body: %w", err)
	}
	var keyIDs []uint64
	packets := packet.NewReader(block.Body)
	for {
		p, err := packets.Next()
		if err == io.EOF {
			return keyIDs, nil
		} else if err != nil {
			return nil, fmt.Errorf("cannot read key packets: %w", err)
		}
		switch p := p.(type) {
		case *packet.EncryptedKey:
			keyIDs = append(keyIDs, p.KeyId)
		case *packet.SymmetricKeyEncrypted:
		default:
			return keyIDs, nil
		}
	}
}

type messageWriter struct {
	plaintext  io.WriteCloser
	ciphertext io.WriteCloser
	b          *bytes.Buffer
	msg        *Message
}

func (w *messageWriter) Write(p []byte) (n int, err error) {
	return w.plaintext.Write(p)
}

func (w *messageWriter) Close() error {
	if err := w.plaintext.Close(); err != nil {
		return err
	}
	if err := w.ciphertext.Close(); err != nil {
		return err
	}
	w.msg.Body = w.b.String()
	return nil
}

func (msg *Message) Encrypt(to []*openpgp.Entity, signed *openpgp.Entity) (plaintext io.WriteCloser, err error) {
	var b bytes.Buffer
	ciphertext, err := armor.Encode(&b, "PGP MESSAGE", nil)
	if err != nil {
		return nil, err
	}

	plaintext, err = openpgp.Encrypt(ciphertext, to, signed, nil, nil)
	if err != nil {
		return nil, err
	}

	return &messageWriter{
		plaintext:  plaintext,
		ciphertext: ciphertext,
		b:          &b,
		msg:        msg,
	}, nil
}

type MessageFilter struct {
	Page     int
	PageSize int
	Limit    int

	Label        string
	Sort         string
	Asc          bool
	Begin        int64
	End          int64
	Keyword      string
	To           string
	From         string
	Subject      string
	Attachments  *bool
	Starred      *bool
	Unread       *bool
	Conversation string
	AddressID    string
	ID           []string
	ExternalID   string
}

func (c *Client) ListMessages(filter *MessageFilter) (total int, messages []*Message, err error) {
	v := url.Values{}
	if filter.Page != 0 {
		v.Set("Page", strconv.Itoa(filter.Page))
	}
	if filter.PageSize != 0 {
		v.Set("PageSize", strconv.Itoa(filter.PageSize))
	}
	if filter.Limit != 0 {
		v.Set("Limit", strconv.Itoa(filter.Limit))
	}
	if filter.Label != "" {
		v.Set("Label", filter.Label)
	}
	if filter.Sort != "" {
		v.Set("Sort", filter.Sort)
	}
	if filter.Asc {
		v.Set("Desc", "0")
	}
	if filter.Conversation != "" {
		v.Set("Conversation", filter.Conversation)
	}
	if filter.AddressID != "" {
		v.Set("AddressID", filter.AddressID)
	}
	if filter.ExternalID != "" {
		v.Set("ExternalID", filter.ExternalID)
	}

	req, err := c.newRequest(http.MethodGet, "/messages?"+v.Encode(), nil)
	if err != nil {
		return 0, nil, err
	}

	var respData struct {
		resp
		Total    int
		Messages []*Message
	}
	if err := c.doJSON(req, &respData); err != nil {
		return 0, nil, err
	}

	return respData.Total, respData.Messages, nil
}

type MessageCount struct {
	LabelID string
	Total   int
	Unread  int
}

func (c *Client) GetMessage(id string) (*Message, error) {
	req, err := c.newRequest(http.MethodGet, "/messages/"+id, nil)
	if err != nil {
		return nil, err
	}

	var respData struct {
		resp
		Message *Message
	}
	if err := c.doJSON(req, &respData); err != nil {
		return nil, err
	}

	return respData.Message, nil
}

// CreateDraftMessage creates a new draft message. ToList, CCList, BCCList,
// Subject, Body and AddressID are required in msg.
func (c *Client) CreateDraftMessage(msg *Message, parentID string) (*Message, error) {
	var actionPtr *MessageAction
	if parentID != "" {
		// TODO: support other actions
		action := MessageReply
		actionPtr = &action
	}

	reqData := struct {
		Message  *Message
		ParentID string         `json:",omitempty"`
		Action   *MessageAction `json:",omitempty"`
	}{msg, parentID, actionPtr}
	req, err := c.newJSONRequest(http.MethodPost, "/messages", &reqData)
	if err != nil {
		return nil, err
	}

	var respData struct {
		resp
		Message *Message
	}
	if err := c.doJSON(req, &respData); err != nil {
		return nil, err
	}

	return respData.Message, nil
}

func (c *Client) UpdateDraftMessage(msg *Message) (*Message, error) {
	reqData := struct {
		Message *Message
	}{msg}
	req, err := c.newJSONRequest(http.MethodPut, "/messages/"+msg.ID, &reqData)
	if err != nil {
		return nil, err
	}

	var respData struct {
		resp
		Message *Message
	}
	if err := c.doJSON(req, &respData); err != nil {
		return nil, err
	}

	return respData.Message, nil
}

const (
	codeSuccess = 1000
	// CodeMessageDoesNotExist is the result of an action on a message that
	// doesn't exist. Proton also gives it when the action has nothing to do,
	// e.g. marking a read message as read.
	CodeMessageDoesNotExist = 2501
)

// messageActionResponse is the response to an action on messages, which holds
// one result per message.
type messageActionResponse struct {
	resp
	Responses []struct {
		ID       string
		Response struct {
			Code    int
			Message string `json:"Error"`
		}
	}
}

// failures returns the error of each message the action failed for.
func (r *messageActionResponse) failures() map[string]error {
	failed := make(map[string]error)
	for _, res := range r.Responses {
		if res.Response.Code != codeSuccess {
			failed[res.ID] = &APIError{Code: res.Response.Code, Message: res.Response.Message}
		}
	}
	return failed
}

// doMessageAction sends req, an action on messages, and returns the error of
// each message it failed for.
func (c *Client) doMessageAction(req *http.Request) (failed map[string]error, err error) {
	var respData messageActionResponse
	if err := c.doJSON(req, &respData); err != nil {
		return nil, err
	}
	return respData.failures(), nil
}

func (c *Client) doMessages(action string, ids []string) (failed map[string]error, err error) {
	reqData := struct {
		IDs []string
	}{ids}
	req, err := c.newJSONRequest(http.MethodPut, "/messages/"+action, &reqData)
	if err != nil {
		return nil, err
	}
	return c.doMessageAction(req)
}

// MarkMessagesRead marks messages as read, and returns the error of each one
// it failed for.
func (c *Client) MarkMessagesRead(ids []string) (failed map[string]error, err error) {
	return c.doMessages("read", ids)
}

// MarkMessagesUnread marks messages as unread, and returns the error of each
// one it failed for.
func (c *Client) MarkMessagesUnread(ids []string) (failed map[string]error, err error) {
	return c.doMessages("unread", ids)
}

// DeleteMessages deletes messages, and returns the error of each one it failed
// for.
func (c *Client) DeleteMessages(ids []string) (failed map[string]error, err error) {
	return c.doMessages("delete", ids)
}

// MaxMessagesPerRequest is the most message IDs one request should carry
// (go-proton-api maxPageSize).
const MaxMessagesPerRequest = 150

// maxStaleRetries bounds how often MessagesByID asks again while Proton says
// its answer is stale.
const maxStaleRetries = 5

// MessagesByID returns the metadata (labels, flags; no body) of the messages
// ids, at most MaxMessagesPerRequest, as Proton has them now. Messages it
// doesn't have are left out. The request is go-proton-api's
// GetMessageMetadataPage: a POST overridden as GET with the filter as body.
// Cancelling ctx aborts the request.
func (c *Client) MessagesByID(ctx context.Context, ids []string) ([]*Message, error) {
	if len(ids) > MaxMessagesPerRequest {
		return nil, fmt.Errorf("cannot get %v messages in one request, at most %v", len(ids), MaxMessagesPerRequest)
	}
	reqData := struct {
		ID       []string
		Desc     int
		Page     int
		PageSize int
		Sort     string
	}{ids, 0, 0, MaxMessagesPerRequest, "ID"}

	for range maxStaleRetries {
		req, err := c.newJSONRequest(http.MethodPost, "/mail/v4/messages", &reqData)
		if err != nil {
			return nil, err
		}
		req = req.WithContext(ctx)
		req.Header.Set("X-HTTP-Method-Override", http.MethodGet)

		var respData struct {
			resp
			Messages []*Message
			Stale    int
		}
		if err := c.doJSON(req, &respData); err != nil {
			return nil, err
		}
		if respData.Stale == 0 {
			return respData.Messages, nil
		}
	}
	return nil, fmt.Errorf("messages still stale after %v requests", maxStaleRetries)
}

// MetadataPageSize is how many messages MessageMetadataPage asks for, as
// proton-bridge's sync (syncservice MetadataPageSize).
const MetadataPageSize = 128

// MessageMetadataPage returns the metadata of the account's messages, every
// label included, newest first: the pageSize newest if endID is empty, else
// the pageSize newest up to endID, which Proton lists first if it still has
// it. It is go-proton-api's GetMessageMetadataPage with {Desc: true, EndID},
// as proton-bridge's sync pages through every message. Cancelling ctx aborts
// the request.
func (c *Client) MessageMetadataPage(ctx context.Context, endID string, pageSize int) ([]*Message, error) {
	reqData := struct {
		EndID    string `json:",omitempty"`
		Desc     int
		Page     int
		PageSize int
		Sort     string
	}{endID, 1, 0, pageSize, "ID"}

	for range maxStaleRetries {
		req, err := c.newJSONRequest(http.MethodPost, "/mail/v4/messages", &reqData)
		if err != nil {
			return nil, err
		}
		req = req.WithContext(ctx)
		req.Header.Set("X-HTTP-Method-Override", http.MethodGet)

		var respData struct {
			resp
			Messages []*Message
			Stale    int
		}
		if err := c.doJSON(req, &respData); err != nil {
			return nil, err
		}
		if respData.Stale == 0 {
			return respData.Messages, nil
		}
	}
	return nil, fmt.Errorf("messages still stale after %v requests", maxStaleRetries)
}

// UndeleteMessages restores deleted messages, and returns the error of each
// one it failed for.
func (c *Client) UndeleteMessages(ids []string) (failed map[string]error, err error) {
	return c.doMessages("undelete", ids)
}

// LabelMessages adds a label to messages, and returns the error of each one it
// failed for.
func (c *Client) LabelMessages(labelID string, ids []string) (failed map[string]error, err error) {
	reqData := struct {
		LabelID string
		IDs     []string
	}{labelID, ids}
	req, err := c.newJSONRequest(http.MethodPut, "/messages/label", &reqData)
	if err != nil {
		return nil, err
	}
	return c.doMessageAction(req)
}

// UnlabelMessages removes a label from messages, and returns the error of each
// one it failed for.
func (c *Client) UnlabelMessages(labelID string, ids []string) (failed map[string]error, err error) {
	reqData := struct {
		LabelID string
		IDs     []string
	}{labelID, ids}
	req, err := c.newJSONRequest(http.MethodPut, "/messages/unlabel", &reqData)
	if err != nil {
		return nil, err
	}
	return c.doMessageAction(req)
}

type MessageKeyPacket struct {
	ID         string
	KeyPackets string
}

type MessagePackageType int

const (
	MessagePackageInternal         MessagePackageType = 1
	MessagePackageEncryptedOutside                    = 2
	MessagePackageCleartext                           = 4
	MessagePackageInlinePGP                           = 8
	MessagePackagePGPMIME                             = 16
	MessagePackageMIME                                = 32
)

// From https://github.com/ProtonMail/WebClient/blob/public/src/app/composer/services/encryptMessage.js

type MessagePackage struct {
	Type MessagePackageType

	BodyKeyPacket        string            `json:",omitempty"`
	AttachmentKeyPackets map[string]string `json:",omitempty"`
	Signature            int
}

type MessagePackageSet struct {
	Type      MessagePackageType // OR of each Type
	Addresses map[string]*MessagePackage
	MIMEType  string
	Body      string // Encrypted body data packet

	// Only if cleartext is sent
	BodyKey        *PackedKey            `json:",omitempty"`
	AttachmentKeys map[string]*PackedKey `json:",omitempty"`

	bodyKey        *packet.EncryptedKey
	attachmentKeys map[string]*packet.EncryptedKey
	signature      int
}

type PackedKey struct {
	Algorithm string
	Key       string
}

func NewMessagePackageSet(attachmentKeys map[string]*packet.EncryptedKey) *MessagePackageSet {
	return &MessagePackageSet{
		Addresses:      make(map[string]*MessagePackage),
		attachmentKeys: attachmentKeys,
	}
}

type outgoingMessageWriter struct {
	cleartext  io.WriteCloser
	ciphertext io.WriteCloser
	encoded    *bytes.Buffer
	set        *MessagePackageSet
}

func (w *outgoingMessageWriter) Write(p []byte) (int, error) {
	return w.cleartext.Write(p)
}

func (w *outgoingMessageWriter) Close() error {
	if err := w.cleartext.Close(); err != nil {
		return err
	}
	if err := w.ciphertext.Close(); err != nil {
		return err
	}
	w.set.Body = w.encoded.String()
	w.encoded = nil
	return nil
}

// Encrypt encrypts the data that will be written to the returned
// io.WriteCloser, and optionally signs it.
func (set *MessagePackageSet) Encrypt(mimeType string, signed *openpgp.Entity) (io.WriteCloser, error) {
	set.MIMEType = mimeType

	config := &packet.Config{}

	key, err := generateUnencryptedKey(packet.CipherAES256, config)
	if err != nil {
		return nil, err
	}
	set.bodyKey = key

	var signer *packet.PrivateKey
	if signed != nil {
		signKey, ok := signingKey(signed, config.Now())
		if !ok {
			return nil, errors.New("no valid signing keys")
		}
		signer = signKey.PrivateKey
		if signer == nil {
			return nil, errors.New("no private key in signing key")
		}
		if signer.Encrypted {
			return nil, errors.New("signing key must be decrypted")
		}
		set.signature = 1
	}

	encoded := new(bytes.Buffer)
	ciphertext := base64.NewEncoder(base64.StdEncoding, encoded)

	cleartext, err := symetricallyEncrypt(ciphertext, key, signer, nil, config)
	if err != nil {
		return nil, err
	}

	return &outgoingMessageWriter{
		cleartext:  cleartext,
		ciphertext: ciphertext,
		encoded:    encoded,
		set:        set,
	}, nil
}

// cipherFunctionString names cipherFunc for the API, which knows the AES
// ciphers only.
func cipherFunctionString(cipherFunc packet.CipherFunction) (string, error) {
	switch cipherFunc {
	case packet.CipherAES128:
		return "aes128", nil
	case packet.CipherAES192:
		return "aes192", nil
	case packet.CipherAES256:
		return "aes256", nil
	case packet.Cipher3DES, packet.CipherCAST5:
		return "", fmt.Errorf("cipher function %v not accepted by the API", cipherFunc)
	}
	return "", fmt.Errorf("unknown cipher function %v", cipherFunc)
}

func packKey(key *packet.EncryptedKey) (*PackedKey, error) {
	algorithm, err := cipherFunctionString(key.CipherFunc)
	if err != nil {
		return nil, err
	}
	return &PackedKey{
		Algorithm: algorithm,
		Key:       base64.StdEncoding.EncodeToString(key.Key),
	}, nil
}

func (set *MessagePackageSet) AddCleartext(addr string) (*MessagePackage, error) {
	if set.BodyKey == nil || set.AttachmentKeys == nil {
		bodyKey, err := packKey(set.bodyKey)
		if err != nil {
			return nil, fmt.Errorf("cannot pack body key: %w", err)
		}

		attachmentKeys := make(map[string]*PackedKey, len(set.attachmentKeys))
		// In attachment ID order, so a failure names the same attachment
		// every time.
		for _, att := range slices.Sorted(maps.Keys(set.attachmentKeys)) {
			key := set.attachmentKeys[att]
			if attachmentKeys[att], err = packKey(key); err != nil {
				return nil, fmt.Errorf("cannot pack key of attachment %v: %w", att, err)
			}
		}

		set.BodyKey, set.AttachmentKeys = bodyKey, attachmentKeys
	}

	pkg := &MessagePackage{
		Type:      MessagePackageCleartext,
		Signature: set.signature,
	}
	set.Addresses[addr] = pkg
	set.Type |= MessagePackageCleartext

	return pkg, nil
}

func serializeEncryptedKey(symKey *packet.EncryptedKey, pub *packet.PublicKey, config *packet.Config) (string, error) {
	var encoded bytes.Buffer
	ciphertext := base64.NewEncoder(base64.StdEncoding, &encoded)

	err := packet.SerializeEncryptedKey(ciphertext, pub, symKey.CipherFunc, symKey.Key, config)
	if err != nil {
		return "", err
	}

	ciphertext.Close()

	return encoded.String(), nil
}

func (set *MessagePackageSet) AddInternal(addr string, pub *openpgp.Entity) (*MessagePackage, error) {
	config := &packet.Config{}

	encKey, ok := encryptionKey(pub, config.Now())
	if !ok {
		return nil, errors.New("cannot encrypt a message to key id " + strconv.FormatUint(pub.PrimaryKey.KeyId, 16) + " because it has no encryption keys")
	}

	bodyKey, err := serializeEncryptedKey(set.bodyKey, encKey.PublicKey, config)
	if err != nil {
		return nil, err
	}

	attachmentKeys := make(map[string]string, len(set.attachmentKeys))
	// In attachment ID order, as AddCleartext.
	for _, att := range slices.Sorted(maps.Keys(set.attachmentKeys)) {
		key := set.attachmentKeys[att]
		attKey, err := serializeEncryptedKey(key, encKey.PublicKey, config)
		if err != nil {
			return nil, err
		}
		attachmentKeys[att] = attKey
	}

	set.Type |= MessagePackageInternal
	pkg := &MessagePackage{
		Type:                 MessagePackageInternal,
		BodyKeyPacket:        bodyKey,
		AttachmentKeyPackets: attachmentKeys,
		Signature:            set.signature,
	}
	set.Addresses[addr] = pkg
	return pkg, nil
}

type OutgoingMessage struct {
	ID string

	// Only if message expires
	ExpirationTime int // Duration in seconds

	Packages []*MessagePackageSet
}

func (c *Client) SendMessage(msg *OutgoingMessage) (sent, parent *Message, err error) {
	req, err := c.newJSONRequest(http.MethodPost, "/messages/"+msg.ID, msg)
	if err != nil {
		return nil, nil, err
	}

	var respData struct {
		resp
		Sent, Parent *Message
	}
	if err := c.doJSON(req, &respData); err != nil {
		return nil, nil, err
	}

	return respData.Sent, respData.Parent, nil
}
