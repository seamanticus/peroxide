# Accounts, credentials and keys

## Logging in: `peroxide auth`

`peroxide auth <username>` is the only time peroxide sees your Proton
password interactively. Step by step (internal/login/login.go, `Run`;
internal/protonmail/auth.go):

1. **Password.** Read from the terminal without echo. If stdin isn't a
   terminal, peroxide opens `/dev/tty` instead.
2. **SRP login.** Proton never receives the password. peroxide asks
   `/auth/info` for the SRP parameters and salt, computes an SRP proof
   from the password (internal/protonmail/srp.go) and sends it to `/auth`. Proton
   answers with a session: a UID, an access token, a refresh token, the
   granted scope and whether 2FA is on. A cookie jar keeps the session
   cookie Proton sets on `/auth/info`; without it Proton treats every
   request as a new, suspicious client.
3. **Human verification.** If Proton answers code 9001, it wants a
   CAPTCHA solved first (see below). peroxide replays the login once
   with the challenge token; a second challenge, or Proton refusing the
   token, is an error.
4. **Two-factor.** With TOTP on, peroxide reads a 6-digit code from
   standard input (your terminal echoes it) and sends it to
   `/auth/2fa`. Security keys are not supported.
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
password. peroxide (internal/auth/manager.go, `Manager.Auth`):

1. Base64-decodes the password, taking only the form `peroxide auth`
   prints: 44 characters of standard padded base64 encoding 32 bytes,
   with no line breaks and the two spare bits of the last character
   zero, so one key has exactly one password. Anything else is refused
   with "Invalid username or password".
2. **If this process already has a session for the username**, it
   first checks that `auth.json` still holds the record the session was
   read from (one file read and a SHA-256, no decryption), and compares
   the key with the session's **verifier**, kept in memory in constant
   time. If the record is unchanged, a matching key reuses the session
   and the unlocked keys, with no request to Proton. If the record is
   gone (`peroxide auth -remove`) or replaced (`peroxide auth` again),
   a login with the old or the new bridge password ends the session
   ([below](#when-the-stored-credentials-change)); the new one then goes
   on as in step 3. An `auth.json` that can't be read is logged and
   answered "Invalid username or password": the client, not yet
   authenticated, learns nothing about the file.
3. **Otherwise** it decrypts the username's `auth.json` record with the
   key (a wrong key fails the Poly1305 check: "Invalid username or
   password"), refreshes the Proton session with the refresh token last
   saved (`session.json`'s, else the record's own), unlocks the keys
   with the stored mailbox password, saves the new tokens in
   `session.json` (only while `auth.json` still holds that record;
   otherwise the login fails; if the file can't be written, the log
   warns and the login goes on, as the refresh has spent the token
   saved before), and keeps the session, the keys and the verifier in
   memory. A `session.json` that can't be read is logged as a warning,
   and the record's own token is tried. If the refresh token is spent
   (code 10013, after a password change or a remote logout), it logs in
   again from scratch with the stored login password; that fails if
   2FA is on or Proton asks for a CAPTCHA, as neither can be answered
   from here: run `peroxide auth` again. Until you do, every login of
   the account is refused at once with that message and no request to
   Proton, so a client that keeps retrying doesn't send the password
   again each time.

The passwords are read from `auth.json` only inside such a login, with
the bridge password the client sent, and are not kept after it; nor is
the bridge password. What stays in memory is the unlocked keys, the
session's tokens, the store key, a **session key**, which opens only the
account's `session.json` record, and the **verifier**, which opens
nothing. Both are derived one-way from the bridge password (HKDF-SHA256,
salted with the username, each under its own label), so neither gives
back the bridge password, which opens `auth.json`.

The verifier is a fast hash on purpose. A slow password hash (bcrypt,
scrypt, Argon2) only slows down guessing a password from a stolen hash;
the bridge password is 32 random bytes, which no guessing can find at
any speed (NIST SP 800-63B asks for an approved one-way function, not a
slow one, for random secrets of 112 bits or more). A guess is also
tested by one decryption of `auth.json`, which must stay fast for
logins.

So the first client login of a process costs a few requests and a
bcrypt derivation per key salt (Proton's scheme for the mailbox
password); later ones cost microseconds.
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
logout), peroxide first tries the token `session.json` holds now, in
case another peroxide process spent this one. If that fails too, the
session is **dead**: peroxide has no password to log in again with.
Requests fail with "the Proton session has ended", the account's IMAP
clients get `* BYE Your Proton session ended: please log in again to
open it` and are disconnected, and the next client login (aerc
reconnects by itself; every SMTP send logs in) opens the session again
as in step 3 above.

Each refresh saves the new tokens in `session.json`, sealed with the
session key, and only while `auth.json` still holds the record the
session came from; `auth.json` itself is written by `peroxide auth`
alone. If `session.json` can't be written at
all (disk full), the log says so: the stored refresh token is then
spent, and the next start logs in with the password, which Proton may
answer with a CAPTCHA. Every write of `auth.json` and `session.json` is
synced to disk with its directory, so a crash can't bring back a file
whose refresh token is spent.

Every Proton request has 30 seconds to connect, 30 for the TLS
handshake and 30 for its answer to start; a request over them fails
rather than hold the account's logins and
refreshes. Downloads aren't cut off once their answer has started.

### When the stored credentials change

After `peroxide auth -remove` or a new `peroxide auth` of the account
while `serve` runs, the session read from the old record ends at the
account's next login with either bridge password, or at its next
refresh (about daily), whichever comes first. Until then the clients
already connected keep working: log in once with the new password to
cut them off at once. A login with neither password is refused and
ends nothing, so no client can disconnect another without a password.

Ending the session:

- logs `The stored credentials of <user> were removed or replaced:
  ending the session read from them`;
- logs it out at Proton (`DELETE /auth`; a refresh ending it logs out
  the tokens it just got), so copied tokens stop working too. If that
  request fails, the log says so, and the session stays valid at Proton
  until it expires: revoke it in Proton's web settings, "Session
  management";
- drops its tokens and keys, disconnects its IMAP clients with `* BYE
  Your Proton session ended: please log in again to open it`, and fails
  the next send of an SMTP connection logged in before, so the client
  logs in again.

The old bridge password is refused from then on, and the new one opens
a new session.

## Keys that change while peroxide runs

The private keys are unlocked at the first login of a process and kept.
A key Proton adds later (a new address, a key rotation) arrives through
Proton's `Addresses` events: peroxide updates the address list (drafts
and APPEND take their sender from it), and when the address has an
active key it hasn't unlocked:

1. It logs `Key <fingerprint> of address <...> is new: it is unlocked
   at the next login` and marks the account's session stale. It unlocks
   nothing itself: that takes the mailbox password, which the next
   login reads from `auth.json` with the bridge password the client
   sends.
2. It disconnects the account's IMAP clients with `* BYE Your Proton
   account has a new key: please log in again to unlock it`. A client
   that reconnects by itself (aerc does, at once) logs in again; in
   others, reconnect.
3. That login, or an SMTP login (every send makes one) if it comes
   first, unlocks all the account's keys again. The IMAP login waits
   for the old session to end and starts a new one, which builds the
   messages stored still encrypted again with the new key, under new
   UIDs ([imap/messages.md](imap/messages.md#messages-that-change)).

So a new key works within one events poll and a reconnect, without a
restart. A key that still can't be unlocked (the log says why)
disconnects once, not at every event. A change of the account's user
key isn't seen until peroxide restarts.

## Files on disk

peroxide keeps its files in `peroxide/` under Go's
`os.UserConfigDir()`: `$XDG_CONFIG_HOME`, else `~/.config`. The
directory is created with mode 0700, and set to 0700 if it exists with
a wider mode; so is the data directory below. One closed to other users
already is left as it is; one open to them that can't be closed (not
yours, a read-only mount) stops peroxide with an error.

| File | What it holds | Protection |
|---|---|---|
| `auth.json` | A JSON object: username -> base64 sealed record (above): the login and mailbox passwords, key salts, and the session of when `peroxide auth` ran | Usernames in clear; records sealed with the bridge password's key. Mode 0600. Written by `peroxide auth` only. Every write goes to a temporary file renamed over it, so it is never half written, and a file an older peroxide created 0644 is replaced 0600 the next time `auth` saves. Each change reads, edits and rewrites the file holding `auth.lock`, so two at once, in one process or two, never lose each other's account |
| `session.json` | A JSON object: username -> base64 sealed Proton session (its tokens), replaced at every refresh | Usernames in clear; records sealed with the session key, derived from the bridge password. Mode 0600, written like `auth.json`, under `auth.lock`. `auth` and `auth -remove` drop the account's record. Deleting the file is harmless: the next start logs in with the password |
| `auth.lock` | Nothing: held while `auth.json` or `session.json` is changed | Released by the kernel when the change is saved or the process ends |
| `serve.lock` | Nothing: `peroxide serve` holds a lock on it while it runs; `sendmail` and `download-mailbox` check it with a shared lock, so checks never mistake each other for `serve` | Released by the kernel when `serve` ends |
| `<username>.db` | The IMAP database: message metadata, mailbox entries, counts, the last event ID, the mailboxes unsubscribed from (see [imap/sync.md](imap/sync.md)) | Not encrypted. Mode 0700 |

`<username>.db` holds, for every message peroxide has listed: subject,
sender and recipient addresses and names, date, size, flags, label IDs,
attachment names and sizes, and Proton's message ID, in clear; and,
sealed under the store key, the structure and envelope of each message
stored. **No keys.** The metadata is everything a mailbox listing
shows, readable by anyone who can read the file.

The records of `auth.json` and `session.json` are padded with spaces
(at least one) up to the next multiple of 512 bytes before they are
sealed, so their length doesn't
tell how long the passwords or tokens in them are. Records an older
peroxide wrote have no padding and are still read.

The messages themselves are in the data directory, `peroxide/<username>/`
under `$XDG_DATA_HOME` (else `~/.local/share`), one sealed file each:
[security.md](security.md#the-message-store). Delete `<username>.db`
(in the configuration directory above) and that directory at will while
peroxide is stopped: both are rebuilt from
Proton (deleting only some files of the directory makes those messages
download again).

`peroxide auth -remove <username>` deletes the `auth.json` record and
the `session.json` one. It does not delete `<username>.db` or the
stored messages. Neither it nor a new `peroxide auth` logs the old
session out at Proton by itself: a `serve` running meanwhile does when
it ends it ([above](#when-the-stored-credentials-change)); otherwise it
stays valid at Proton until it expires (revoke it in Proton's web
settings, "Session management"), as peroxide no longer holds its tokens.

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
   `WAYLAND_DISPLAY` set). Only the CAPTCHA is offered. In both modes
   it also asks the terminal to copy the URL to the clipboard (the
   OSC 52 escape sequence, written to `/dev/tty`; over SSH it reaches
   your local terminal's clipboard). Terminals without OSC 52 ignore
   it; inside tmux it needs `set -g set-clipboard on`.
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

<!-- nav -->

---

[Previous: Commands and options](commands.md) | [Contents](README.md) | [Next: The IMAP server](imap/README.md)
