# Accounts, credentials and keys

## Logging in: `peroxide auth`

`peroxide auth <username>` is the only time peroxide sees your Proton
password interactively. Step by step (internal/login/login.go, `Run`;
internal/protonmail/auth.go):

1. **Password.** Read from the terminal without echo. If stdin is a
   pipe, peroxide opens `/dev/tty` instead.
2. **SRP login.** Proton never receives the password. peroxide asks
   `/auth/info` for the SRP parameters and salt, computes an SRP proof
   from the password (internal/protonmail/srp.go) and sends it to `/auth`. Proton
   answers with a session: a UID, an access token, a refresh token, the
   granted scope and whether 2FA is on. A cookie jar keeps the session
   cookie Proton sets on `/auth/info`; without it Proton treats every
   request as a new, suspicious client.
3. **Human verification.** If Proton answers code 9001, it wants a
   CAPTCHA solved first (see below). peroxide replays the login once
   with the challenge token; a second challenge is an error.
4. **Two-factor.** With TOTP on, peroxide reads a 6-digit code and sends
   it to `/auth/2fa`. Security keys are not supported.
5. **Mailbox password.** Proton accounts have one password, or (older
   "two-password mode") a login password and a separate mailbox
   password that unlocks the keys. peroxide asks for the second only in
   two-password mode.
6. **Unlocking the keys** (to check the password before storing it):
   peroxide fetches the key salts (`/keys/salts`), derives each
   key's passphrase from the mailbox password with bcrypt and the salt,
   unlocks the user keys, then each address key (whose passphrase is a
   token encrypted to, and signed by, the user key). An address whose
   keys fail is logged and skipped; if no key at all unlocks, `auth`
   fails.
7. **The bridge password.** peroxide draws 32 random bytes
   (`crypto/rand`). Base64 of them, 44 characters, is the bridge
   password it shows, boxed, on the terminal (`/dev/tty`) only. The
   same 32 bytes are the secret key that encrypts your credentials.
8. **Storing.** It writes to `auth.json`, under your username, the
   credentials sealed with NaCl `secretbox` (XSalsa20-Poly1305, random
   24-byte nonce) under that key. The sealed record holds: the Proton
   session (UID, access and refresh tokens, scope), **your login
   password**, **your mailbox password**, and the key salts.

The bridge password is shown and forgotten. It is the only key to the
record: peroxide can't decrypt `auth.json` without it, so it can't
start a session for you until a client gives it.

## Logging in from a client

Your mail client logs in to peroxide's IMAP (`LOGIN`, or `AUTHENTICATE
PLAIN`) or SMTP (`AUTH PLAIN`) with your Proton username and the bridge
password. peroxide (internal/auth/auth.go, `Manager.Auth`):

1. Base64-decodes the password. Anything that isn't exactly 32 bytes is
   refused with "Invalid username or password".
2. **If this process already has a session for the username**, it
   compares the key with the bcrypt hash it kept, and reuses the
   session and the unlocked keys. No request to Proton.
3. **Otherwise** it decrypts the username's `auth.json` record with the
   key (a wrong key fails the Poly1305 check: "Invalid username or
   password"), refreshes the Proton session with the stored refresh
   token, unlocks the keys with the stored mailbox password, writes the
   record back with the new refresh token (only if it is still there
   and still opens with this key; otherwise the login fails), and keeps
   the session, the keys and a bcrypt hash of the key in memory.

So the first client login of a process costs a few requests and a
bcrypt derivation per key salt; later ones cost one bcrypt comparison.
IMAP and SMTP in one `serve` process share the session.

The IMAP username is the one you gave `peroxide auth`, which is the one
Proton knows: `name` or `name@proton.me`, as you typed it.

## Session refresh

Proton's access token expires. When a request answers HTTP 401,
peroxide runs `ReAuth` (internal/protonmail/protonmail.go, `do`): it refreshes
with the refresh token and retries the request once; a 401 to the
retry is the request's answer. One refresh of an account runs at a
time, and requests meeting the expired token meanwhile wait for it,
then are sent again. If Proton refuses
the refresh token (code 10013, after a password change or a remote
logout), peroxide first tries the token `auth.json` holds now, in case
another peroxide process spent this one, then logs in again from
scratch with the stored login
password, and unlocks the keys again. That fails if 2FA is on, as no
code can be typed: run `peroxide auth` again.

Each refresh rewrites the `auth.json` record, sealed with the same key,
but only over the record the session came from. After `peroxide auth
-remove` or a new `peroxide auth` of the account while `serve` runs,
`serve` stops saving its session (logged as `Not saving the renewed
session of <user>: the stored credentials were removed or replaced`),
so the change sticks; `serve` keeps using the session it has until it
exits. If the record can't be written at all (disk full), the log says
so: the stored refresh token is then spent, and the next start logs in
with the password, which Proton may answer with a CAPTCHA.

Every Proton request has 30 seconds to connect, 30 for the TLS
handshake and 30 for its answer to start, as in proton-bridge; a
request over them fails rather than hold the account's logins and
refreshes. Downloads aren't cut off once their answer has started.

## Keys that change while peroxide runs

The private keys are unlocked once per process, at the first login. A
key Proton adds later (a new address, a key rotation) arrives through
Proton's `Addresses` events: peroxide updates the address list (drafts
and APPEND take their sender from it) and logs `Key <fingerprint> of
address <...> is new: it is unlocked when peroxide restarts`. Mail
encrypted to that key can't be decrypted, and the SMTP server can't
send from that address, until peroxide restarts. The SMTP session's
address list isn't refreshed at all: a new address can send only after
the SMTP client logs in again in a new process. `TODO.md` section 7.
After the restart, messages stored still encrypted are built again with
the new key and get new UIDs
([imap/messages.md](imap/messages.md#messages-that-change)).

## Files on disk

peroxide keeps its files in `peroxide/` under Go's
`os.UserConfigDir()`: `$XDG_CONFIG_HOME`, else `~/.config`. The
directory is created with mode 0700.

| File | What it holds | Protection |
|---|---|---|
| `auth.json` | A JSON object: username -> base64 sealed record (above) | Usernames in clear; records sealed with the bridge password's key. Mode 0600. Every write goes to a temporary file renamed over it, so it is never half written, and a file an older peroxide created 0644 is replaced 0600 the next time credentials are saved (`auth`, or any session refresh). Each change reads, edits and rewrites the file holding `auth.lock`, so two at once, in one process or two, never lose each other's account |
| `auth.lock` | Nothing: held while `auth.json` is changed | Released by the kernel when the change is saved or the process ends |
| `serve.lock` | Nothing: `peroxide serve` holds a lock on it while it runs; `sendmail` and `download-mailbox` check it with a shared lock, so checks never mistake each other for `serve` | Released by the kernel when `serve` ends |
| `<username>.db` | The IMAP database: message metadata, mailbox entries, counts, the last event ID, the mailboxes unsubscribed from (see [imap/sync.md](imap/sync.md)) | Not encrypted. Mode 0700 |

`<username>.db` holds, for every message peroxide has listed: subject,
sender and recipient addresses and names, date, size, flags, label IDs,
attachment names and sizes, and Proton's message ID, in clear; and,
sealed under the store key, the structure and envelope of each message
stored. **No keys.** The metadata is everything a mailbox listing
shows, readable by anyone who can read the file.

The messages themselves are in the data directory, `peroxide/<username>/`
under `$XDG_DATA_HOME` (else `~/.local/share`), one sealed file each:
[security.md](security.md#the-message-store). Delete `<username>.db` and
that directory at will while peroxide is stopped: both are rebuilt from
Proton (deleting only some files of the directory makes those messages
download again).

`peroxide auth -remove <username>` deletes the `auth.json` record. It
does not delete `<username>.db` or the stored messages, and it does not
log out the Proton session on Proton's side (revoke it in Proton's web
settings, "Session management").

## What the stored credentials mean

The `auth.json` record holds your Proton login password and mailbox
password, sealed under a key that is the bridge password. Whoever has
both the file and the bridge password has your Proton password. The
bridge password must therefore be kept like the Proton password itself:

- Don't store it in a client's configuration in plain text. aerc,
  mutt and others can run a command to get it (`pass`, `gpg -d`); see
  the README.
- A client that saves it in plain text, together with read access to
  `~/.config/peroxide/auth.json`, gives your Proton password away.

The login password is stored so that peroxide can log in again when
Proton revokes the refresh token, without asking you. That is a
convenience peroxide inherited from hydroxide.

## Human verification (CAPTCHA)

Proton sometimes refuses a login until a human solves a challenge (API
code 9001, with a challenge token). peroxide does not solve it; you do,
in your browser, on Proton's own page:

1. peroxide prints
   `https://verify.proton.me/?methods=captcha&token=<challenge token>`
   and, with `-captcha-mode browser` (the default), opens it with
   `xdg-open` inside a graphical session (`DISPLAY` or
   `WAYLAND_DISPLAY` set). Only the CAPTCHA is offered.
2. You solve the CAPTCHA there. Proton's page itself hands the result
   to Proton (`POST /core/v4/verification/captcha/<challenge token>`),
   which marks the challenge token as solved. Nothing has to be copied
   back.
3. You press Enter in the terminal. peroxide reads it from `/dev/tty`,
   waiting up to `-captcha-timeout` (default 10 minutes); Ctrl+C or a
   closed terminal cancels without retrying.
4. peroxide replays the login once, with the challenge token in the
   `x-pm-human-verification-token` header and `captcha` as its type.
   If Proton still refuses (the page wasn't solved, or the token has
   expired or was already used), `auth` reports it; run it again for a
   new challenge.

peroxide can't tell when the page is solved, so it waits for Enter:
every replay is a full login, and the token is single use.

The URL carries the challenge token, so it shows up in the browser's
history. The token belongs to this one login attempt and is good for
nothing else.

On a headless machine use `-captcha-mode manual` and open the URL in a
browser on any device: the page talks to Proton directly. With
`-captcha-mode disabled` peroxide reports Proton's error instead.

peroxide can't answer a challenge when it refreshes a session in the
background (no one to solve it): the login fails, and the log tells you
to run `peroxide auth` again.

## How proton-bridge does it

- **Storage.** proton-bridge keeps one vault file (`vault.enc`,
  internal/vault), sealed with AES-256-GCM under a key held in the
  system keychain (Secret Service, pass, macOS Keychain, Windows
  Credential Manager). Per account it stores the session UID and
  refresh token, a *key passphrase* already derived from the mailbox
  password (`KeyPass`), and the bridge password itself (16 random
  bytes). **It does not store your login password.** If Proton revokes
  the refresh token, proton-bridge signs the account out and asks you
  to log in again.
- **Bridge password.** Stored in the vault, so its GUI and CLI can show
  it at any time. peroxide doesn't store it: shown once.
- **Why peroxide differs.** No keychain dependency: peroxide runs the
  same on a headless server, in a container, or on a system without a
  Secret Service, with nothing but its config directory. The cost is
  that the bridge password is the only secret, and that it unlocks the
  stored login password. Storing a derived key passphrase instead of
  the mailbox password, and dropping the stored login password, would
  bring peroxide to proton-bridge's exposure; neither is done yet.
- **Human verification.** The same flow: its CLI prints
  `https://verify.proton.me/?methods=<every offered method>&token=...`
  and waits for Enter (internal/frontend/cli, `promptHvURL`); its GUI
  shows the link. It then replays the login with the original token,
  with every offered method as the token type. peroxide offers only the
  CAPTCHA, the one method whose proof the page sends to Proton by
  itself.
