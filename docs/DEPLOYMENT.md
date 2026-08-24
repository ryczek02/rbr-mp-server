# Deploying RBR-MP-Server

The server is a single static binary that listens on one UDP port. There is no
database and no TLS — deployment is "run the process, open the port". The only
state on disk is the ban list (`-bans`, default `bans.json` next to the
working directory), and the only admin interface is the optional RCON console
below.

## What it needs

| Resource | Requirement |
|---|---|
| CPU | negligible — a relay at 30 Hz for a handful of cars is microseconds of work per tick |
| Memory | a few MB; each client holds at most ~4 s of pose history |
| Network | **UDP 40100** inbound (and the replies outbound). ~7 kB/s in and `(players − 1) × ~5 kB/s` out per player at the defaults |
| Disk | none beyond the binary/image |

Any 1-vCPU VPS is far more than enough.

## Option 1 — Docker Compose (recommended)

```bash
git clone https://github.com/ryczek02/rbr-mp-server
cd rbr-mp-server
docker compose up -d
```

That builds the image (multi-stage, final stage is `scratch` — a static binary
and nothing else) and starts it with:

* UDP 40100 published,
* `restart: unless-stopped`,
* a read-only filesystem, all Linux capabilities dropped, non-root uid,
* a 128 MB memory cap.

Tune the server by editing the `command:` line in
[docker-compose.yml](../docker-compose.yml):

```yaml
command: ["-addr", ":40100", "-tick", "60", "-stale", "2s", "-stats", "60s"]
```

Logs and health:

```bash
docker compose logs -f       # join/leave lines and periodic traffic stats
docker compose ps            # is it running
docker compose down          # stop
```

## Option 2 — plain binary under systemd

Build (or cross-compile) and copy the binary:

```bash
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o rbrmp-server ./cmd/rbrmp-server
scp rbrmp-server you@host:/usr/local/bin/
```

`/etc/systemd/system/rbrmp-server.service`:

```ini
[Unit]
Description=RBR-MP multiplayer relay
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/rbrmp-server -addr :40100 -tick 30 -stale 2s -stats 60s
Restart=always
User=nobody
DynamicUser=yes
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now rbrmp-server
journalctl -u rbrmp-server -f
```

## Option 3 — Ansible (repeatable, many hosts, upgrades)

Option 2, automated: [deploy/ansible/](../deploy/ansible/) fetches a pinned
release binary from GitHub Releases, installs a hardened systemd unit and
opens UDP 40100 through ufw. One command deploys, the same command with a new
version upgrades and restarts:

```bash
cd deploy/ansible
cp inventory.example.ini inventory.ini   # put your host in (gitignored)
ansible-playbook -i inventory.ini deploy.yml
ansible-playbook -i inventory.ini deploy.yml -e rbrmp_version=v0.2.0   # upgrade
```

The repo is private, so the binary is downloaded **on your machine** with
`gh release download` and copied up — no GitHub token ever reaches the server.
That means the control node needs `gh` installed and `gh auth login` done;
releases are cached under `~/.cache/rbrmp-deploy/<version>/`. If the repo ever
goes public, swap that task back to `get_url` on the release URL.

Flags live in `rbrmp_flags`, the port in `rbrmp_port`, and
`rbrmp_manage_ufw=false` skips the firewall task on hosts managed elsewhere.
Needs the `community.general` collection for the ufw task
(`ansible-galaxy collection install community.general`).

By default (`rbrmp_auto_update: true`) the playbook also installs a systemd
timer (`rbrmp-update.timer`, every `rbrmp_update_every`, default `5min`) that
independently checks GitHub Releases on the **server itself** and swaps the
binary in when a new one appears — on top of whatever version you pinned with
`ansible-playbook ... -e rbrmp_version=...`. It does a plain unauthenticated
`curl` against `.../releases/latest/download/...`, which only succeeds if the
repository is public; against a private repo (the current state of this repo)
the timer will fail every run. Set `rbrmp_auto_update: false` until either the
repo goes public or the update script is given credentials, or rely on
Option 4 below instead.

## Option 4 — automatic, via CI on every release tag

[.github/workflows/release.yml](../.github/workflows/release.yml) has a
`deploy` job that runs after every `v*` tag build: it downloads the fresh
`rbrmp-server-linux-amd64` binary with `gh release download`, `scp`s it to a
VPS over SSH, and does the same `/opt/rbrmp/rbrmp-server-<version>` +
symlink + `systemctl restart rbrmp-server` dance as the Ansible playbook above
— so both target the same host layout and only one should be managing a given
server. It needs three repository secrets set (Settings → Secrets and
variables → Actions):

| Secret | Value |
|---|---|
| `DEPLOY_SSH_KEY` | private key for a user allowed to `scp` and run the `sudo mv/ln/systemctl` commands |
| `DEPLOY_HOST` | the VPS hostname or IP |
| `DEPLOY_USER` | the SSH user on the VPS |

Once those are set, `git tag vX.Y.Z && git push --tags` is the entire deploy:
build, publish the release, ship the binary, restart the service.

## Firewall

The one rule everyone forgets: the port is **UDP**, not TCP.

```bash
# ufw
sudo ufw allow 40100/udp
# firewalld
sudo firewall-cmd --add-port=40100/udp --permanent && sudo firewall-cmd --reload
```

Cloud providers gate this in their security groups / network rules as well —
"connection works from the VPS itself but not from outside" is almost always
the provider's firewall still closed for UDP.

## RCON (admin console)

With `-rcon-password` (or the `RBRMP_RCON_PASSWORD` environment variable) set,
the server also listens on **TCP** `-rcon-addr` (default `:40101`) for the
admin console that `clirbrmp` talks to: kick, ban/unban, say, players, status.
No password = no listener.

**Do not open this port to the world.** It is plain TCP with a plaintext
password — an obstacle, not a wall. Either:

* bind it to localhost (`-rcon-addr 127.0.0.1:40101`) and administrate over an
  SSH tunnel: `ssh -L 40101:127.0.0.1:40101 you@host`, then `clirbrmp` against
  `127.0.0.1:40101`, or
* firewall TCP 40101 to your admin IPs only:
  `sudo ufw allow from <your-ip> to any port 40101 proto tcp`.

Under systemd, keep the password out of the unit file's command line (visible
in `ps`) by passing it as an environment variable:

```ini
[Service]
Environment=RBRMP_RCON_PASSWORD=change-me
# or, better, a root-only file:
# EnvironmentFile=/etc/rbrmp/rcon.env
ExecStart=/usr/local/bin/rbrmp-server -addr :40100 -rcon-addr 127.0.0.1:40101 -bans /var/lib/rbrmp/bans.json
```

Note the hardened unit in Option 2 uses `ProtectSystem=strict`, which makes
the filesystem read-only — point `-bans` somewhere writable and whitelist it
(`ReadWritePaths=/var/lib/rbrmp` plus a `StateDirectory=rbrmp`), or bans will
not persist.

## Verifying a deployment without the game

`rbrmp-sim` speaks the full protocol and measures the loop end-to-end:

```bash
go run ./cmd/rbrmp-sim -server your.host:40100 -for 5s
```

A healthy public server shows a round trip of your internet latency plus half
a tick interval. Run two sims at once to see them relay each other.

## Operational notes

* **Stats**: with `-stats 60s` the log carries one line per minute — client
  count, packets and kB/s each way. That plus join/leave/timeout lines is the
  whole observability story, by design.
* **Scaling**: snapshots are built per client per tick, so traffic grows with
  the square of the player count. At rally-sized sessions (a handful of cars)
  this is nothing; a 50-car server is untested territory.
* **Security**: the protocol is unauthenticated by design (see README —
  "no authority"). Run it for people you know, on a port you can close.
