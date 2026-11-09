package protonmail

import (
	"errors"
	"net/http"
	"net/url"
)

const (
	LabelInbox    = "0"
	LabelAllDraft = "1"
	LabelAllSent  = "2"
	LabelTrash    = "3"
	LabelSpam     = "4"
	LabelAllMail  = "5"
	LabelArchive  = "6"
	LabelSent     = "7"
	LabelDraft    = "8"
	LabelStarred  = "10"
	// LabelAllScheduled holds the messages scheduled to be sent. peroxide
	// shows no mailbox for it.
	LabelAllScheduled = "12"
)

type LabelType int

// GET /labels and events give folders and labels both LabelMessage, telling
// them apart by Exclusive; /core/v4/labels, which creates and updates them,
// takes LabelFolder for a folder, and may answer with it.
const (
	LabelMessage LabelType = 1
	LabelContact LabelType = 2
	LabelFolder  LabelType = 3
)

// IsFolder reports whether l is a folder, a message is in one, rather than a
// label, a message has any number of.
func (l *Label) IsFolder() bool {
	return l.Exclusive == 1 || l.Type == LabelFolder
}

// HoldsMessages reports whether l is a folder or a label, not a contact
// group.
func (l *Label) HoldsMessages() bool {
	return l.Type == LabelMessage || l.Type == LabelFolder
}

type Label struct {
	ID string
	// The folder this one is in, "" at the top. Path is not decoded: it
	// escapes a "/" in a name as "\/" but leaves "\" alone, so "A\" holding
	// "B" and a folder named "A/B" have the same Path.
	ParentID  string
	Name      string
	Color     string
	Display   int
	Type      LabelType
	Exclusive int // 1 for a folder, 0 for a label; both have Type LabelMessage
	Notify    int
	Order     int
}

func (c *Client) ListLabels() ([]*Label, error) {
	req, err := c.newRequest(http.MethodGet, "/labels", nil)
	if err != nil {
		return nil, err
	}

	var respData struct {
		resp
		Labels []*Label
	}
	if err := c.doJSON(req, &respData); err != nil {
		return nil, err
	}

	return respData.Labels, nil
}

// NewLabelColor is the color peroxide gives the folders and labels it creates,
// proton-bridge's (connector.go, createFolder and createLabel).
const NewLabelColor = "#f66"

// CreateLabel creates a folder (LabelFolder, in parentID, or at the top for
// "") or a label (LabelMessage, parentID ""), as proton-bridge does through
// go-proton-api (POST /core/v4/labels), and returns it as Proton made it.
func (c *Client) CreateLabel(name string, t LabelType, parentID string) (*Label, error) {
	reqData := struct {
		Name     string
		Color    string
		Type     LabelType
		ParentID string `json:",omitempty"`
	}{name, NewLabelColor, t, parentID}
	req, err := c.newJSONRequest(http.MethodPost, "/core/v4/labels", &reqData)
	if err != nil {
		return nil, err
	}
	return c.doLabel(req)
}

// UpdateLabel renames the folder or label id, and moves a folder into
// parentID, keeping color, as go-proton-api's UpdateLabel does: parentID ""
// is left out of the request, as bridge leaves it.
func (c *Client) UpdateLabel(id, name, color, parentID string) (*Label, error) {
	reqData := struct {
		Name     string
		Color    string
		ParentID string `json:",omitempty"`
	}{name, color, parentID}
	req, err := c.newJSONRequest(http.MethodPut, "/core/v4/labels/"+url.PathEscape(id), &reqData)
	if err != nil {
		return nil, err
	}
	return c.doLabel(req)
}

// DeleteLabel deletes the folder or label id (DELETE /core/v4/labels/{id}).
func (c *Client) DeleteLabel(id string) error {
	req, err := c.newRequest(http.MethodDelete, "/core/v4/labels/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	return c.doJSON(req, nil)
}

// doLabel sends a request answered with one label.
func (c *Client) doLabel(req *http.Request) (*Label, error) {
	var respData struct {
		resp
		Label *Label
	}
	if err := c.doJSON(req, &respData); err != nil {
		return nil, err
	}
	if respData.Label == nil || respData.Label.ID == "" {
		return nil, errors.New("Proton answered with no label")
	}
	return respData.Label, nil
}
