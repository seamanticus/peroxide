package main

import (
	"errors"
	"flag"
	"regexp"
	rtdebug "runtime/debug"
	"slices"
	"strings"
	"testing"
)

// parseArgs: global options before or after the command, a command's own
// only after it, sendmail's -i and recipients after --; every wrong line an
// error naming the fix, never options dropped.
// evidence: witnesses
func TestParseArgs(t *testing.T) {
	for _, tc := range []struct {
		args      []string
		wantErrIn string // "" for success; "help" for errHelp
		check     func(*invocation) bool
	}{
		{[]string{"serve"}, "", func(i *invocation) bool {
			return i.command == "serve" && !i.serve.disableSMTP && i.serve.showAllMail && i.serve.smtpPort == "1025"
		}},
		{[]string{"serve", "-disable-smtp", "-imap-port", "2143"}, "", func(i *invocation) bool {
			return i.serve.disableSMTP && i.serve.imapPort == "2143"
		}},
		{[]string{"-debug", "serve"}, "", func(i *invocation) bool { return i.global.debug }},
		{[]string{"serve", "-debug"}, "", func(i *invocation) bool { return i.global.debug }},
		{[]string{"serve", "-show-all-mail=false"}, "", func(i *invocation) bool { return !i.serve.showAllMail }},
		{[]string{"auth", "-captcha-mode", "manual", "bob"}, "", func(i *invocation) bool {
			return i.auth.captchaMode == "manual" && slices.Equal(i.args, []string{"bob"})
		}},
		{[]string{"download-mailbox", "-discard-local-copy", "bob"}, "", func(i *invocation) bool {
			return i.discard && slices.Equal(i.args, []string{"bob"})
		}},
		{[]string{"sendmail", "-smtp-port", "2025", "bob", "--", "-i", "a@x.org", "b@x.org"}, "", func(i *invocation) bool {
			return i.sendmail.smtpPort == "2025" && slices.Equal(i.args, []string{"bob"}) &&
				slices.Equal(i.sendmail.recipients, []string{"a@x.org", "b@x.org"})
		}},
		{[]string{"-V"}, "", func(i *invocation) bool { return i.global.version }},
		{[]string{"serve", "-V"}, "", func(i *invocation) bool { return i.global.version }},

		// The silent drop that made `serve -disable-smtp` run SMTP.
		{[]string{"-disable-smtp", "serve"}, "is an option of serve: write it after the command", nil},
		{[]string{"-discard-local-copy", "download-mailbox", "bob"}, "option of download-mailbox", nil},
		{[]string{"serve", "-nope"}, "peroxide serve: unknown option -nope", nil},
		{[]string{"-nope", "serve"}, "unknown option -nope", nil},
		{[]string{"serve", "extra"}, "takes no arguments", nil},
		{[]string{"serve", "-disable-imap", "x", "-disable-smtp"}, "takes no arguments", nil},
		{[]string{"auth"}, "usage: peroxide auth", nil},
		{[]string{"download-mailbox", "a", "b"}, "usage: peroxide download-mailbox", nil},
		{[]string{"sendmail", "bob", "a@x.org"}, "usage: peroxide sendmail", nil},
		{[]string{"sendmail", "bob", "--", "-t", "a@x.org"}, "only -i is understood", nil},
		{[]string{"imap"}, `unknown command "imap"`, nil},
		{[]string{"serve", "-imap-port"}, "peroxide serve:", nil},

		{nil, "help", nil},
		{[]string{"help"}, "help", nil},
		{[]string{"-h"}, "help", nil},
		{[]string{"serve", "-h"}, "help", nil},
	} {
		inv, err := parseArgs(tc.args)
		switch {
		case tc.wantErrIn == "help":
			if !errors.Is(err, errHelp) {
				t.Errorf("parseArgs(%q) = %v, want the help page", tc.args, err)
			}
		case tc.wantErrIn != "":
			if err == nil || errors.Is(err, errHelp) || !strings.Contains(err.Error(), tc.wantErrIn) {
				t.Errorf("parseArgs(%q) = %v, want an error with %q", tc.args, err, tc.wantErrIn)
			}
		case err != nil:
			t.Errorf("parseArgs(%q) = %v", tc.args, err)
		case !tc.check(inv):
			t.Errorf("parseArgs(%q) = %+v", tc.args, inv)
		}
	}
}

// The help page names every option every command defines, the global ones
// too, and every command: it can't drift from parseArgs.
// proof: exhaustive (every command's flag set)
func TestUsageListsEveryOption(t *testing.T) {
	for _, c := range commands {
		if !strings.Contains(usage, "\n  "+c) {
			t.Errorf("usage doesn't list the command %v", c)
		}
		fs, ok := commandFlags(c, &invocation{})
		if !ok {
			t.Fatalf("commandFlags(%v) unknown", c)
		}
		fs.VisitAll(func(f *flag.Flag) {
			if !regexp.MustCompile(`(^|[\s,])-` + regexp.QuoteMeta(f.Name) + `\b`).MatchString(usage) {
				t.Errorf("usage doesn't list -%v of %v", f.Name, c)
			}
		})
	}
	for line := range strings.SplitSeq(usage, "\n") {
		if len(line) > 80 {
			t.Errorf("usage line longer than 80 columns: %q", line)
		}
	}
}

// -V prints the commit, its date in UTC, "no (HEAD)" or "yes (dirty)" with a
// warning, the toolchain and the commit's URL; with no commit recorded, the
// module version; with no build information, says so.
// evidence: witnesses
func TestVersionText(t *testing.T) {
	info := func(modified string) *rtdebug.BuildInfo {
		return &rtdebug.BuildInfo{GoVersion: "go1.27.3", Main: rtdebug.Module{Version: "(devel)"}, Settings: []rtdebug.BuildSetting{
			{Key: "GOOS", Value: "linux"}, {Key: "GOARCH", Value: "amd64"},
			{Key: "vcs.revision", Value: "abc123"}, {Key: "vcs.time", Value: "2026-11-10T10:41:00+13:00"},
			{Key: "vcs.modified", Value: modified},
		}}
	}
	clean := versionText(info("false"), true)
	for _, want := range []string{"commit   : abc123\n", "date     : 2026-11-09 21:41 UTC\n", "modified : no (HEAD)\n",
		"go       : go1.27.3 linux/amd64\n", "url      : <https://github.com/seamanticus/peroxide/commit/abc123>\n"} {
		if !strings.Contains(clean, want) {
			t.Errorf("versionText(clean) = %q, want %q in it", clean, want)
		}
	}
	if strings.Contains(clean, "warning") {
		t.Errorf("versionText(clean) warns: %q", clean)
	}
	dirty := versionText(info("true"), true)
	if !strings.Contains(dirty, "modified : yes (dirty)\n") || !strings.Contains(dirty, "warning: built with uncommitted changes") {
		t.Errorf("versionText(dirty) = %q", dirty)
	}
	noVCS := versionText(&rtdebug.BuildInfo{GoVersion: "go1.27.3", Main: rtdebug.Module{Version: "v1.2.3"}}, true)
	if !strings.Contains(noVCS, "version  : v1.2.3") || strings.Contains(noVCS, "url") {
		t.Errorf("versionText(no vcs) = %q", noVCS)
	}
	if got := versionText(nil, false); !strings.Contains(got, "unknown") {
		t.Errorf("versionText(none) = %q", got)
	}
}
