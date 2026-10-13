# peroxide

A third-party, open-source ProtonMail bridge. For power users only, designed to
run locally.

**Project scope**: peroxide is being reduced to a very simple and lightweight
bridge to ProtonMail that only supports SMTP and IMAP — nothing more. Keeping
the surface area small is the only way this bridge stays up to date.

Rationale:

* No GUI, only a CLI (so it runs in headless environments)
* Standard-compliant (we don't care about Microsoft Outlook)
* Fully open-source

## How does it work?

peroxide is a server that translates standard protocols (SMTP and IMAP)
into ProtonMail API requests. It allows you to use your preferred e-mail clients
and `git-send-email` with ProtonMail.

    +-----------------+             +-------------+  ProtonMail  +--------------+
    |                 | IMAP, SMTP  |             |     API      |              |
    |  E-mail client  <------------->  peroxide  <-------------->  ProtonMail  |
    |                 |             |             |              |              |
    +-----------------+             +-------------+              +--------------+

## Setup

### Installing

Start by installing peroxide:

```shell
git clone https://github.com/seamanticus/peroxide.git
cd peroxide
go build ./cmd/peroxide
```

Then you'll need to login to ProtonMail via peroxide, so that peroxide can
retrieve e-mails from ProtonMail. You can do so with this command:

```shell
peroxide auth <username>
```

Once you're logged in, a "bridge password" will be printed. Don't close your
terminal yet, as this password is not stored anywhere by peroxide and will be
needed when configuring your e-mail client.

Your ProtonMail credentials are stored on disk encrypted with this bridge
password (a 32-byte random password generated when logging in).

### Human verification

Proton often answers `peroxide auth` with a CAPTCHA challenge (API error 9001)
before it will accept a login. peroxide does not solve, bypass or weaken that
challenge — it is served, rendered and scored by Proton throughout. peroxide
only points you at it and carries its result back to the login request.

When a challenge comes up, `peroxide auth` starts a single-use helper server on
`127.0.0.1:8765`, opens your browser on it, and waits. The page walks you
through four steps:

1. Copy the one-line snippet it shows you.
2. Open Proton's challenge in a new tab, using the button on the page.
3. In that tab, open the developer console (<kbd>F12</kbd> → "Console") and
   paste the snippet. Firefox and Chrome make you type `allow pasting` first.
   Press <kbd>Enter</kbd> — it prints `undefined`, which is correct. Now solve
   the CAPTCHA; a text box appears at the top of the tab with the result already
   selected. Copy it.
4. Paste that result back into the peroxide page and press "Continue".

The console step is needed because Proton only lets its own web apps embed the
challenge, so it can't be shown inline and its result has to be moved by hand.

Once you confirm, authentication is replayed with the challenge result and the
login continues as usual. The helper server shuts down immediately afterwards.

#### Options

These apply to `peroxide auth` only:

| Flag | Default | Meaning |
| --- | --- | --- |
| `-captcha-mode` | `browser` | `browser` opens the helper page, `manual` only prints its URL, `disabled` reports the API error instead |
| `-captcha-listen` | `127.0.0.1:8765` | Address of the helper server. Keep it on loopback |
| `-captcha-timeout` | `10m` | How long to wait for you to finish |
| `-no-open-browser` | | Print the URL without trying to launch a browser |
| `-captcha-endpoint` | `https://mail-api.proton.me` | API host serving the challenge |

`-captcha-endpoint` is worth knowing about: the host must serve both
`/core/v4/captcha` and the widget's assets under `/captcha/v1/assets/`. Several
Proton hosts serve the first but not the second, and the challenge then loads as
an empty box. peroxide checks this before sending you to it and tells you if
the host looks wrong.

Verification can only be completed interactively, so a long-running `peroxide
imap`/`smtp` process that hits a challenge while refreshing its session can't
resolve it on its own. It will tell you to run `peroxide auth`
again.

## Usage

peroxide can be served locally with:

```
peroxide serve
```

> Don't start peroxide multiple times.
> This requires ports 1025 (smtp) and 1143 (imap) available.

Once the bridge is started, you can configure your e-mail client with the
following settings:

* Hostname: `localhost`
* Port: 1025
* Security: none
* Username: your ProtonMail username
* Password: the bridge password (NOT your ProtonMail password)

## License

MIT

## Disclosure

This project started as a fork of
[hydroxide](https://codeberg.org/emersion/hydroxide) — thanks to emersion and
all upstream contributors. peroxide diverges from it: the long-term goal is a
minimal SMTP/IMAP-only bridge.
