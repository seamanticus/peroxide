# Commands and flags

    peroxide [global options...] <command> [command options...] [arguments...]

Global options come **before** the command: `peroxide -debug serve`,
not `peroxide serve -debug`. They are parsed by Go's `flag` package:
`-flag value` or `-flag=value` for options taking a value, `-flag` for
on/off ones, and `--flag` works as `-flag`. `peroxide help`,
or any unknown command, prints the usage text; an unknown command then
exits with status 1.

Every error ends the program with a message on stderr (stdout with
`-debug`) and status 1.

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

### `imap`, `smtp`

Run one server alone. Two processes (`peroxide imap` and
`peroxide smtp`) each unlock your keys and hold their own Proton
session; `serve` does it once.

### `status`

Lists the usernames that have stored credentials. It reads only the
names in `auth.json` (they are not encrypted) and makes no request.

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
the message. No other sendmail option is understood.

## Global options

| Option | Default | Meaning |
|---|---|---|
| `-debug` | off | Log API traffic, IMAP and SMTP sessions to stdout. Read [debug.md](debug.md) first |
| `-imap-host`, `-imap-port` | `127.0.0.1`, `1143` | Where the IMAP server listens |
| `-smtp-host`, `-smtp-port` | `127.0.0.1`, `1025` | Where the SMTP server listens |
| `-disable-imap`, `-disable-smtp` | off | For `serve`: don't start that server |
| `-tls-cert`, `-tls-key` | none | Serve IMAP and SMTP over TLS with this certificate |
| `-tls-client-ca` | none | With TLS: require a client certificate signed by this CA |
| `-api-endpoint` | `https://mail.proton.me/api` | Proton's API base URL |
| `-app-version` | `Other` | The `x-pm-appversion` peroxide reports to Proton |

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
that CA (`tls.RequireAndVerifyClientCert`). It needs `-tls-cert` and
`-tls-key`.

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
