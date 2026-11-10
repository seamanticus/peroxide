package login

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/seamanticus/peroxide/internal/protonmail"
)

const testToken = "signed-challenge-token"

func hvError(methods ...string) *protonmail.APIError {
	details, err := json.Marshal(protonmail.HumanVerification{
		Methods: methods,
		Token:   testToken,
	})
	if err != nil {
		panic(err)
	}
	return &protonmail.APIError{
		Code:    protonmail.CodeHumanVerificationRequired,
		Message: "For security reasons, please complete CAPTCHA.",
		Details: details,
	}
}

// fakeClient records the human verification details of each attempt and
// replays canned results.
type fakeClient struct {
	results []error
	auths   []*protonmail.Auth
	hvSeen  []*protonmail.HumanVerification
}

func (c *fakeClient) AuthWithVerification(username, password string, info *protonmail.AuthInfo, hv *protonmail.HumanVerification) (*protonmail.Auth, error) {
	n := len(c.hvSeen)
	c.hvSeen = append(c.hvSeen, hv)
	if n >= len(c.results) {
		return nil, errors.New("unexpected authentication attempt")
	}
	var auth *protonmail.Auth
	if n < len(c.auths) {
		auth = c.auths[n]
	}
	return auth, c.results[n]
}

// terminal returns the read end of a pipe holding typed, and closes the write
// end when close is set. Pipes support read deadlines, as /dev/tty does.
func terminal(t *testing.T, typed string, close bool) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		r.Close()
		w.Close()
	})
	if _, err := w.WriteString(typed); err != nil {
		t.Fatal(err)
	}
	if close {
		w.Close()
	}
	return r
}

// browser records the URLs it is asked to open.
type browser struct {
	opened []string
	err    error
}

func (b *browser) open(url string) error {
	b.opened = append(b.opened, url)
	return b.err
}

// enterOpts returns options whose user presses Enter right away.
func enterOpts(t *testing.T, out *bytes.Buffer, b *browser) Options {
	return Options{
		Mode:    ModeBrowser,
		Timeout: time.Minute,
		Input:   terminal(t, "\n", false),
		Output:  out,
		openURL: b.open,
	}
}

const wantVerifyURL = "https://verify.proton.me/?methods=captcha&token=" + testToken

func hvFor(methods ...string) *protonmail.HumanVerification {
	return &protonmail.HumanVerification{Methods: methods, Token: testToken}
}

func TestAuthenticate_NoVerificationNeeded(t *testing.T) {
	want := &protonmail.Auth{UID: "uid"}
	c := &fakeClient{results: []error{nil}, auths: []*protonmail.Auth{want}}

	var out bytes.Buffer
	var b browser
	got, err := Authenticate(t.Context(), c, "user@proton.me", "hunter2", enterOpts(t, &out, &b))
	if err != nil {
		t.Fatalf("Authenticate() = %v", err)
	}
	if got != want {
		t.Errorf("Authenticate() = %v, want %v", got, want)
	}
	if len(c.hvSeen) != 1 {
		t.Errorf("%v authentication attempts, want 1", len(c.hvSeen))
	}
	if c.hvSeen[0] != nil {
		t.Error("human verification headers sent on a login that didn't need them")
	}
	if out.Len() != 0 || len(b.opened) != 0 {
		t.Errorf("unexpected user-facing output %q or browser %v", out.String(), b.opened)
	}
}

func TestAuthenticate_CaptchaThenSuccess(t *testing.T) {
	want := &protonmail.Auth{UID: "uid"}
	c := &fakeClient{
		results: []error{hvError("captcha", "email"), nil},
		auths:   []*protonmail.Auth{nil, want},
	}

	var out bytes.Buffer
	var b browser
	got, err := Authenticate(t.Context(), c, "user@proton.me", "hunter2", enterOpts(t, &out, &b))
	if err != nil {
		t.Fatalf("Authenticate() = %v", err)
	}
	if got != want {
		t.Errorf("Authenticate() = %v, want %v", got, want)
	}
	if len(c.hvSeen) != 2 {
		t.Fatalf("%v authentication attempts, want 2", len(c.hvSeen))
	}
	// The verification page hands its proof to Proton itself: the retry
	// carries the original challenge token, as the CAPTCHA method.
	if c.hvSeen[1] == nil {
		t.Fatal("the second attempt carried no verification details")
	}
	if c.hvSeen[1].Token != testToken {
		t.Errorf("second attempt sent %q, want the original challenge token", c.hvSeen[1].Token)
	}
	if c.hvSeen[1].SolvedMethod != protonmail.CaptchaMethod {
		t.Errorf("second attempt sent method %q, want %q", c.hvSeen[1].SolvedMethod, protonmail.CaptchaMethod)
	}
	// Only the CAPTCHA is offered, even when Proton offers more methods.
	if len(b.opened) != 1 || b.opened[0] != wantVerifyURL {
		t.Errorf("browser opened %v, want [%v]", b.opened, wantVerifyURL)
	}
	if !strings.Contains(out.String(), wantVerifyURL) {
		t.Errorf("the verification URL was not printed:\n%v", out.String())
	}
}

func TestAuthenticate_RejectedAfterCompletion(t *testing.T) {
	c := &fakeClient{results: []error{hvError("captcha"), hvError("captcha")}}

	_, err := Authenticate(t.Context(), c, "user@proton.me", "hunter2", enterOpts(t, &bytes.Buffer{}, &browser{}))
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("Authenticate() = %v, want ErrRejected", err)
	}
	if len(c.hvSeen) != 2 {
		t.Errorf("%v authentication attempts, want exactly 2 (one CAPTCHA-assisted retry)", len(c.hvSeen))
	}
}

func TestAuthenticate_InvalidTokenAfterCompletion(t *testing.T) {
	c := &fakeClient{results: []error{
		hvError("captcha"),
		&protonmail.APIError{Code: protonmail.CodeHumanVerificationInvalidToken, Message: "Invalid verification token"},
	}}

	_, err := Authenticate(t.Context(), c, "user@proton.me", "hunter2", enterOpts(t, &bytes.Buffer{}, &browser{}))
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("Authenticate() = %v, want ErrRejected", err)
	}
}

func TestAuthenticate_Disabled(t *testing.T) {
	c := &fakeClient{results: []error{hvError("captcha")}}

	opts := enterOpts(t, &bytes.Buffer{}, &browser{})
	opts.Mode = ModeDisabled
	_, err := Authenticate(t.Context(), c, "user@proton.me", "hunter2", opts)
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("Authenticate() = %v, want ErrDisabled", err)
	}
	if len(c.hvSeen) != 1 {
		t.Errorf("%v authentication attempts, want 1", len(c.hvSeen))
	}
}

func TestAuthenticate_UnsupportedMethod(t *testing.T) {
	c := &fakeClient{results: []error{hvError("sms", "email")}}

	_, err := Authenticate(t.Context(), c, "user@proton.me", "hunter2", enterOpts(t, &bytes.Buffer{}, &browser{}))
	if !errors.Is(err, ErrUnsupportedMethod) {
		t.Fatalf("Authenticate() = %v, want ErrUnsupportedMethod", err)
	}
}

func TestAuthenticate_MalformedChallenge(t *testing.T) {
	c := &fakeClient{results: []error{&protonmail.APIError{
		Code:    protonmail.CodeHumanVerificationRequired,
		Message: "For security reasons, please complete CAPTCHA.",
	}}}

	_, err := Authenticate(t.Context(), c, "user@proton.me", "hunter2", enterOpts(t, &bytes.Buffer{}, &browser{}))
	if err == nil {
		t.Fatal("Authenticate() = nil, want an error")
	}
	if len(c.hvSeen) != 1 {
		t.Errorf("%v authentication attempts, want 1", len(c.hvSeen))
	}
}

func TestAuthenticate_OtherErrorIsUnchanged(t *testing.T) {
	want := &protonmail.APIError{Code: 8002, Message: "Incorrect login credentials"}
	c := &fakeClient{results: []error{want}}

	_, err := Authenticate(t.Context(), c, "user@proton.me", "hunter2", enterOpts(t, &bytes.Buffer{}, &browser{}))
	if err != error(want) {
		t.Fatalf("Authenticate() = %v, want the original API error", err)
	}
}

// TestAuthenticate_NoReplayWithoutEnter checks that a wait that ends without
// Enter never spends the challenge token on a replay.
func TestAuthenticate_NoReplayWithoutEnter(t *testing.T) {
	c := &fakeClient{results: []error{hvError("captcha"), nil}}

	opts := enterOpts(t, &bytes.Buffer{}, &browser{})
	opts.Input = terminal(t, "", true)
	_, err := Authenticate(t.Context(), c, "user@proton.me", "hunter2", opts)
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("Authenticate() = %v, want ErrCanceled", err)
	}
	if len(c.hvSeen) != 1 {
		t.Errorf("%v authentication attempts, want 1", len(c.hvSeen))
	}
}

func TestSolve_ManualModeDoesNotOpenBrowser(t *testing.T) {
	var out bytes.Buffer
	var b browser
	opts := enterOpts(t, &out, &b)
	opts.Mode = ModeManual
	if err := Solve(t.Context(), &protonmail.HumanVerification{Methods: []string{"captcha"}, Token: testToken}, opts); err != nil {
		t.Fatalf("Solve() = %v", err)
	}
	if len(b.opened) != 0 {
		t.Errorf("manual mode opened a browser on %v", b.opened)
	}
	if !strings.Contains(out.String(), wantVerifyURL) {
		t.Errorf("the verification URL was not printed:\n%v", out.String())
	}
}

func TestSolve_BrowserFailureIsReported(t *testing.T) {
	var out bytes.Buffer
	b := browser{err: errors.New("no browser here")}
	if err := Solve(t.Context(), &protonmail.HumanVerification{Methods: []string{"captcha"}, Token: testToken}, enterOpts(t, &out, &b)); err != nil {
		t.Fatalf("Solve() = %v, want the wait to go on without a browser", err)
	}
	if !strings.Contains(out.String(), "no browser here") {
		t.Errorf("the browser failure was not reported:\n%v", out.String())
	}
}

// TestSolve_WaitEnds checks every way the wait for Enter ends without one.
// None depends on timing: no input ever arrives.
func TestSolve_WaitEnds(t *testing.T) {
	hv := &protonmail.HumanVerification{Methods: []string{"captcha"}, Token: testToken}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	tests := []struct {
		name    string
		ctx     context.Context
		timeout time.Duration
		input   func(t *testing.T) *os.File
		want    error
	}{
		{"timeout", t.Context(), time.Nanosecond, func(t *testing.T) *os.File { return terminal(t, "", false) }, ErrTimeout},
		{"canceled", canceled, time.Hour, func(t *testing.T) *os.File { return terminal(t, "", false) }, ErrCanceled},
		{"input closed", t.Context(), time.Hour, func(t *testing.T) *os.File { return terminal(t, "", true) }, ErrCanceled},
		{"no input", t.Context(), time.Hour, func(*testing.T) *os.File { return nil }, ErrNoInput},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := enterOpts(t, &bytes.Buffer{}, &browser{})
			opts.Timeout = tt.timeout
			opts.Input = tt.input(t)
			if err := Solve(tt.ctx, hv, opts); !errors.Is(err, tt.want) {
				t.Errorf("Solve() = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestSolve_InputWithoutDeadlines(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	opts := enterOpts(t, &bytes.Buffer{}, &browser{})
	opts.Input = f
	err = Solve(t.Context(), &protonmail.HumanVerification{Methods: []string{"captcha"}, Token: testToken}, opts)
	if !errors.Is(err, ErrNoInput) {
		t.Errorf("Solve() = %v, want ErrNoInput for a file without read deadlines", err)
	}
}

func TestParseMode(t *testing.T) {
	for _, s := range []string{"browser", "manual", "disabled"} {
		if _, err := ParseMode(s); err != nil {
			t.Errorf("ParseMode(%q) = %v", s, err)
		}
	}
	for _, s := range []string{"", "auto", "BROWSER", "none"} {
		if _, err := ParseMode(s); err == nil {
			t.Errorf("ParseMode(%q) = nil error", s)
		}
	}
}

func TestDefaults(t *testing.T) {
	opts, err := Options{}.withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	if opts.Mode != ModeBrowser {
		t.Errorf("Mode = %v, want %v", opts.Mode, ModeBrowser)
	}
	if opts.Timeout != DefaultTimeout {
		t.Errorf("Timeout = %v, want %v", opts.Timeout, DefaultTimeout)
	}
}
