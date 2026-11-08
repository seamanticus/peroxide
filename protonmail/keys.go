package protonmail

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
)

type PrivateKeyFlags int

const (
	PrivateKeyVerify  PrivateKeyFlags = 1
	PrivateKeyEncrypt PrivateKeyFlags = 2
)

type PrivateKey struct {
	ID          string
	Version     int
	Flags       PrivateKeyFlags
	PrivateKey  string
	Fingerprint string
	Primary     int
	Active      int
	Token       string
	Signature   string
	// TODO: Fingerprints, PublicKey, Activation
}

func (priv *PrivateKey) Entity() (*openpgp.Entity, error) {
	keyRing, err := openpgp.ReadArmoredKeyRing(strings.NewReader(priv.PrivateKey))
	if err != nil {
		return nil, fmt.Errorf("failed to read private key: %v", err)
	}
	if len(keyRing) == 0 {
		return nil, errors.New("private key is empty")
	}
	return keyRing[0], nil
}

type RecipientType int

const (
	RecipientInternal RecipientType = 1
	RecipientExternal               = 2
)

type PublicKeyResp struct {
	RecipientType RecipientType
	MIMEType      string
	Keys          []*PublicKey
}

// PublicKeyFlags are the states Proton gives a public key (go-proton-api
// KeyState).
type PublicKeyFlags int

const (
	// PublicKeyTrusted: not compromised; signatures made by it can be
	// trusted.
	PublicKeyTrusted PublicKeyFlags = 1 << iota
	// PublicKeyActive: not obsolete; mail can be encrypted to it.
	PublicKeyActive
)

type PublicKey struct {
	// Send is not in Proton's answers (they carry Flags); unused.
	Send      int
	Flags     PublicKeyFlags
	PublicKey string
}

func (pub *PublicKey) Entity() (*openpgp.Entity, error) {
	keyRing, err := openpgp.ReadArmoredKeyRing(strings.NewReader(pub.PublicKey))
	if err != nil {
		return nil, fmt.Errorf("failed to read public key: %v", err)
	}
	if len(keyRing) == 0 {
		return nil, errors.New("public key is empty")
	}
	return keyRing[0], nil
}

// GetPublicKeys retrieves the public keys of the address email. Cancelling
// ctx aborts the request.
func (c *Client) GetPublicKeys(ctx context.Context, email string) (*PublicKeyResp, error) {
	v := url.Values{}
	v.Set("Email", email)
	// TODO: Fingerprint

	req, err := c.newRequest(http.MethodGet, "/keys?"+v.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)

	var respData struct {
		resp
		*PublicKeyResp
	}
	if err := c.doJSON(req, &respData); err != nil {
		return nil, err
	}

	return respData.PublicKeyResp, nil
}
