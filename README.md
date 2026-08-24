<img src="logo.svg" alt="RBR-MP-Server logo" width="100%">

# RBR-MP-Server

<p>
  <em>The server half of a multiplayer mod for <strong>Richard Burns Rally</strong>:
  a small, dependency-free UDP relay in Go.</em>
</p>

It passes car state between players — pose, velocity, engine RPM, steering and
wheel speeds, and which car everyone drives — at a fixed tick rate over plain
UDP, with no authority and no physics: each client owns its own car and the
server only relays.

The client is a separate project: [RBR-MP-Client](../RBR-MP-Client) — a DLL
injected into the game that renders the other cars inside its own 3D scene,
wheels turning, shadows on the ground and engines audible where the cars are.

> A hobby project, not affiliated with or endorsed by the Richard Burns Rally
> team or RallySimFans.

---

## Quick start

```bash
go build ./cmd/rbrmp-server
./rbrmp-server                # listens on UDP :40100
```

Or with Docker, one command:

```bash
docker compose up -d
```

Open **UDP** port 40100 on the firewall — not TCP.

## Configuration

Everything is a flag; there is no config file to manage.

```
Usage of rbrmp-server:
  -addr string           UDP address to listen on (default ":40100")
  -tick int              snapshots per second sent to each client (default 30)
  -timeout duration      drop a client silent for this long (default 5s)
  -stale duration        stop relaying a player whose newest state is older
                         than this, so others despawn him quickly (default 2s)
  -stats duration        how often to print a traffic line, 0 = never (default 10s)
  -v                     log malformed datagrams and send errors
  -bans string           JSON file the ban list persists to (default "bans.json")
  -rcon-addr string      TCP address for the RCON admin console (default ":40101")
  -rcon-password string  RCON password; empty disables RCON
                         (env RBRMP_RCON_PASSWORD is the fallback)
```

Cross-compiling for a Linux box:

```bash
GOOS=linux GOARCH=amd64 go build -o rbrmp-server ./cmd/rbrmp-server
```

## Administration: RCON and the CLI

With a password set, the server opens a plain-TCP admin console (RCON) next to
the game port:

```bash
./rbrmp-server -rcon-password hunter2            # RCON on TCP :40101
RBRMP_RCON_PASSWORD=hunter2 ./rbrmp-server       # same, password via env
```

No password, no listener — RCON is off by default. The port is TCP, meant for
localhost or a firewalled admin IP, **not** for the open internet (see
[docs/DEPLOYMENT.md](docs/DEPLOYMENT.md)).

`clirbrmp` is the matching client. One-shot:

```bash
clirbrmp -password hunter2 players
clirbrmp -password hunter2 kick 3 flooding the chat
clirbrmp -addr my.vps:40101 -password hunter2 status
```

Or interactive — run it with no command and type at the `rbrmp>` prompt
(`help` lists everything, `exit` leaves). `-password` falls back to
`RBRMP_RCON_PASSWORD`, the same variable the server reads.

Commands: `players`, `kick <id|name> [reason...]`, `ban <id|name|ip>
[reason...]`, `unban <ip>`, `bans`, `say <text...>` (broadcast a chat line as
**SERVER**), `status`, `quit`.

**Kicks** delete the session, tell the client why (a `Kick` datagram with the
reason), and ignore the address for 10 s so the client's automatic re-register
doesn't put the player straight back.

**Bans** are by IP and persist to a JSON file (`-bans`, default `bans.json`),
written atomically on every change and loaded at startup — a restart forgets
nothing. A banned address has everything it sends dropped, and receives a
`Kick("banned: ...")` at most every 5 s so it knows why.

**Ping**: the server measures every client's real round trip with a
Ping/Pong exchange (every 2 s, its own clock, no tick-wait in the number) and
puts it in every snapshot — your own as `SelfPingMs`, everyone else's per
entity — so clients can show a proper ping column. `players` shows the same
number.

## Installation

* **[docs/INSTALL.md](docs/INSTALL.md)** — the "play tonight" guide: prebuilt
  binaries from the release pages, local/LAN/internet setup, client install,
  troubleshooting.

## Deployment

* **[Dockerfile](Dockerfile)** — multi-stage build ending on `scratch`: a
  single static binary, a few megabytes, nothing to patch, runs as non-root.
* **[docker-compose.yml](docker-compose.yml)** — one-command deployment with
  the UDP port mapped, a memory cap, a read-only filesystem and all
  capabilities dropped. Tune the server through its `command:` line.
* **[docs/DEPLOYMENT.md](docs/DEPLOYMENT.md)** — the longer walkthrough:
  VPS setup, firewall, running it under systemd instead, and what to monitor.
* Every `v*` tag also triggers CI to build the release binaries and, on the
  server's own VPS, download and restart the service automatically — see
  [.github/workflows/release.yml](.github/workflows/release.yml).

## Design

**Self-hosted, not central.** The binary is meant to be run by whoever wants a
session, in the style of SAMP/MTA/FiveM — not as one server everyone connects
to. A single central server would need throughput nobody has volunteered, and
peer-to-peer would generate more support questions than races.

**No authority, no simulation.** Every client owns its own car; the server
stores what it is told and forwards it. Physics stay in the game, where they
belong. That also means there is nothing here to cheat *through* — and nothing
stopping a client from lying about where it is, which is fine for driving with
friends and not fine for a leaderboard.

**Clocks are never synchronised.** Clients timestamp their own packets with
their own clock; the server stores that number, drives everything off its own
receive clock, and hands the client's number back untouched. Each side can then
measure real latency using only the clock it owns.

**Dumb wire, smart edges.** The protocol is fixed-layout little-endian UDP with
no reliability layer: state is continuous, so a lost packet is simply replaced
by the next one. Interpolation, extrapolation and animation are the client's
job; the server only relays and replays.

## Measuring the delay without the game

`cmd/rbrmp-sim` is a fake client. It drives a circle, sends full v2 telemetry
exactly like the mod does (wheels turning, revs rising and falling — so a real
client's animation and audio can be tested against it), and reports the round
trip the server hands back in every snapshot:

```
$ ./rbrmp-server -tick 60 &
$ ./rbrmp-sim -for 5s
joined as player 1
round trip 8.7 ms over 60 snapshots

300 snapshots: average round trip 8.6 ms
```

**round trip** is how stale the server's picture of you is: the trip out, plus
however long your state waited for the next server tick, plus the trip back.
On loopback the tick wait is most of it, so this is not a ping.

There is no self-echo/replay player — an earlier version of the server fed
each client's own state back to it on a delay so the loop could be watched
with one machine, but it added enough complexity for what it bought that it
was removed (`Welcome.EchoDelayMs` still exists on the wire and is always `0`,
kept only so old and new peers don't reject each other's handshake). To see a
second car without a second PC, run a second `rbrmp-sim` instead.

`-rate`, `-speed`, `-radius` and `-car` change what the fake car does and
claims to be. Several instances can run at once to simulate a field of cars —
which is also how to test with company but no second machine.

## Protocol

Version 3: fixed-layout little-endian UDP, documented field-by-field in
**[docs/PROTOCOL.md](docs/PROTOCOL.md)**. The reference implementation is
`internal/protocol`, mirrored by the client's `src/net_protocol.cpp`.

A state packet is 112 bytes — about 6.7 kB/s per client at 60 Hz. A snapshot is
20 bytes plus 156 per entity. Version 3 added server-measured ping (Ping/Pong,
carried per entity and per client in every snapshot) and the admin Kick;
version 2 added car identity and a telemetry block (velocity, RPM, steering,
gear, per-wheel angular velocity); version 1 carried pose and speed only.

## Repository layout

```
cmd/rbrmp-server/   the binary: flags, signals, wiring
cmd/rbrmp-sim/      fake game client for testing and measurement
cmd/clirbrmp/       admin CLI: one-shot or interactive RCON client
internal/protocol/  the wire format: encode/decode, no I/O
internal/server/    the relay: sessions, tick loop, pose history, RCON, bans
docs/               INSTALL.md, PROTOCOL.md, DEPLOYMENT.md
```

## Tests

```bash
go test ./...
```

The protocol tests are round trips and truncation checks. The server tests
stand up a real server on a real socket and drive it over UDP: joining,
relaying between clients, timeouts, and RTT measurement.

`go test -race` needs cgo and a C compiler; it has not been run on the machine
this was written on, so the concurrency has been reviewed by hand rather than
by tool. All shared state is behind one mutex, and the only fields read outside
it are set before a client is published into the map.

## Status and roadmap

Working: joining, relaying between any number of players, car identity, full
telemetry pass-through (wheels, RPM, steering, gear, velocity), timeouts,
server-measured ping in every snapshot, chat, kick and persistent IP bans,
RCON admin console with a CLI, traffic stats, the simulator, Docker deployment.

Not done yet:

- [ ] Stage identity, so players only see others on the same stage
- [ ] Spectator mode
- [ ] A server list, so sessions can be found rather than typed in
- [ ] Load testing — the tick loop builds and sends one snapshot per client
      per tick, which is O(clients²) work and the first thing that will hurt

## License

MIT — see [LICENSE](LICENSE).
