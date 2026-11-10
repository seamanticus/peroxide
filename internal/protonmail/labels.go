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

// rootFolder is the ParentID Proton's web client sends to put a folder at
// the top (WebClients, constants.ts ROOT_FOLDER, FolderTreeViewList.tsx).
// go-proton-api can't send it: its ParentID is a string with omitempty.
const rootFolder = 0

// UpdateLabel renames the folder or label id, keeping color. A folder is put
// in parentID, or at the top for "" (ParentID 0, as the web client sends
// when a folder is dragged to the top); a label has no parent, and the
// request none.
func (c *Client) UpdateLabel(id, name, color string, folder bool, parentID string) (*Label, error) {
	reqData := struct {
		Name     string
		Color    string
		ParentID any `json:",omitempty"` // nil for a label
	}{Name: name, Color: color}
	if folder {
		reqData.ParentID = parentID
		if parentID == "" {
			reqData.ParentID = rootFolder
		}
	}
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
