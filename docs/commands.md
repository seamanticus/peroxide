# Commands and options

    peroxide [-debug] <command> [options] [arguments]
    peroxide -V

A command's options come **after** the command: `peroxide serve
-disable-smtp`. `-debug`, `-V`, `-api-endpoint` and `-app-version` are
accepted before the command or after it. Options are parsed by Go's
`flag` package: `-flag value` or `-flag=value` for options taking a
value, `-flag` for on/off ones, and `--flag` works as `-flag`.

Nothing is ignored. A command's option written before the command
(`peroxide -disable-smtp serve`) is refused with the line to type
instead; so are an unknown option, an unknown command and arguments a
command doesn't take. These exit with status 2. `peroxide help`,
`peroxide`, or `-h` after any command, print the help: one page, every
command and option.

Every other error ends the program with a message on stderr (stdout
with `-debug`) and status 1.

## Commands

### `auth [options...] <username>`

Logs in to Proton interactively and stores the credentials. See
[accounts.md](accounts.md) for every step. It asks for:

1. Your Proton password (no echo).
2. Proton's human verification, if Proton asks for it (the CAPTCHA
   helper, see [accounts.md](accounts.md#human-verification-captcha)).
3. A TOTP code, if two-factor authentication is on. Only TOTP is
   supported; a security key (FIDO2/U2F) alone fails with "Only TOTP is
   supported as a 2FA method". The code is echoed by the terminal and
   never logged.
4. Your mailbox password, if the account uses Proton's two-password
   mode.

It then prints `Bridge password: <44 characters>`. Write it down or put
it in your password manager: peroxide doesn't keep it, and without it
the stored credentials can't be decrypted. Lost, run `auth` again.

If credentials for that username already exist, it asks before
replacing them. `auth` options:

| Option | Default | Meaning |
|---|---|---|
| `-remove` | | Delete the stored credentials for the username and exit |
| `-captcha-mode` | `browser` | `browser` starts the helper and opens a browser on it; `manual` only prints its URL; `disabled` reports Proton's error instead |
| `-captcha-listen` | `127.0.0.1:8765` | Address of the helper server. Keep it on loopback |
| `-captcha-timeout` | `10m` | How long to wait for the challenge |
| `-no-open-browser` | | Print the URL without launching a browser |
| `-captcha-endpoint` | `https://mail-api.proton.me` | API host serving the challenge |

### `serve`

Runs the SMTP and the IMAP servers in one process, sharing one set of
sessions. If either server stops with an error, the process exits. Use
`-disable-smtp` or `-disable-imap` to run only one.

`serve` is the only command that runs a server: run one per account set.
Proton's refresh tokens are single use, so two peroxide processes
holding sessions for the same account would each refresh it, and the
slower one, its token spent, is logged out until `peroxide auth` is run
again. (Older peroxide had separate `imap` and `smtp` commands, which
did exactly that when run side by side.) `serve` holds a lock,
`serve.lock` in the configuration directory, while it runs: a second
`serve` stops with "peroxide serve is already running", `sendmail`
sends through it, and `download-mailbox` refuses to run. Clients
logging in to one account at once share one login, and requests that
meet an expired session renew it once between them.

### `status`

Lists the usernames that have stored credentials. It reads only the
names in `auth.json` (they are not encrypted) and makes no request.

### `download-mailbox [-discard-local-copy] <username>`

Downloads the account's mail into peroxide's local copy now and exits
once it is complete: the same download `serve` runs in the background
after a client logs in (see [imap/sync.md](imap/sync.md#the-download)),
metadata then messages, newest first. Unlike the background download,
it also tries again the messages that failed before. Use it before the
first client connects to a large account, so the client finds every
mailbox full and every message stored. It asks for the bridge password
or reads `PEROXIDE_BRIDGE_PASS`, as `sendmail` does.

Interrupted (Ctrl-C, SIGTERM) or failed (a message that can't be
downloaded, logged), it exits with status 1, and the next run, or the
next login, resumes where it stopped. A local copy already complete
makes it exit at once. Events that arrived meanwhile are applied at
the next login.

`-discard-local-copy` first wipes the local copy, then downloads the
account from scratch: for a copy that went wrong. Every mailbox gets a
new UIDVALIDITY, so clients drop their caches and fetch again. Nothing
in Proton changes. It is what peroxide does by itself on a Refresh
event or an event it can't apply (see
[imap/sync.md](imap/sync.md#rebuilding-the-database)).

While `serve` runs, the command refuses at once, before logging in:
`serve` downloads the account itself once a client logs in, and logging
in here would spend the session `serve` holds. Stop `serve` first.

### `sendmail <username> -- <recipients...>`

A `sendmail(1)` replacement for programs such as `git send-email`. It
reads one message (headers and body) from stdin and sends it, the same
way the SMTP server does (see [smtp.md](smtp.md)). The recipients on
the command line become Bcc recipients unless they already appear in
To or Cc, or the message has a Bcc header. It asks for the bridge
password or reads `PEROXIDE_BRIDGE_PASS`.

`-i` is accepted, because `git send-email` always passes it, and
changes nothing: peroxide reads stdin to its end whatever it contains,
so a line holding only `.` never ends
the message. No other sendmail option is understood. At least one
recipient is required, alone or through `serve`. `-h` after `--`
prints the help, as anywhere else.

While `serve` runs, `sendmail` hands the message to `serve`'s SMTP
server (`-smtp-host`, `-smtp-port`), logged in with the username and
bridge password, so that the account keeps the one session `serve`
holds; the envelope sender is the message's From. Give it the same
`-smtp-host`, `-smtp-port` and `-tls-cert` as `serve`, after the
command (`peroxide sendmail -tls-cert cert.pem <username> -- ...`):
with TLS it trusts only the certificate that file holds, byte for byte,
so a self-signed one
works; the file may hold the key too, as `serve` accepts. If `serve`
runs with `-disable-smtp` or `-tls-client-ca`, `sendmail` fails rather
than log in itself. If `serve` doesn't greet within 10 seconds (TLS on
one side only), it fails with that hint. A `QUIT` that fails after
`serve` accepted the message is a warning, not a failure. With no
`serve` running, it sends on its own, as `serve` would.

## Options

| Option | Commands | Default | Meaning |
|---|---|---|---|
| `-debug` | all | off | Log API traffic, IMAP and SMTP sessions to stdout. Read [debug.md](debug.md) first |
| `-V` | all | | Print the commit the binary was built from, and exit (below) |
| `-imap-host`, `-imap-port` | `serve` | `127.0.0.1`, `1143` | Where the IMAP server listens |
| `-smtp-host`, `-smtp-port` | `serve`, `sendmail` | `127.0.0.1`, `1025` | Where the SMTP server listens; for `sendmail`, where `serve`'s is |
| `-disable-imap`, `-disable-smtp` | `serve` | off | Don't start that server |
| `-show-all-mail` | `serve` | on | `=false` leaves All Mail out of IMAP LIST, as proton-bridge's "Show All Mail" off; see [imap/mailboxes.md](imap/mailboxes.md) |
| `-tls-cert`, `-tls-key` | `serve` (`sendmail`: `-tls-cert`) | none | Serve IMAP and SMTP over TLS with this certificate; `sendmail` trusts only that certificate |
| `-tls-client-ca` | `serve` | none | With TLS: require a client certificate signed by this CA |
| `-api-endpoint` | all | `https://mail.proton.me/api` | Proton's API base URL, for testing |
| `-app-version` | all | `Other` | The `x-pm-appversion` peroxide reports to Proton, for testing |

The options of `auth` and `download-mailbox` are under those commands.

### `-V`

Prints where the binary comes from, from what Go records in a binary
built in a git checkout (no build flags needed):

    this peroxide binary was compiled from:

    commit   : cab57103b0b089db065f1b29d2420f6b13ae14bf
    date     : 2026-11-09 20:13 UTC
    modified : no (HEAD)
    go       : go1.27.3 linux/amd64
    url      : <https://github.com/seamanticus/peroxide/commit/cab57103...>

`date` is the commit's, not the build's. `modified : yes (dirty)` means
the tree had uncommitted changes when it was built, which the commit
doesn't hold, and says so in a warning. A binary built outside a
checkout (`go install ...@version`) shows its module version instead.

`-V` with a command (`peroxide serve -V`) checks the whole line as if
it ran, then prints only the version: a line that would be refused
(`peroxide -V serve stray`) is refused, with status 2, and prints no
version.

### Listening and TLS

By default both servers listen on `127.0.0.1` without TLS, and accept
passwords in clear text (`AllowInsecureAuth`): the connection never
leaves your machine. Any other process on the machine can connect,
but it needs the bridge password to log in.

With `-tls-cert` and `-tls-key`, both servers use **implicit TLS**: the
connection is TLS from its first byte (as on ports 993 and 465), not
STARTTLS, on the same ports as without it. Configure the client for
"SSL/TLS", not "STARTTLS": in aerc, `imaps://` and `smtps://` rather
than `imap://` and `smtp://` (which mean STARTTLS there) or the
`+insecure` forms; in Thunderbird, "SSL/TLS". A self-signed certificate
must be trusted by the client. Plain
connections are then refused, and the servers advertise each protocol
by ALPN (`imap`, `smtp`, RFC 8314).

`-tls-client-ca` makes every client present a certificate signed by
that CA (`tls.RequireAndVerifyClientCert`).

Any of the three options asks for TLS, and `serve` never falls back to
plain text: it stops with the reason when `-tls-cert` comes without
`-tls-key` or the reverse, `-tls-client-ca` comes without both, the
pair doesn't load, or the CA file can't be read, holds no certificate,
or holds anything other than certificates.

Listening on another address (`-imap-host 0.0.0.0`) exposes the
servers to the network. Do it only with TLS, and preferably with a
client CA: the bridge password is then the only thing between the
network and your mail.

### `-app-version`

Proton expects an application version header, and accepts `Other`
today. If Proton ever starts refusing it (API code 5003, "app version
bad"), every request fails: pass a version Proton accepts, such as one
proton-bridge sends.

## Environment

- `PEROXIDE_BRIDGE_PASS`: the bridge password for `sendmail`, instead
  of a prompt. Environment variables can be read by other processes of
  the same user (`/proc/<pid>/environ`); prefer piping it from a
  password manager into the command's environment only.
- `XDG_CONFIG_HOME`: where `peroxide/` is created (default
  `~/.config`; on macOS `~/Library/Application Support`). See
  [accounts.md](accounts.md#files-on-disk).

## How proton-bridge does it

proton-bridge has a GUI and an interactive CLI (`--cli`) in which you
log in, list accounts and read the bridge password at any time, because
it keeps the bridge password in its vault. peroxide prints it once and
forgets it. proton-bridge generates its own self-signed certificate
(internal/certs) and offers, per server, STARTTLS or implicit TLS
(`IMAPSSL`, `SMTPSSL` in its vault settings); peroxide uses TLS only
with a certificate you give it, implicit only. proton-bridge takes the
first free ports from 1143 (IMAP) and 1025 (SMTP); peroxide uses those
numbers as fixed defaults and fails to start if they are taken.
