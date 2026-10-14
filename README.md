# peroxide

In the Unix philosophy: do one thing and one thing well. Everything else is just the bartender watering your drink.

peroxide is a third-party Proton-Mail bridge, a fork of hydroxide from the emersion group, with grateful thanks. We owe those people a drink. Several.

Unlike its predecessor, peroxide aims at one thing and one thing only: a local bridge to your IMAP and SMTP in the Proton-Mail eco system. No CardDAV, no CalDAV, no nothing. It doesn't want to hold your contacts or plan your week. It wants to move your mail and then be left alone. It's a local email bridge, period.

The official Proton Bridge is excellent at what it does, and it costs RAM the way a bad week costs money. Some users report as much as 2000Mb of RAM being hogged, with 600Mb as normal. peroxide, firing on all cylinders, barely hits 21Mb. Still, it is the official and secure option and it gets the job done magnificently for its load. We're not picking a fight with it. It could take us.

If you are NOT struggling with RAM usage and are not savy around the terminal, this is NOT for you. Be advised, here be dragons!!! And they don't even buy you a drink first.

## How does it work?

peroxide is a server that stands between you and ProtonMail and translates. Standard protocols (SMTP and IMAP) go in one ear, ProtonMail API requests come out the other. It allows you to use your preferred e-mail clients and `git-send-email` with ProtonMail. It asks for nothing in return but two small ports and a password.

    +-----------------+   IMAP, SMTP   +-------------+   ProtonMail   +--------------+
    |                 |                |             |      API       |              |
    |  E-mail client  <---------------->  peroxide   <---------------->  ProtonMail  |
    |                 |    peroxide    |             |   protonmail   |              |
    +-----------------+    password    +-------------+    password    +--------------+

## Building from source

peroxide is purely written in GO, no cheap tricks, so you will need at least version 1.24 in your system to compile the binary. What comes out is one fully static binary, no linked dependencies, no strings attached:

```shell
git clone https://github.com/seamanticus/peroxide.git
cd peroxide
go build ./cmd/peroxide
```

Then simply move the `peroxide` binary into your `$PATH` and get on with your life. For Arch Linux:

```shell
su -c 'cp peroxide /usr/bin/'
```

## Authentication

Peroxide can only retrieve emails from ProtonMail once the credentials have been authenticated and stored in the system (encrypted, btw). Read the following steps ATTENTIVELY, as it is nuanced. It is a twelve-step program, but nobody here is quitting anything. Once you get the hang of it, it's quite quick. The process begins with:

```shell
peroxide auth <username>@protonmail.com
```

1. Upon dispatch, you will be prompted for a `password`, this is your *PROTONMAIL PASSWORD*. The real one. Don't panic, it is being asked for a good reason.

2. peroxide does not solve the Captcha, it must be done by a human, and tonight the human is you. If your system has the global `$BROWSER` set, it should spawn automatically and you will see the helper page on `127.0.0.1:8765`, otherwise copy/paste the url displayer in your browser of preference, whichever one you keep around out of love or habit.

3. In the helper page, Section 1, copy the snippet presented to you to the clipboard by clicking the button `Copy`.

4. Then click the button in Section 2 to open the Proton Captcha page but DO NOT solve it yet. patience. the good part is coming.

5. Go to the new opened page, if not already there, and press F12, to open your browser's console. Yes, the console. Tonight you are
the kind of person.

6. For Firefox and Chromium-based browsers, type `allow pasting` and press ENTER. It should return `unexpected token: identifier` and that is correct.

7. Paste the snippet copied from Section 1 into the console and press ENTER. It should return `undefined`, which is ever so reassuring.

8. Now solve the Captcha... prove to the machine that you are not a machine. The machine, notably, is never asked to prove anything.

9. A long string will then appear at the very top of the page. If you have a mouse, just hover over it, right click and copy the long string. Ugly, long, beautiful string. It is yours.

10. Return to the helper page, paste it into Section 4 and press the button. It will send that token back to `peroxide auth` session.

11. If you have enabled 2FA (which you should!!!) it will prompt you for it. Press ENTER after having input the 6 numbers.

12. A 32-byte string `bridge password` ending in `=` will appear and this is the *BRIDGE PASSWORD* for your ProtonMail account, so you must save it carefully to later use it in your mail client of choice. Guard it the way you guard the last cigarette in the pack. Peroxide does NOT store this password anywhere, NOR does it copy it into your clipboard. It is yours and only yours. Everyone should have something like that.

*Note*: your ProtonMail credentials are encrypted and stored on disk and the *bridge password* is the key to decrypt it. So discretion is advised in its use and storage. If you are a neck-beard terminal monkey (or just tech savy), you already know the move: save the *bridge password* to a `.gpg` so you can later invoke into mutt, neomutt or aerc.

### Extra Options for Authentications

These flags only apply to `peroxide auth`:

| Flag | Default | Meaning |
| --- | --- | --- |
| `-captcha-mode` | `browser` | `browser` opens the helper page, `manual` only prints its URL, `disabled` reports the API error instead |
| `-captcha-listen` | `127.0.0.1:8765` | Address of the helper server. Keep it on loopback |
| `-captcha-timeout` | `10m` | How long to wait for you to finish |
| `-no-open-browser` | | Print the URL without trying to launch a browser |
| `-captcha-endpoint` | `https://mail-api.proton.me` | API host serving the challenge |

`-captcha-endpoint` is worth knowing about: the host must serve both `/core/v4/captcha` and the widget's assets under `/captcha/v1/assets/`. Several Proton hosts serve the first but not the second, and the challenge then loads as an empty box. peroxide performs checks prior to sending you to the helper page and will inform you of errors otherwise.

*Note*: Verification can only be completed interactively. A long-running `peroxide serve` process that hits a challenge while refreshing its session can NOT resolve it on its own; it was never taught to solve puzzles, only to carry mail. It will tell you to reauthenticate using `peroxide auth` again, and you will go through the twelve steps once more, older and wiser.

## Usage

### Option 1 - Plain vanilla

Served locally, straight up, no ice:

```sh
peroxide serve
```

> peroxide MUST NOT be spawned multiple times as it requires
> ports 1025 (smtp) and 1143 (imap) available for use. One bridge, two ports, no roommates.

### Option 2 - Desktop Environment / Window Managers

If integrating it to autocommands in your desktop environment or window manager, it is best to incorporate a check prior to dispatching:

```sh
pidof peroxide || peroxide serve
```

### Option 3 - SystemD Service in Arch

If you want it completely automated in Arch or any systemD distro, you must first create a service script (all inclusive below). It is ONLY advised to have it at `--user` level, never at system wide level. A bridge is a personal thing; keep it in your own room. You have been warned:

```sh
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
" | sudo tee /etc/systemd/user/peroxide.service

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

* Hostname: `localhost`
* Port: `1143`
* Security: none
* Username: `your ProtonMail username`
* Password: `bridge password` (*NOT YOUR PROTONMAIL PASSWORD*)

For SMTP:

* Hostname: `localhost`
* Port: `1025`
* Security: none
* Username: `your ProtonMail username`
* Password: `bridge password` (*NOT YOUR PROTONMAIL PASSWORD*)

*Note*: some clients store your *bridge password* in plain text, which is an aberration!!! Like leaving your house key under the mat with a sign pointing at it. An elegant solution is presented below: encrypted and on demand.

### aerc + gpg encryption

If using using aerc email client and gpg (for encrypted *bridge password*) for the ultimate lightweight combo, your `account.conf` would look as below (if using `pass`). Notice that `@` is replaced with `%40` in source and outgoing, which is how it should be written:

```
[ username ]
from                = <username> <username@protonmail.com>
source              = imap+insecure://username%40protonmail.com@127.0.0.1:1143
source-cred-cmd     = gpg --quiet --for-your-eyes-only --decrypt ~/.password-store/peroxide_username.gpg | sed -n 1p
outgoing            = smtp+insecure://username%40protonmail.com@127.0.0.1:1025
outgoing-cred-cmd   = gpg --quiet --for-your-eyes-only --decrypt ~/.password-store/peroxide-username.gpg | sed -n 1p
```

*Note*: you must have first stored your *bridge password* in the appropriate gpg files. Easy bullet-proof solution in any Unix systems is using `pass`. But you can also create it manually, though that is another rabbit hole, and the rabbit hole has no pub at the bottom. In this case, the bridge password is
stored in `peroxide-username.gpg`

## Why the name peroxide?

after a night on the piss, not recalling how you got back home, peroxide is what will be poured on cuts on forehead for disenfection, ain't it? it's also what you will use to write a very melodramatical and lame excuse to your boss for not showing up to work that day, but that's not today.

don't worry mate, we are not alcoholics.

we just code...

after the pub...

like a good sailor.

## License

MIT

## Disclosure

This project started as a fork of [hydroxide](https://codeberg.org/emersion/hydroxide) , thanks to emersion, all upstream contributors and patches performed. peroxide diverges from its premises: the long-term goal is a minimal SMTP/IMAP-only bridge to ProtonMail. One job, done well, and then the pub closes, too soon.
