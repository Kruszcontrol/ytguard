# YTGuard

Family YouTube controls for Linux Mint (or any systemd Linux) + Google Chrome.

- **Two-tier filtering**: **Hide** (the kid never sees the video — no title or thumbnail anywhere) and **Block** (visible with a lock; the kid can ask a parent). Each tier has its own Allow and Deny lists for videos, channels, keywords (title / description / tags / channel name), categories and attributes (Shorts, live, long videos, YouTube's kid-safety flags).
- **Shared filter lists**: subscribe to public lists by topic and age range (like Pi-hole/ad-block lists) from [ytguard-lists](https://github.com/Kruszcontrol/ytguard-lists) or any https address. They update daily, and your own entries always override them.
- **Shorts switch** per kid: follow the filters, block all Shorts, or hide them everywhere.
- **Time controls** per kid and weekday: daily minutes, allowed hours, enforced breaks (e.g. every 20 min watching → 10 min pause), bonus time, pause now.
- **Approval requests**: the kid taps "Ask a parent"; you approve the video or its whole channel from the web UI or a Home Assistant phone notification.
- **Daily report** per kid of every video watched, plus blocked/hidden attempts, by **email** and/or **Home Assistant**.
- **Remote web UI** on each PC (HTTPS, app password, "remember this device").
- **Home Assistant**: over MQTT, each PC and each kid appear automatically as devices (time used/left, watching now, pause switch, +time buttons, update notices). A ready-made blueprint sends approval requests to your phone with Allow/Deny buttons, for every PC. A webhook and a REST API with scoped tokens are there as an alternative.

## How it works

```
Kid's Chrome ── YTGuard extension (force-installed by Chrome policy, can't be removed)
                    │  http://127.0.0.1:7878  (loopback only)
                    ▼
             ytguard daemon (systemd, own user, SQLite in /var/lib/ytguard)
               ├─ identifies the kid by the Linux user that owns the connection
               ├─ rules engine, time accounting, watch log
               ├─ https://<pc>:8443  parent web UI + REST API
               └─ daily reports → SMTP / Home Assistant webhook
```

- The extension hides every video tile until the daemon says it may be shown, so hidden videos never flash on screen.
- Video pages are covered and paused until the video is checked. If the daemon can't be reached, nothing plays (fail closed).
- Watch time is counted by the daemon from heartbeats, so several tabs count once.
- Linux accounts that aren't set up as kids (e.g. yours) are not filtered (configurable).

## Get it

**Download a release** (recommended). Go to the repository's **Releases** page, download `ytguard-linux-amd64` (or `-arm64`) and `SHA256SUMS` to each kid PC, then:

```bash
sha256sum --check --ignore-missing SHA256SUMS
chmod +x ytguard-linux-amd64
mv ytguard-linux-amd64 ytguard
```

**Or build from source** with Go 1.22+:

```bash
git clone https://github.com/kruszcontrol/ytguard.git && cd ytguard
make            # builds ./ytguard and prints its version
make test
```

Build with `make` rather than plain `go build`. `make` stamps the version from the git tag and the GitHub repository (used for update checks); a plain `go build` produces an unversioned "dev" build.

The result is one static binary (~12 MB) with the extension and web UI built in.

## Install (on each kid PC)

Each kid needs their own **standard** (non-sudo) Linux account. Then, from an admin account:

```bash
sudo ./ytguard install
```

The installer:

1. installs `/usr/local/bin/ytguard` and a `ytguard` system user;
2. asks for **YTGuard's parent login** (its own username/password — not a Linux account);
3. asks which Linux accounts are kids;
4. writes Chrome policy (`/etc/opt/chrome/policies/managed/ytguard.json`) that force-installs the extension and disables incognito, guest mode, adding profiles, developer tools, `javascript:` URLs/bookmarklets, and (optionally) all other extensions;
5. blocks alternative YouTube front-ends (Invidious, Piped…) listed in `/etc/ytguard/url-blocklist.txt`. That file is written once, so edit it, then run `sudo ytguard install --upgrade` to apply;
6. starts the `ytguard` service and prints the UI address and certificate fingerprint.

Options: `--admin-addr :8443`, `--youtube-restrict 1|2` (also force YouTube Restricted Mode), `--allow-extensions`.

Then have each kid fully quit and reopen Chrome, and check `chrome://policy` and `chrome://extensions`.

**Other browsers:** this only controls Chrome. YTGuard reports other browsers it finds (see [Other browsers and video apps](#other-browsers-and-video-apps)). Remove or restrict them for kid accounts, e.g. Mint's built-in Firefox:

```bash
sudo groupadd browser-adults && sudo usermod -aG browser-adults $USER
```

```bash
sudo dpkg-statoverride --update --add root browser-adults 0750 /usr/lib/firefox/firefox
```

Also check for Flatpak/Snap browsers (`flatpak list`, `snap list`).

## Updating

YTGuard never updates itself. Twice a day each PC asks GitHub whether a newer release exists. It sends nothing except a normal web request, and you can turn it off under Settings → Updates. When there is one:

- the web UI shows a banner with a link to the release notes;
- the Home Assistant API reports `update_available`, and an `update_available` webhook event is sent once;
- an adult installs it when convenient:

```bash
sudo ytguard upgrade            # shows what's new, asks, downloads, verifies the checksum, installs
sudo ytguard upgrade --check    # just check
```

An upgrade keeps everything: kids, filters, history, logins, Home Assistant tokens, the certificate and the extension ID. The service restarts (kids may see "not responding" for a few seconds). Chrome picks up the new extension within a few hours, or as soon as it's restarted. Database changes are applied automatically, after a backup to `/var/lib/ytguard/ytguard.db.backup-*`. The previous binary is kept as `/usr/local/lib/ytguard/ytguard.previous` in case you need to go back. The download is checked against the release's `SHA256SUMS`. That catches corrupted downloads, but trusts whoever controls the GitHub repository.

To install your own build over an existing install: `make upgrade` (or `sudo ./ytguard upgrade --file ./ytguard`).

## Using it

Open `https://<kid-pc>:8443` from your phone or computer. Accept the self-signed certificate after checking that its fingerprint matches what the installer printed. Log in and tick **Remember this device**.

- **Dashboard**: each kid's time, what they're watching now, +15/+30/+60 min, pause, end break, and pending requests.
- **Filters**: tabs for **Hide** and **Block**. Each has Video / Channel / Keyword / Category / Attribute sections with Allow and Deny lists. Paste links or @handles, choose all kids or one kid, and move entries between lists and tiers. **Overview** shows every entry with its setting in both tiers.
- **History**: per kid and day, every video watched, plus blocked and hidden attempts. One-click Hide/Block/Allow for a video or its channel. You can preview or resend the daily report.
- **Kids**: Hide/Block defaults for unknown videos, the **Shorts** setting, comments/autoplay options, and the weekly schedule (daily minutes, allowed hours, breaks).
- **Tester**: paste a video link to see what each kid would get, and which rule decided.
- **Settings**: email (SMTP), Home Assistant, report time, optional YouTube API key, and import/export of filters to copy them between PCs.
- **Security**: change the password, see and sign out devices, create API tokens, and read the activity log.

### How filtering decides

1. **Hide** tier first. If a Hide **Deny** wins, the video is gone: removed from every list, and a direct link shows "This video isn't available". You still see the attempt in History.
2. Otherwise the **Block** tier decides. If a Block **Deny** wins, the video shows with a lock and the kid can ask. Otherwise it plays.
3. Within a tier, the most specific type wins: **Video → Channel → Keyword → Category → Attribute → kid's default**. At each step, rules for a specific kid beat all-kids rules, and **Deny beats Allow**.

Examples: Block-Allow keyword "minecraft" + Hide-Deny keyword "creepy" → "Creepy Minecraft house" is hidden. Hide-Allow a video + Block-Deny the same video → it shows up but needs your OK.

"Allowlist only" setups: set a kid's **Block default** to *block unknown videos*, then Allow channels you trust. Setting the **Hide default** to *hide unknown* goes further: only Hide-Allow entries ever appear.

Without a YouTube API key, feed tiles only expose title and channel, so description/tag/category rules are checked when a video is opened. Hidden videos then get the "not available" screen. With a free YouTube Data API v3 key (Settings), those rules also apply to tiles before they're shown.

### Shared filter lists

Under **Filters → Lists** you can subscribe to lists that other people maintain, choose which kids each applies to, and switch them on or off.
The recommended lists come from [Kruszcontrol/ytguard-lists](https://github.com/Kruszcontrol/ytguard-lists): scary/horror, violence, mature content, dangerous challenges, gambling and scams, and basics for young kids, with suggested ages.
You can also subscribe to any https:// address.

- **Your own entries win** over a list's entry of the same type. The usual order still applies (video → channel → keyword → category → attribute). So to undo a list's channel entry, add that channel (or a video) yourself; a broad keyword Allow won't unhide a channel a list hides.
- The Rule tester and History say which list made a decision, and the Tester has one-click **Allow this video / channel** overrides.
- Lists are checked for updates daily. If a download fails, the previous copy stays in use. Lines a list gets wrong are skipped and shown as warnings.
- Lists can contain Allow entries too; the Lists page shows how many, so you can see whether a list loosens anything.
- **Share your own:** Filters → Lists → *Download list file* exports your all-kids entries in list format.

List format (plain text):

```
! Title: Scary and horror
! Description: Hides horror and creepypasta.
! Ages: 0-12
[hide deny]
keyword: creepypasta
keyword(title,description,tags): jumpscare
channel: UCxxxxxxxxxxxxxxxxxxxxxx @handle Channel name
attribute: not_family_safe
[block deny]
keyword: prank
```

The full format and contribution guide are in the [ytguard-lists README](https://github.com/Kruszcontrol/ytguard-lists#list-format). Check a file with `ytguard list-check FILE`.

### Shorts

Per kid (Kids → Shorts), overriding all filter rules and approvals:

- **Follow the filters**: Shorts are treated like any other video.
- **Block all Shorts**: Shorts are listed with a lock, never play, and there's no "Ask a parent" button.
- **Hide all Shorts**: Shorts shelves, the Shorts tab, Shorts search chips, Shorts tiles and direct `/shorts/` links are all removed.

A video is known to be a Short once it has appeared anywhere as a Short. Opening it later through a plain `/watch?v=` link is still caught.

### Time and breaks

Time counts only while a video is actually playing. Breaks: after *N* minutes of watching, YouTube pauses for *M* minutes with a countdown. Pausing for *M* minutes on their own also counts as a break. Allowed hours and the daily limit both apply; the kid sees a "time left · break in" badge.

### Other browsers and video apps

YTGuard can't control other browsers, but it looks for them so you know. It's on by default; turn it off under Settings.

- **Every minute** the daemon checks the kids' running programs for any browser or YouTube app other than the managed Chrome: Firefox, Tor Browser, Brave, Chromium and unmanaged Chrome copies, AppImages, Flatpaks, FreeTube, yt-dlp, mpv and others.
- **Every hour** a root job (`ytguard-scan.timer`) looks for installed ones:
  - portable browsers, browser downloads and AppImages in kids' home folders;
  - Flatpaks installed by a kid (possible without sudo on Mint: `flatpak install --user`);
  - browsers installed for everyone that kids are allowed to run.

  Run it now with `sudo ytguard scan`.
- Findings appear on the dashboard (with a **Dismiss** button for anything you're fine with), in the daily report, and as a one-time `unapproved_app` Home Assistant event.

To deal with what it finds:

```bash
sudo -u KID flatpak uninstall --user org.mozilla.firefox
```

```bash
sudo rm -rf /home/KID/Downloads/firefox
```

```bash
sudo dpkg-statoverride --update --add root browser-adults 0750 /usr/lib/firefox/firefox
```

The first removes a Flatpak a kid installed, the second deletes a portable copy, and the third makes Mint's Firefox usable only by members of the `browser-adults` group (see Install). To stop kids running programs from their home folders at all, mount `/home` with `noexec`. Detection goes by program names, so a renamed browser can slip through.

### Reports

Sent daily at the configured time (default 20:30). If the PC is off then, the report goes out at the next start. Email works with any SMTP server. For Gmail, use an app password with `smtp.gmail.com:587` and STARTTLS. Home Assistant receives it as a `daily_report` event (MQTT or webhook). See [docs/home-assistant.md](docs/home-assistant.md).

## Authentication

| Access | Credential |
|---|---|
| You, in a browser | YTGuard admin username/password (stored as PBKDF2-SHA256). "Remember this device" keeps you logged in for 90 days (configurable). Each device can be signed out from Security. Changing the password signs out everything. |
| Home Assistant / scripts | Named API tokens with scopes `read`, `control`, `admin`. Sent as `Authorization: Bearer …`. Stored hashed. Can't open the web UI. |
| Kid's extension | No credential. Talks only to 127.0.0.1 and is identified by Linux user. This API has no admin functions. |
| Forgot password | `sudo ytguard passwd` on the PC (local only). |

The UI uses HTTPS (self-signed certificate generated at install), CSRF tokens on every form, login rate limiting with lockout, strict security headers, and an activity log. No OS or root credentials are ever used or sent over the network.

## Threat model and limits

Designed to stop kids from casually (and moderately cleverly) getting around it on a PC where they have **no sudo**:

- Can't remove or disable the extension, use incognito or guest profiles, open devtools or view-source, or install other extensions.
- Can't stop the daemon or read or alter its data (it runs as its own user; the data is `0700`).
- Can't impersonate a sibling: identity comes from the kernel's socket ownership, not from anything the kid controls.
- Can't fake video details to the daemon: what the extension sends is only used for that one decision, never cached. Approval requests only work for videos the kid was actually shown as blocked, and are limited to 10 per hour.
- Root never trusts anything the service account can write: install settings live in `/etc/ytguard`, and the rollback binary in `/usr/local/lib/ytguard`.

Known limits:

- **Chrome only.** Other browsers, YouTube on phones/TVs/consoles, and YouTube videos embedded in Google search results are out of scope. Use router/DNS controls for other devices.
- A kid can run a **portable browser** (e.g. a Firefox download unpacked in their home folder) without sudo; YTGuard can't see that browser. Mounting `/home` with the `noexec` option stops programs running from home folders. This is the main remaining gap for tech-savvy kids.
- If the ytguard service is stopped, the extension fails closed (nothing plays). A kid can't stop it, but if it crashed, another program could briefly take its port during the 2-second restart. That is far-fetched, but noted.
- A determined, technical kid could edit the extension's files inside their own Chrome profile directory. Chrome doesn't verify self-hosted extensions the way it does Web Store ones.
- YouTube changes its page structure from time to time. Tile selectors live in `extension/content.js` and `extension/overlay.css`. Unknown tile types stay hidden rather than shown (fail closed).
- Policy is machine-wide: blocking extensions and devtools also affects parent accounts on that PC.

## Commands

```
sudo ytguard install [--admin-addr :8443] [--youtube-restrict 0|1|2] [--allow-extensions]
sudo ytguard upgrade [--check] [--file PATH] [-y]
sudo ytguard scan
sudo ytguard uninstall [--purge]
sudo ytguard passwd
ytguard serve [--data DIR] [--admin-addr ADDR] [--admin-http] [--trust-proxy] [--dev]
ytguard report --kid NAME [--day YYYY-MM-DD] [--send] [--html]
ytguard policy
ytguard list-check FILE...
ytguard version [-v]
```

Logs: `journalctl -u ytguard -f`.

## Releasing (maintainers)

```bash
git tag v1.2.0
git push origin v1.2.0
```

The `release` GitHub Action runs the tests, builds `ytguard-linux-amd64` and `ytguard-linux-arm64` stamped with the tag and repository, and publishes them with `SHA256SUMS` and generated release notes. Tags with a suffix (`v1.3.0-rc1`) become pre-releases, which installed copies don't offer as updates. Forks automatically check their own repository.

Versioning: bump the patch number for fixes, the minor number for features, and the major number for changes that need manual steps. The Chrome extension version comes from the tag, so every release reaches the kids' Chrome automatically.

Changing the database: add a function to the end of `migrations` in `internal/store/store.go`. Never edit one that has been released.

## Development

```bash
make test
./ytguard passwd --data ./devdata
./ytguard serve --data ./devdata --admin-addr 127.0.0.1:8443 --dev
```

In `--dev` mode the daemon accepts an unpacked extension. Branded Chrome no longer honours `--load-extension`, so load `extension/` from `chrome://extensions` → *Load unpacked*, or via the DevTools protocol (`Extensions.loadUnpacked`). Add your own Linux user as a kid in the UI to see filtering.

Layout:

```
cmd/ytguard/          CLI entry point
internal/rules/       two-tier rules engine
internal/timekeeper/  limits, hours, breaks
internal/peercred/    kid identification from /proc/net/tcp
internal/core/        shared app logic (decisions, approvals, Shorts setting)
internal/extapi/      loopback API for the extension + CRX/update serving
internal/admin/       web UI, REST API, templates, static files
internal/auth/        passwords, sessions, API tokens, rate limiting
internal/report/      daily reports + scheduler
internal/notify/      SMTP + Home Assistant webhook
internal/store/       SQLite
internal/install/     installer, upgrade, Chrome policy, systemd unit, certificates
internal/update/      GitHub release check + verified download
internal/appscan/     detection of other browsers / video apps
internal/filterlist/  shared filter list format, download, catalog
internal/mqtt/        minimal MQTT 3.1.1 client
internal/hamqtt/      Home Assistant MQTT discovery, state and commands
docs/blueprints/      Home Assistant blueprint (notifications + approvals)
extension/            Chrome MV3 extension (packed and signed by the daemon at start)
```

## License

MIT — see [LICENSE](LICENSE).
