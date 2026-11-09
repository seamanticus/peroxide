package protonmail

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// CreateLabel, UpdateLabel and DeleteLabel send what go-proton-api sends to
// /core/v4/labels: a ParentID only when there is one, the color given (Proton
// keeps none of a request that leaves it out), and the label ID escaped in
// the path. A reply without a label is an error, not a label with no ID.
// evidence: witnesses
func TestLabelRequests(t *testing.T) {
	type request struct {
		method, path string
		body         map[string]any
	}
	label := `{"Code":1000,"Label":{"ID":"n","Name":"x","Type":3,"Exclusive":1}}`
	for _, tc := range []struct {
		name  string
		call  func(c *Client) error
		reply string
		want  request
	}{
		{"create folder", func(c *Client) error {
			_, err := c.CreateLabel("a/b", LabelFolder, "p")
			return err
		}, label, request{"POST", "/core/v4/labels", map[string]any{"Name": "a/b", "Color": NewLabelColor, "Type": 3.0, "ParentID": "p"}}},
		{"create label", func(c *Client) error {
			_, err := c.CreateLabel("work", LabelMessage, "")
			return err
		}, label, request{"POST", "/core/v4/labels", map[string]any{"Name": "work", "Color": NewLabelColor, "Type": 1.0}}},
		{"update", func(c *Client) error {
			_, err := c.UpdateLabel("a/b==", "c", "#123", "")
			return err
		}, label, request{"PUT", "/core/v4/labels/a%2Fb==", map[string]any{"Name": "c", "Color": "#123"}}},
		{"delete", func(c *Client) error { return c.DeleteLabel("id") },
			`{"Code":1000}`, request{"DELETE", "/core/v4/labels/id", nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got request
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got.method, got.path = r.Method, r.URL.EscapedPath()
				if b, _ := io.ReadAll(r.Body); len(b) > 0 {
					if err := json.Unmarshal(b, &got.body); err != nil {
						t.Errorf("body %q: %v", b, err)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, tc.reply)
			}))
			defer srv.Close()
			if err := tc.call(&Client{RootURL: srv.URL}); err != nil {
				t.Fatalf("call = %v", err)
			}
			if got.method != tc.want.method || got.path != tc.want.path || len(got.body) != len(tc.want.body) {
				t.Fatalf("request %+v, want %+v", got, tc.want)
			}
			for k, v := range tc.want.body {
				if got.body[k] != v {
					t.Errorf("body[%q] = %v, want %v", k, got.body[k], v)
				}
			}
		})
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"Code":1000}`)
	}))
	defer srv.Close()
	if l, err := (&Client{RootURL: srv.URL}).CreateLabel("x", LabelMessage, ""); err == nil {
		t.Errorf("CreateLabel() = %+v, want an error for a reply with no label", l)
	}
}

// IsFolder holds for a folder however Proton describes it: Exclusive 1 in
// GET /labels and events, Type 3 from /core/v4/labels.
// evidence: witnesses
func TestLabelIsFolder(t *testing.T) {
	for _, tc := range []struct {
		label         Label
		folder, holds bool
	}{
		{Label{Type: LabelMessage, Exclusive: 1}, true, true},
		{Label{Type: LabelFolder}, true, true},
		{Label{Type: LabelMessage}, false, true},
		{Label{Type: LabelContact}, false, false},
		{Label{}, false, false},
	} {
		if got := tc.label.IsFolder(); got != tc.folder {
			t.Errorf("%+v.IsFolder() = %v", tc.label, got)
		}
		if got := tc.label.HoldsMessages(); got != tc.holds {
			t.Errorf("%+v.HoldsMessages() = %v", tc.label, got)
		}
	}
}
