package protonmail

import (
	"context"
	"net/http"
)

// SignExternal is the mail setting "Sign external messages"
// (go-proton-api SignExternalMessages).
type SignExternal int

const (
	SignExternalDisabled SignExternal = iota
	SignExternalEnabled
)

// MailSettings are the account's mail settings peroxide reads.
type MailSettings struct {
	// Sign says whether mail to recipients without a key is signed.
	Sign SignExternal
}

// GetMailSettings returns the account's mail settings
// (GET /mail/v4/settings). Cancelling ctx aborts the request.
func (c *Client) GetMailSettings(ctx context.Context) (*MailSettings, error) {
	req, err := c.newRequest(http.MethodGet, "/mail/v4/settings", nil)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)

	var respData struct {
		resp
		MailSettings *MailSettings
	}
	if err := c.doJSON(req, &respData); err != nil {
		return nil, err
	}
	if respData.MailSettings == nil {
		return &MailSettings{}, nil
	}
	return respData.MailSettings, nil
}
