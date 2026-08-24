# RBR-MP Server — Roadmap

The client-side roadmap lives in the client repo
([RBR-MP-Client/ROADMAP.md](../RBR-MP-Client/ROADMAP.md)). The server stays
what it is today — a small, stdlib-only Go relay that is authoritative about
identity and time — and grows session logic, not game logic.

## Done

- Protocol v3: fixed-layout UDP relay (Hello/Welcome/State/Snapshot/Bye),
  chat broadcast, server-measured **ping** (Ping/Pong) published per entity,
  Kick message.
- Admin: kick, IP bans persisted in `bans.json`, RCON over TCP with password,
  `clirbrmp` CLI (one-shot + interactive), `say` server broadcasts.
- Deployment: Docker, Ansible + systemd, GitHub Actions release cross-builds.

## Phase 1 — operations quality

- [ ] Structured log option (`-log json`) so hosted servers can ship logs.
- [ ] RCON: `mute <id> <duration>` (drops chat only) and `pardon`-style
      temporary bans with expiry in `bans.json`.
- [ ] Rate limiting beyond chat: per-address packet budget, oversized-datagram
      drop counters in `status`.
- [ ] Persistent player identity: client-generated GUID in `Hello` (protocol
      v4), so bans survive IP changes and names can be reserved.

## Phase 2 — race sessions (protocol v4)

- [ ] Stage identity in `State`/`Entity`; snapshots filtered per stage.
- [ ] Session object on the server: stage, car class, state machine
      (lobby → countdown → running → results), all RCON-controllable
      (`session start <stage>`, `session abort`).
- [ ] Synchronized start: server broadcasts the countdown against its own
      clock; late joiners become spectators of the running session.
- [ ] Timing: clients report splits/finish; the server owns the results table
      and broadcasts it; results archived as JSON per session.

## Phase 3 — web panel

- [ ] Read-only HTTP endpoint (localhost by default, same auth story as RCON):
      players, pings, chat log, session state as JSON.
- [ ] Small embedded web UI on top of it (no external deps — `embed`ed static
      files): live player table, chat view, kick/ban buttons behind the RCON
      password.
- [ ] Webhooks (Discord-compatible) for join/leave/session results.

## Phase 4 — master server

- [ ] A separate tiny service (same repo, `cmd/rbrmp-master`): servers
      announce themselves (name, address, players, stage, version) over HTTP
      with a heartbeat; the list is served as JSON.
- [ ] `rbrmp-server -announce <master-url>` opt-in flag.
- [ ] Client-facing list endpoint with server-side ping expectations
      (region tag), pagination, and stale-entry expiry.

## Non-goals

- Running game logic (physics, damage) server-side — the server relays and
  arbitrates, the game simulates.
- Accounts/passwords for players; identity stays GUID + name + bans.
- TCP for game state; the state path stays fire-and-forget UDP.
