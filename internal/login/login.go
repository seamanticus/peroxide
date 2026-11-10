// Package login is the interactive `peroxide auth` dialogue: the Proton
// password, human verification, the two-factor code, the mailbox password,
// and the new bridge password it ends with.
package login

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"golang.org/x/term"

	"github.com/seamanticus/peroxide/internal/auth"
	"github.com/seamanticus/peroxide/internal/protonmail"
)

// ErrKept is returned when the user chose to keep the credentials already
// stored for the username.
var ErrKept = errors.New("login: the stored credentials were kept")

// Run logs username in and stores its credentials under a new bridge
// password, which it writes to the terminal only (/dev/tty): redirecting or
// tee-ing peroxide's output never captures it.
//
// opts.Input is set by Run: it reads the CAPTCHA confirmation from /dev/tty.
func Run(ctx context.Context, c *protonmail.Client, username string, opts Options) error {
	opts, err := opts.withDefaults()
	if err != nil {
		return err
	}

	stored, err := auth.ListUsernames()
	if err != nil {
		return err
	}
	if slices.Contains(stored, username) {
		if !confirm(opts.Output, fmt.Sprintf("Credentials for %v already exist, delete them to create new ones? [y/N] ", username)) {
			return ErrKept
		}
		if err := auth.DeleteUsername(username); err != nil {
			return err
		}
	}

	// Read and written directly, never through Fd: Fd would put the file in
	// blocking mode, and the wait for Enter relies on its read deadline.
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("login: open the terminal: %w", err)
	}
	defer tty.Close()
	opts.Input = tty

	fmt.Fprintln(opts.Output)
	pass, err := Password(opts.Output, "Your Proton Mail password")
	if err != nil {
		return err
	}
	loginPassword := string(pass)

	a, err := Authenticate(ctx, c, username, loginPassword, opts)
	if err != nil {
		return err
	}

	if a.TwoFactor.Enabled != 0 {
		if a.TwoFactor.TOTP != 1 {
			return errors.New("login: only TOTP is supported as a 2FA method")
		}
		// The code is typed in and echoed by the terminal, never logged.
		fmt.Fprintf(opts.Output, "\nYour 2FA code (6 digits): ")
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Scan()
		scope, err := c.AuthTOTP(scanner.Text())
		if err != nil {
			return err
		}
		a.Scope = scope
	}

	var mailboxPassword string
	if a.PasswordMode == protonmail.PasswordSingle {
		mailboxPassword = loginPassword
	}
	if mailboxPassword == "" {
		prompt := "Your Proton Mail password"
		if a.PasswordMode == protonmail.PasswordTwo {
			prompt = "Your Proton Mail mailbox password"
		}
		fmt.Fprintln(opts.Output)
		pass, err := Password(opts.Output, prompt)
		if err != nil {
			return err
		}
		mailboxPassword = string(pass)
	}

	keySalts, err := c.ListKeySalts()
	if err != nil {
		return err
	}
	if _, err := c.Unlock(a, keySalts, mailboxPassword); err != nil {
		return err
	}

	secretKey, bridgePassword, err := auth.GeneratePassword()
	if err != nil {
		return err
	}
	err = auth.EncryptAndSave(&auth.CachedAuth{
		Auth:            *a,
		LoginPassword:   loginPassword,
		MailboxPassword: mailboxPassword,
		KeySalts:        keySalts,
	}, username, secretKey)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(tty, "\n%v\nUse it as this account's password in your mail client.\n"+
		"peroxide doesn't store it: save it now.\n", box(colourEnv(), "Bridge password: "+bridgePassword))
	if err != nil {
		return fmt.Errorf("login: show the bridge password: %w", err)
	}
	return nil
}

// Password writes prompt to out and reads a password without echo from the
// terminal: stdin when it is one, /dev/tty otherwise. Only the prompt is
// written.
func Password(out io.Writer, prompt string) ([]byte, error) {
	f := os.Stdin
	if !term.IsTerminal(int(f.Fd())) {
		var err error
		if f, err = os.Open("/dev/tty"); err != nil {
			return nil, err
		}
		defer f.Close()
	}
	fmt.Fprintf(out, "%v: ", prompt)
	b, err := term.ReadPassword(int(f.Fd()))
	if err == nil {
		fmt.Fprintf(out, "\n")
	}
	return b, err
}

// confirm asks a yes/no question on out and reports whether the answer read
// from stdin is y or Y. Everything else, including EOF, means no.
func confirm(out io.Writer, prompt string) bool {
	fmt.Fprintf(out, "%v", prompt)
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return false
	}
	answer := strings.TrimSpace(scanner.Text())
	return answer == "y" || answer == "Y"
}

// ANSI styles (SGR parameters) for the dialogue.
const (
	headingStyle = "1;36" // bold cyan
	valueStyle   = "1;33" // bold yellow
)

// colourEnv reports whether the environment allows colours: NO_COLOR
// (no-color.org) unset, and TERM neither empty nor "dumb".
func colourEnv() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	t := os.Getenv("TERM")
	return t != "" && t != "dumb"
}

// colour reports whether text written to w may carry colours: w is a terminal
// and the environment allows them. A pipe or a file, such as a tee'd -debug
// log, gets plain text.
func colour(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && colourEnv() && term.IsTerminal(int(f.Fd()))
}

// copyToClipboard asks the terminal tty to put s on the clipboard, with the
// OSC 52 escape sequence, and reports whether it was sent. Terminals that
// don't support it ignore it; over SSH it reaches the local terminal. Only a
// character device gets it, never a pipe or a file, and not TERM=dumb. Stat,
// not Fd: Fd would put tty in blocking mode and break its read deadline.
func copyToClipboard(tty *os.File, s string) bool {
	if tty == nil {
		return false
	}
	fi, err := tty.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if t := os.Getenv("TERM"); t == "" || t == "dumb" {
		return false
	}
	_, err = fmt.Fprintf(tty, "\x1b]52;c;%v\a", base64.StdEncoding.EncodeToString([]byte(s)))
	return err == nil
}

// paint wraps s in the ANSI style when on.
func paint(on bool, style, s string) string {
	if !on {
		return s
	}
	return "\x1b[" + style + "m" + s + "\x1b[0m"
}

// box frames the one-line ASCII text s, bold when on. The text stays on one
// line, so it copies without frame characters.
func box(on bool, s string) string {
	line := "+" + strings.Repeat("-", len(s)+2) + "+\n"
	return line + "| " + paint(on, valueStyle, s) + " |\n" + line
}
