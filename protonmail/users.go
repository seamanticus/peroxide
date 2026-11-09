package protonmail

import (
	"net/http"
)

type User struct {
	ID         string
	Name       string
	UsedSpace  int64
	Currency   string // e.g. EUR
	Credit     int
	MaxSpace   int64
	MaxUpload  int
	Role       int // TODO
	Private    int
	Subscribed int // TODO
	Services   int // TODO
	Delinquent int
	Keys       []*PrivateKey
}

func (c *Client) GetCurrentUser() (*User, error) {
	return c.currentUser(true)
}

// currentUser is GetCurrentUser; renew says whether a 401 may renew the
// session (see withoutRenewal).
func (c *Client) currentUser(renew bool) (*User, error) {
	req, err := c.newRequest(http.MethodGet, "/users", nil)
	if err != nil {
		return nil, err
	}
	if !renew {
		req = withoutRenewal(req)
	}

	var respData struct {
		resp
		User *User
	}
	if err := c.doJSON(req, &respData); err != nil {
		return nil, err
	}

	return respData.User, nil
}
