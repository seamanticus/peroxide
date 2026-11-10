package login

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestBox(t *testing.T) {
	const want = "+-----------+\n" +
		"| some text |\n" +
		"+-----------+\n"
	if got := box(false, "some text"); got != want {
		t.Errorf("box(false) =\n%v\nwant\n%v", got, want)
	}

	// Colour wraps the text only: the frame keeps its width.
	got := box(true, "some text")
	if want := "| \x1b[1msome text\x1b[0m |\n"; !strings.Contains(got, want) {
		t.Errorf("box(true) = %q, want a line %q", got, want)
	}
	if !strings.HasPrefix(got, "+-----------+\n") {
		t.Errorf("box(true) = %q, want the plain frame", got)
	}
}

func TestPaint(t *testing.T) {
	if got := paint(false, headingStyle, "x"); got != "x" {
		t.Errorf("paint(false) = %q, want %q", got, "x")
	}
	if got, want := paint(true, headingStyle, "x"), "\x1b[1;36mx\x1b[0m"; got != want {
		t.Errorf("paint(true) = %q, want %q", got, want)
	}
}

// TestColour checks that only a terminal in a colour-capable environment
// gets escape codes.
func TestColour(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")

	if colour(&bytes.Buffer{}) {
		t.Error("colour(buffer) = true")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if colour(w) {
		t.Error("colour(pipe) = true")
	}

	for _, env := range []struct{ term, noColor string }{
		{"dumb", ""},
		{"", ""},
		{"xterm", "1"},
	} {
		t.Setenv("TERM", env.term)
		t.Setenv("NO_COLOR", env.noColor)
		if colourEnv() {
			t.Errorf("colourEnv() = true with TERM=%q NO_COLOR=%q", env.term, env.noColor)
		}
	}
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "")
	if !colourEnv() {
		t.Error("colourEnv() = false with TERM=xterm and no NO_COLOR")
	}
}

func TestSolve_PlainOutputWithoutTerminal(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "")

	var out bytes.Buffer
	opts := enterOpts(t, &out, &browser{})
	if err := Solve(t.Context(), hvFor("captcha"), opts); err != nil {
		t.Fatalf("Solve() = %v", err)
	}
	if strings.Contains(out.String(), "\x1b[") {
		t.Errorf("escape codes written to a buffer:\n%q", out.String())
	}
	if !strings.Contains(out.String(), "| "+wantVerifyURL+" |\n") {
		t.Errorf("the verification URL is not boxed on one line:\n%v", out.String())
	}
}
