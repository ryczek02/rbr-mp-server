# Deploying RBR-MP-Server

The server is a single static binary that listens on one UDP port. There is no
database, no persistent state, no TLS and no admin interface — deployment is
"run the process, open the port".

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
git clone https://github.com/lukaszryczko/rbr-mp-server
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
command: ["-addr", ":40100", "-tick", "60", "-echo", "1s", "-stats", "60s"]
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
ExecStart=/usr/local/bin/rbrmp-server -addr :40100 -tick 30 -echo 0 -stats 60s
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

## Verifying a deployment without the game

`rbrmp-sim` speaks the full protocol and measures the loop end-to-end:

```bash
go run ./cmd/rbrmp-sim -server your.host:40100 -for 5s
```

A healthy public server shows a round trip of your internet latency plus half a
tick interval, and (with the echo enabled) an echo age within a few ms of the
configured delay.

## Operational notes

* **Stats**: with `-stats 60s` the log carries one line per minute — client
  count, packets and kB/s each way. That plus join/leave/timeout lines is the
  whole observability story, by design.
* **The echo player** (`-echo`) is a testing aid. For a real session between
  friends run `-echo 0`; anyone who wants the ghost for solo practice can run
  their own server locally.
* **Scaling**: snapshots are built per client per tick, so traffic grows with
  the square of the player count. At rally-sized sessions (a handful of cars)
  this is nothing; a 50-car server is untested territory.
* **Security**: the protocol is unauthenticated by design (see README —
  "no authority"). Run it for people you know, on a port you can close.
