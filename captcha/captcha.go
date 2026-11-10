// Package captcha implements Proton's interactive human verification flow.
//
// Proton sometimes answers authentication requests with API error 9001,
// asking for a CAPTCHA to be completed before the request can be replayed.
// peroxide doesn't bypass, solve or weaken that challenge: it points the
// user's own browser at the Proton-operated verification page, waits for the
// user to press Enter once it is solved, and then replays the authentication
// request with the human verification headers Proton expects.
//
// Outside Proton's own apps, the verification page itself sends the solved
// CAPTCHA to Proton (POST /core/v4/verification/captcha/<token>, WebClients
// commit c5d23c0), so the replay carries the original challenge token and
// nothing needs to be carried back. proton-bridge's CLI works the same way
// (internal/frontend/cli, promptHvURL).
package captcha

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/seamanticus/peroxide/protonmail"
)

// Mode selects how the user is asked to complete a challenge.
type Mode string

const (
	// ModeBrowser prints the verification URL and opens a browser on it.
	ModeBrowser Mode = "browser"
	// ModeManual only prints the verification URL. Use this on headless
	// machines: the URL works from any device.
	ModeManual Mode = "manual"
	// ModeDisabled turns human verification handling off: the original API
	// error is reported to the user instead.
	ModeDisabled Mode = "disabled"
)

// ParseMode parses a -captcha-mode flag value.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeBrowser:
		return ModeBrowser, nil
	case ModeManual:
		return ModeManual, nil
	case ModeDisabled:
		return ModeDisabled, nil
	default:
		return "", fmt.Errorf("captcha: invalid mode %q (want browser, manual or disabled)", s)
	}
}

// DefaultTimeout is the default for Options.Timeout.
const DefaultTimeout = 10 * time.Minute

// Options configures the interactive human verification flow.
type Options struct {
	// Mode selects how the user is asked to complete the challenge.
	Mode Mode
	// Timeout bounds how long we wait for the user.
	Timeout time.Duration
	// Input is read for the Enter key. Required unless Mode is ModeDisabled.
	// It must support read deadlines, which bound the wait: a terminal
	// opened with os.Open("/dev/tty") or a pipe does, os.Stdin may not.
	Input *os.File
	// Output receives the user-facing instructions. Defaults to os.Stderr.
	Output io.Writer

	// openURL is a test hook standing in for the user's browser.
	openURL func(url string) error
}

// Errors reported by this package. They are always wrapped with more context,
// use errors.Is to test for them.
var (
	ErrDisabled          = errors.New("captcha: human verification is required, but CAPTCHA handling is disabled")
	ErrNoChallenge       = errors.New("captcha: the API response doesn't carry a challenge")
	ErrUnsupportedMethod = errors.New("captcha: the API doesn't offer the CAPTCHA verification method")
	ErrNoInput           = errors.New("captcha: no terminal to confirm the challenge on")
	ErrTimeout           = errors.New("captcha: timed out waiting for the challenge to be completed")
	ErrCanceled          = errors.New("captcha: canceled")
	ErrRejected          = errors.New("captcha: the challenge was rejected by Proton")
)

func (opts Options) withDefaults() (Options, error) {
	if opts.Mode == "" {
		opts.Mode = ModeBrowser
	} else if _, err := ParseMode(string(opts.Mode)); err != nil {
		return opts, err
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.Output == nil {
		opts.Output = os.Stderr
	}
	if opts.openURL == nil {
		opts.openURL = openURL
	}
	return opts, nil
}

// verifyURL is Proton's verification page offering the CAPTCHA only, the one
// method whose proof the page sends to Proton by itself.
func verifyURL(hv *protonmail.HumanVerification) string {
	return (&protonmail.HumanVerification{
		Methods: []string{protonmail.CaptchaMethod},
		Token:   hv.Token,
	}).URL()
}

// Solve points the user at Proton's verification page and waits until they
// press Enter, which they do once the page says the challenge is solved.
//
// It cannot tell a solved challenge from an abandoned one: Proton answers the
// replayed request, see Authenticate.
func Solve(ctx context.Context, hv *protonmail.HumanVerification, opts Options) error {
	opts, err := opts.withDefaults()
	if err != nil {
		return err
	}
	if opts.Mode == ModeDisabled {
		return ErrDisabled
	}
	if hv == nil || hv.Token == "" {
		return ErrNoChallenge
	}
	if !hv.HasMethod(protonmail.CaptchaMethod) {
		return fmt.Errorf("%w: it offered %v", ErrUnsupportedMethod, hv.Methods)
	}
	if opts.Input == nil {
		return ErrNoInput
	}

	u := verifyURL(hv)
	fmt.Fprintf(opts.Output, "\nProton requires human verification. Solve the CAPTCHA on this page,\n"+
		"in any browser on any device:\n\n%v\n\n", u)
	if opts.Mode == ModeBrowser {
		if err := opts.openURL(u); err != nil {
			fmt.Fprintf(opts.Output, "warning: failed to open a browser (%v)\nOpen the URL above manually.\n\n", err)
		}
	}
	fmt.Fprintf(opts.Output, "Press Enter here once the page says the verification succeeded "+
		"(up to %v, Ctrl+C to cancel): ", opts.Timeout)

	err = waitForEnter(ctx, opts.Input, opts.Timeout)
	fmt.Fprintln(opts.Output)
	if err != nil {
		return err
	}
	fmt.Fprintf(opts.Output, "Continuing authentication...\n")
	return nil
}

// waitForEnter reads one line from in. The read deadline bounds it by
// timeout, and is moved to now when ctx is done, so no read outlives the
// call.
func waitForEnter(ctx context.Context, in *os.File, timeout time.Duration) error {
	if err := in.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("%w: can't bound the wait: %v", ErrNoInput, err)
	}
	stop := context.AfterFunc(ctx, func() {
		// An error leaves the timeout deadline in place, which still ends
		// the read.
		_ = in.SetReadDeadline(time.Now())
	})
	defer stop()

	_, err := bufio.NewReader(in).ReadString('\n')
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return fmt.Errorf("%w: %v", ErrCanceled, ctx.Err())
	case errors.Is(err, os.ErrDeadlineExceeded):
		return fmt.Errorf("%w after %v", ErrTimeout, timeout)
	case errors.Is(err, io.EOF):
		return fmt.Errorf("%w: the terminal closed its input", ErrCanceled)
	default:
		return fmt.Errorf("captcha: read the Enter key: %w", err)
	}
}

// Client is the subset of *protonmail.Client used to authenticate.
type Client interface {
	AuthWithVerification(username, password string, info *protonmail.AuthInfo, hv *protonmail.HumanVerification) (*protonmail.Auth, error)
}

// Authenticate performs SRP authentication, handling at most one human
// verification challenge.
//
// When Proton asks for human verification, the user is directed to Proton's
// own verification page; once they report having completed it, the
// authentication request is replayed exactly once with the original challenge
// token, which Proton now holds the page's proof for. Any further challenge is
// reported as an error instead of looping.
func Authenticate(ctx context.Context, c Client, username, password string, opts Options) (*protonmail.Auth, error) {
	opts, err := opts.withDefaults()
	if err != nil {
		return nil, err
	}

	auth, err := c.AuthWithVerification(username, password, nil, nil)
	if err == nil {
		return auth, nil
	}

	hv, hvErr := challenge(err)
	if hv == nil {
		if hvErr != nil {
			// A human verification error we can't act on.
			return nil, hvErr
		}
		return nil, err
	}

	if opts.Mode == ModeDisabled {
		return nil, fmt.Errorf("%w: %v", ErrDisabled, err)
	}

	if err := Solve(ctx, hv, opts); err != nil {
		return nil, err
	}
	hv.SolvedMethod = protonmail.CaptchaMethod

	auth, err = c.AuthWithVerification(username, password, nil, hv)
	if err == nil {
		return auth, nil
	}

	if apiErr, ok := errors.AsType[*protonmail.APIError](err); ok {
		switch {
		case apiErr.IsHumanVerificationRequired():
			return nil, fmt.Errorf("%w: Proton asked for human verification again after the challenge was completed; "+
				"make sure the challenge was solved in the browser before continuing, then try again: %v", ErrRejected, err)
		case apiErr.Code == protonmail.CodeHumanVerificationInvalidToken:
			return nil, fmt.Errorf("%w: the challenge was not completed, already used or has expired. "+
				"Make sure Proton's page says the verification succeeded before confirming here, then try again: %v", ErrRejected, err)
		}
	}
	return nil, err
}

// challenge extracts a human verification challenge from an API error.
//
// It returns (nil, nil) when err isn't a human verification error, and
// (nil, err) when it is one we can't act on.
func challenge(err error) (*protonmail.HumanVerification, error) {
	var apiErr *protonmail.APIError
	if !errors.As(err, &apiErr) || !apiErr.IsHumanVerificationRequired() {
		return nil, nil
	}

	hv, parseErr := apiErr.HumanVerification()
	if parseErr != nil {
		return nil, parseErr
	}
	if !hv.HasMethod(protonmail.CaptchaMethod) {
		return nil, fmt.Errorf("%w: it offered %v; complete the verification in the Proton web app, then retry",
			ErrUnsupportedMethod, hv.Methods)
	}
	return hv, nil
}
