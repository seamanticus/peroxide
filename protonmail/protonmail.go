// Package protonmail implements a ProtonMail API client.
package protonmail

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"

	"log"
)

const Version = 3

const headerAPIVersion = "X-Pm-Apiversion"

type resp struct {
	Code int
	*RawAPIError
}

func (r *resp) Err() error {
	// Details alone doesn't make a response an error: only report one when the
	// API actually returned an error message.
	if err := r.RawAPIError; err != nil && err.Message != "" {
		return &APIError{
			Code:    r.Code,
			Message: err.Message,
			Details: err.Details,
		}
	}
	return nil
}

type maybeError interface {
	Err() error
}

type RawAPIError struct {
	Message string `json:"Error"`
	// Details holds request-specific error details, e.g. a human verification
	// challenge. Its contents vary from one endpoint to another.
	Details json.RawMessage `json:"Details,omitempty"`
}

type APIError struct {
	// Status is the HTTP status of the response, 0 when the error didn't come
	// from one (e.g. an action's result for one message).
	Status  int
	Code    int
	Message string
	Details json.RawMessage
	// RetryAfter is how long the response's Retry-After asks to wait before
	// the next request (RFC 9110 section 10.2.3), 0 if it gives none.
	RetryAfter time.Duration
}

func (err *APIError) Error() string {
	return fmt.Sprintf("[%v] %v", err.Code, err.Message)
}

// retryAfter reads the Retry-After of h: a number of seconds or an HTTP
// date, relative to now. One missing, malformed or in the past is 0.
func retryAfter(h http.Header, now time.Time) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseUint(v, 10, 32); err == nil {
		return time.Duration(secs) * time.Second
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return 0
	}
	if d := t.Sub(now); d > 0 {
		return d
	}
	return 0
}

type Timestamp int64

func (t Timestamp) Time() time.Time {
	return time.Unix(int64(t), 0)
}

// Client is a ProtonMail API client.
type Client struct {
	RootURL    string
	AppVersion string
	// Debug logs requests and responses, credentials redacted. Responses are
	// logged as received rather than as decoded, so fields this client doesn't
	// know about are visible.
	Debug bool

	HTTPClient *http.Client
	ReAuth     func() error

	uid         string
	accessToken string
	keyRing     openpgp.EntityList
}

func (c *Client) setRequestAuthorization(req *http.Request) {
	if c.uid != "" && c.accessToken != "" {
		req.Header.Set("X-Pm-Uid", c.uid)
		req.Header.Set("Authorization", "Bearer "+c.accessToken)
	}
}

func (c *Client) newRequest(method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, c.RootURL+path, body)
	if err != nil {
		return nil, err
	}

	if c.Debug {
		// RequestURI: the path and the query, such as ListMessages' filter.
		log.Printf(">> %v %v\n", req.Method, req.URL.RequestURI())
	}

	req.Header.Set("X-Pm-Appversion", c.AppVersion)
	req.Header.Set(headerAPIVersion, strconv.Itoa(Version))
	c.setRequestAuthorization(req)
	return req, nil
}

func (c *Client) newJSONRequest(method, path string, body any) (*http.Request, error) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		return nil, err
	}
	b := buf.Bytes()

	req, err := c.newRequest(method, path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}

	if c.Debug {
		log.Print(string(redactJSON(b)))
	}

	req.Header.Set("Content-Type", "application/json")
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(b)), nil
	}
	return req, nil
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:101.0) Gecko/20100101 Firefox/101.0")

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return resp, err
	}

	// Check if access token has expired
	_, hasAuth := req.Header["Authorization"]
	canRetry := req.Body == nil || req.GetBody != nil
	if resp.StatusCode == http.StatusUnauthorized && hasAuth && c.ReAuth != nil && canRetry {
		resp.Body.Close()
		c.accessToken = ""
		if err := c.ReAuth(); err != nil {
			return resp, err
		}
		c.setRequestAuthorization(req) // Access token has changed
		if req.Body != nil {
			body, err := req.GetBody()
			if err != nil {
				return resp, err
			}
			req.Body = body
		}
		return c.do(req)
	}

	return resp, nil
}

func (c *Client) doJSON(req *http.Request, respData any) error {
	req.Header.Set("Accept", "application/json")

	if respData == nil {
		respData = new(resp)
	}

	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var body io.Reader = resp.Body
	var raw []byte
	if c.Debug {
		// Read the whole body so it can be logged even if it fails to decode.
		if raw, err = io.ReadAll(resp.Body); err != nil {
			return fmt.Errorf("cannot read response to %v %v: %w", req.Method, req.URL.Path, err)
		}
		body = bytes.NewReader(raw)
	}

	decodeErr := json.NewDecoder(body).Decode(respData)

	if c.Debug {
		log.Printf("<< %v %v %v", req.Method, req.URL.Path, resp.Status)
		if json.Valid(raw) {
			log.Printf("%s", redactJSON(raw))
		} else {
			// It can't be searched for secrets: describe it instead.
			log.Printf("(%v bytes of %q withheld: not JSON)", len(raw), resp.Header.Get("Content-Type"))
		}
	}

	if decodeErr != nil {
		if resp.StatusCode >= http.StatusBadRequest {
			// A gateway's HTML page, say: the status is what callers can
			// act on (a 502 is worth retrying).
			return &APIError{
				Status:     resp.StatusCode,
				Message:    fmt.Sprintf("%v, with a body that isn't an API response: %v", resp.Status, decodeErr),
				RetryAfter: retryAfter(resp.Header, time.Now()),
			}
		}
		return decodeErr
	}

	if maybeError, ok := respData.(maybeError); ok {
		if err := maybeError.Err(); err != nil {
			if apiErr, ok := err.(*APIError); ok {
				apiErr.Status = resp.StatusCode
				apiErr.RetryAfter = retryAfter(resp.Header, time.Now())
			}
			log.Printf("request failed: %v %v: %v", req.Method, req.URL.String(), err)
			return err
		}
	}
	return nil
}
