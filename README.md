# peroxide

The Unix philosophy: *do one thing and one thing well*. Everything else is just the bartender watering your drink.

peroxide is a third-party Proton-Mail bridge, built upon hydroxide from [emersion](https://codeberg.org/emersion) and numerous contributors, with grateful thanks. we owe these people a drink. several.

it aims at one thing and one thing only: a local bridge to your IMAP and SMTP in ProtonMAIL. Note that *mail* does not mean *suite*. No CardDAV, no CalDAV, no nothing. It doesn't want to hold your contacts, your hand or plan your week. It wants to move your mail and then be left alone.

the official [Proton Bridge](https://github.com/ProtonMail/proton-bridge) is excellent, but costs RAM the way a bad week costs money... and most things these days. some users report 2000 MiB of RAM being hogged, with 600 MiB as normal. peroxide on the other hand: idle barely hits 21MiB. still, proton bridge gets the job done magnificently, with all bells and whistles attached.  But we're not picking a fight with it. it could take us.

if you are NOT struggling with RAM and are not savvy around the terminal, this is NOT for you. Be advised, here be dragons, and they don't even buy you a drink first.

## How does it work?

peroxide is the server between you and ProtonMail. Standard protocols (SMTP and IMAP) go in one ear, ProtonMail API requests come out the other. It allows you to use your preferred e-mail clients (thunderbird, aerc, neomutt, etc) and `git-send-email` with ProtonMail. It asks for nothing in return but two small ports and a password.

    +-----------------+   IMAP, SMTP   +-------------+   ProtonMail   +--------------+
    |                 |                |             |      API       |              |
    |  E-mail client  <---------------->  peroxide   <---------------->  ProtonMail  |
    |                 |    peroxide    |             |   protonmail   |              |
    +-----------------+    password    +-------------+    password    +--------------+

## Building from source

peroxide runs on Linux only (amd64 and arm64 are tested), and building it for any other system stops at once with `undefined: peroxide_builds_for_linux_only`. It is purely written in Go, no cheap tricks, so you will need at least version 1.27 in your system to compile the binary. What comes out is one fully static binary, no linked dependencies, no strings attached:

```shell
git clone https://github.com/seamanticus/peroxide.git
cd peroxide
go build ./cmd/peroxide
```

With a C compiler around, Go links the binary dynamically against libc by default; `CGO_ENABLED=0 go build ./cmd/peroxide` makes it fully static.

Then simply move the `peroxide` binary into your `$PATH` and get on with your life.

For Arch Linux:

```shell
su -c 'cp peroxide /usr/bin/'
```

## Authentication

Peroxide can only retrieve emails from ProtonMail once the credentials have been authenticated and stored in the system (encrypted, btw). It used to be a twelve-step program, but we quit trying to quit; so it is five now. The process begins with:

```shell
peroxide auth <username>@protonmail.com
```

1. Upon dispatch, you will be prompted for a `password`, this is your *PROTONMAIL PASSWORD*. The real one. Don't panic, it is being asked for a good reason.

2. If Proton asks for a Captcha, peroxide prints a `https://verify.proton.me/...` URL and opens it in your browser (through `xdg-open`, in a graphical session). peroxide does not solve the Captcha, it must be done by a human, and tonight the human is you. No browser popped up, or the wrong one? The URL is already on your clipboard if your terminal allows it (most modern ones do); paste it into the browser of your preference, whichever one you keep around out of love or habit, on this machine or any other, phone included.

3. Solve the Captcha... prove to the machine that you are not a machine. The machine, notably, is never asked to prove anything. The page eventually tells Proton you have passed; good, you can close it. Then return to the terminal and press ENTER there.

4. If you have enabled 2FA (which you should!!!) it will prompt you for it. Press ENTER after having input the 6 numbers. (An account still in Proton's old two-password mode is also asked for its mailbox password here.)

5. A 44-character string ending in `=` will appear in a box: this is the *BRIDGE PASSWORD* for your ProtonMail account, so you must save it carefully to later use it in your mail client of choice. Guard it the way you guard the last cigarette in the pack. Peroxide does NOT store this password anywhere, NOR does it copy it into your clipboard. It is yours and only yours. Everyone should have something like that.

*Note*: your ProtonMail credentials are encrypted and stored on disk and the *bridge password* is the key to decrypt it. So discretion is advised in its use and storage. The neck-beards already know the move: save the *bridge password* to a `.gpg` to later invoke into mutt, neomutt or aerc.

### Extra options for authentication

These flags only apply to `peroxide auth`:

| Flag | Default | Meaning |
| --- | --- | --- |
| `-captcha-mode` | `browser` | `browser` prints the verification URL and opens it, `manual` only prints it, `disabled` reports the API error instead |
| `-captcha-timeout` | `10m` | How long to wait for your ENTER |
| `-remove` | | Delete the stored credentials of `<username>` and exit |

*Note*: Verification can only be completed interactively. A long-running `peroxide serve` process that hits a challenge while refreshing its session CANNOT resolve it alone; it was never taught to solve puzzles, only to carry mail. It will tell you to reauthenticate using `peroxide auth`, and you will go through the five steps once more, now older and wiser.

## Usage

### Plain vanilla

Served locally, straight up, no ice:

```sh
peroxide serve
```

> One `serve` at a time: it holds ports 1025 (smtp) and 1143 (imap) and a lock, and a second
> one stops with "peroxide serve is already running". One bridge, two ports, no roommates.

### Desktop Environment / Window Managers

If integrating it to autocommands in your desktop environment or window manager, it is best to incorporate a check prior to dispatching:

```sh
pidof peroxide || peroxide serve
```

### systemd service

If you want it completely automated in a systemd distro, you must first create a service script (all inclusive below). It is ONLY advised to have it at `--user` level, never at system wide level. A bridge is a personal thing; keep it in your own room. You have been warned:

```sh
mkdir -p ~/.config/systemd/user
printf "[Unit]
Description=ProtonMail IMAP and SMTP bridge (peroxide)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/bin/peroxide serve
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
" > ~/.config/systemd/user/peroxide.service

systemctl --user enable --now peroxide.service
```

To check if or how peroxide is running:

```sh
systemctl --user status peroxide.service
```

## E-mail clients

Once peroxide is fully functional and running, you can configure your e-mail client of choice
with the following settings:

For IMAP:

* Hostname: `127.0.0.1` (not `localhost`, which may resolve to `::1`, where peroxide doesn't listen)
* Port: `1143`
* Security: `SSL/TLS`, *not STARTTLS* (with `serve -plaintext`: `none`)
* Username: `your_username@protonmail.com`
* Password: `bridge password` (*NOT your proton password*)

For SMTP:

* Hostname: `127.0.0.1`
* Port: `1025`
* Security: `SSL/TLS`, *not STARTTLS* (with `serve -plaintext`: `none`)
* Authentication: `PLAIN` (peroxide doesn't offer `LOGIN`)
* Username: `your_username@protonmail.com`
* Password: `bridge password` (*NOT your proton password*)

*Note*: peroxide speaks TLS from the first byte (implicit TLS, the way ports 993 and 465 do) on ports 1143 and 1025, even on `127.0.0.1`. Why bother on your own machine? because any other user on it can sit on port 1143 while peroxide is down, and your client would cheerfully hand them the bridge password. The certificate is what tells the real peroxide from the impostor, so your client has to trust it, not skip checking it. The first `serve` makes one (no CA, it can vouch for nothing but peroxide) and logs its fingerprint; `peroxide list` shows it again. How to trust it, client by client: [Listening and TLS](docs/commands.md#listening-and-tls).

*Notes on STARTTLS*: peroxide doesn't have it in the menu, so don't ask. A client set up for plain text or STARTTLS gets the connection closed after 10 seconds, and `serve` logs why.

*Notes on plain text*: `serve -plaintext` turns TLS off, on loopback addresses only. You're on your own then.

*More notes on aerc*: use `imaps://` and `smtps://` (in aerc, `imap://` and `smtp://` mean STARTTLS), and start aerc with peroxide's certificate directory added to the system's: `SSL_CERT_DIR=/etc/ssl/certs:$HOME/.config/peroxide/tls aerc` (an alias does it). DO NOT reach for `SSL_CERT_FILE`, it throws the system's roots away and your other accounts with them. Don't use `+insecure` with TLS: it turns certificate checking off, which is the whole point. `imap+insecure://` and `smtp+insecure://` are for `serve -plaintext` only.

*Even more notes*: some clients store your *bridge password* in plain text, which is an aberration!!! Like leaving your house key under the mat with a sign pointing at it. An elegant solution is presented below: encrypted and on demand.

### aerc + gpg encryption

If using the aerc email client and gpg (for an encrypted *bridge password*) for the ultimate lightweight combo, your `accounts.conf` would look as below (if using `pass`). Notice that `@` is replaced with `%40` in source and outgoing, which is how it should be written:

```
[ username ]
from                = <username> <username@protonmail.com>
source              = imaps://username%40protonmail.com@127.0.0.1:1143
source-cred-cmd     = gpg --quiet --for-your-eyes-only --decrypt ~/.password-store/peroxide-username.gpg | sed -n 1p
outgoing            = smtps://username%40protonmail.com@127.0.0.1:1025
outgoing-cred-cmd   = gpg --quiet --for-your-eyes-only --decrypt ~/.password-store/peroxide-username.gpg | sed -n 1p
```

*Note*: you must have first stored your *bridge password* in the appropriate gpg files. Easy bullet-proof solution in any Unix systems is using `pass`. But you can also create it manually, though that is another rabbit hole, and the rabbit hole has no pub at the bottom. In this case, the bridge password was stored in `peroxide-username.gpg`

*Note on the certificate*: start aerc with `SSL_CERT_DIR` as in [the notes on aerc](#e-mail-clients) above, so it trusts peroxide's certificate and still every one it trusted before.

## Documentation In-Depth

the above is enough to get on with your day. if you wanna dig through what's under the hood, the [documentation](docs/README.md) is quite detailed for a sunday afternoon.

## Why the name peroxide?

hmmm... things you learn from strangers, at a road-side pub, on a monday night: in chemical nomenclature, the peroxide group can only accept two external connections. apparently electrons are also fond of IMAP and SMTP to get their point across without friction. if you wanna complain, I will listen, if you shout the next round.

## Credits

this project started as a fork of [hydroxide](https://codeberg.org/emersion/hydroxide). thanks to emersion, all upstream contributors and patches performed.

License : [MIT](LICENSE)
