# RBR-MP wire protocol, version 3

UDP, little-endian, fixed layout, one message per datagram. There is no framing
beyond the datagram itself and no reliability layer: state is sent continuously,
so a lost packet is replaced by the next one a few milliseconds later.

The layout is deliberately dull. The other end is C++ inside a 32-bit game
process, and the less parsing it has to do per frame the better.

`internal/protocol/protocol.go` is the implementation (mirrored by the client's
`src/net_protocol.cpp`); if they ever disagree with this document, the code is
right and this document is stale.

## What changed since version 2

Version 3 adds server-side ping measurement and an administrative kick:

* **Ping / Pong (7/8)** — the server sends every client a `Ping` with an
  opaque token (its own monotonic clock, roughly every 2 s); the client echoes
  it in a `Pong`. The round trip is measured entirely on the server, with one
  clock and no tick-wait in it — an actual ping, unlike the snapshot's echoed
  client time.
* **Per-entity ping** — every `Entity` grew a trailing `u16` ping (plus 2
  bytes of padding), so each client can show everyone else's latency. Entity
  size is now **156** bytes (was 152).
* **Self ping** — the snapshot header's padding `u16` at offset 18 is now the
  receiving client's own server-measured round trip.
* **Kick (9)** — the server can remove a player (RCON `kick`/`ban`) and tell
  them why. A **banned** address also receives a `Kick` (reason prefixed
  `banned: `, re-sent at most every 5 s) while everything it sends is dropped.

## What changed since version 1

Version 2 exists so a remote car can be a *car* instead of a gliding statue:

* **Car identity** — `Hello` and every `Entity` carry the `Cars\<folder>` name
  the player drives, so each remote player is drawn in their actual car.
* **Telemetry** — `State` and `Entity` carry a 44-byte telemetry block:
  world velocity (drives dead-reckoning extrapolation on the receiving side),
  speed, engine RPM (drives positional engine audio), steering input and the
  four wheels' angular velocities (drive wheel steer/spin animation), and the
  gear.

A version-1 datagram is rejected by a version-2 peer and vice versa; both sides
show "nothing arrives", which is the correct failure for a lockstep format.

## Common header

Every datagram starts with 8 bytes:

| Offset | Type | Field | Value |
|---|---|---|---|
| 0 | `u32` | magic | `0x504D524F`, which reads as `ORMP` in a hex dump |
| 4 | `u16` | version | `3` |
| 6 | `u16` | type | see below |

| Type | Name | Direction |
|---|---|---|
| 1 | Hello | client → server |
| 2 | Welcome | server → client |
| 3 | State | client → server |
| 4 | Snapshot | server → client |
| 5 | Bye | client → server |
| 6 | Chat | both |
| 7 | Ping | server → client |
| 8 | Pong | client → server |
| 9 | Kick | server → client |

A datagram with the wrong magic or version is dropped without a reply.

## Shared types

**Name** — 24 bytes, NUL-padded. Anything longer is truncated and still
NUL-terminated. Car identity fields use the same layout.

**Transform** — 48 bytes: a position and an orientation.

| Offset | Type | Field |
|---|---|---|
| 0 | `f32[3]` | position X, Y, Z |
| 12 | `f32[9]` | orientation, row-major |

Coordinates are **metres, Z-up, right-handed** — RBR's own physics frame,
unconverted. The orientation is the car's local axes as world direction
vectors, one axis per row: row 0 is local +X (left), row 1 local +Y
(backwards; forward is −Y), row 2 local +Z (up). The position is the car's
**centre of mass** (what RBR's telemetry publishes).

The orientation is sent as a 3×3 rather than a quaternion on purpose: it is
exactly what the client reads out of the game and exactly what it renders with,
so a round trip through the server cannot introduce a conversion error.

**Telemetry** — 44 bytes: what the car is doing.

| Offset | Type | Field |
|---|---|---|
| 0 | `f32[3]` | world velocity, m/s |
| 12 | `f32` | speed, m/s |
| 16 | `f32` | engine RPM |
| 20 | `f32` | steering input, −1..+1, positive = right |
| 24 | `i32` | gear: 0 = reverse, 1 = neutral, 2.. = 1st.. |
| 28 | `f32[4]` | wheel angular velocity, rad/s, order **LF RF LB RB** |

The server never interprets any of it; it stores the newest sample per client
and passes it straight through to everyone else's next snapshot.

## Hello (1) — client → server

| Offset | Type | Field |
|---|---|---|
| 8 | `char[24]` | player name |
| 32 | `char[24]` | car: the `Cars\<folder>` name, may be empty |

Sent on connect and safe to repeat; the server answers every one with a
Welcome. A repeated Hello is also how the client announces a name or car
change. A client that only ever sends State is registered anyway (with no car),
so a server restart does not require the client to do anything.

## Welcome (2) — server → client

| Offset | Type | Field |
|---|---|---|
| 8 | `u32` | player id, ≥ 1 |
| 12 | `u16` | snapshot tick rate, Hz |
| 14 | `u16` | echo delay, ms — always `0` since the echo player was removed; the field stays for wire compatibility |
| 16 | `u32` | server uptime, ms |

## State (3) — client → server

| Offset | Type | Field |
|---|---|---|
| 8 | `u32` | player id (as assigned; ignored by the server, which keys on the address) |
| 12 | `u32` | sequence number, incrementing |
| 16 | `u32` | client time, ms |
| 20 | `Transform` | pose |
| 68 | `Telemetry` | telemetry |

112 bytes. At 60 Hz that is about 6.7 kB/s per client upstream, before UDP and
IP headers.

**Sequence number.** UDP reorders, and a sample older than the newest one held
would corrupt the replay timeline, so the server drops anything that does not
advance the sequence. A jump backwards of more than 1000 is read as a client
restart and clears that client's history instead.

**Client time.** The server never interprets this value. It stores it, hands it
back, and drives everything else off its own receive clock — so the two clocks
never have to be synchronised, and the client can still measure the true age of
anything it receives using only its own clock.

## Snapshot (4) — server → client

| Offset | Type | Field |
|---|---|---|
| 8 | `u32` | server uptime, ms |
| 12 | `u32` | the last client time this client sent, echoed back |
| 16 | `u16` | entity count |
| 18 | `u16` | **self ping**: the receiving client's own server-measured round trip, ms, clamped to 65535; 0 = not yet measured (was padding in version 2) |
| 20 | `Entity[]` | the entities |

`now − echoed client time` is the **round trip**, measured with one clock and no
synchronisation. Note what it includes: the trip out, however long that state
sat waiting for the next server tick, and the trip back. On a LAN the tick wait
is the largest part of it — at 30 Hz it averages ~17 ms — so this number is
"how stale the server's picture of me is", not a ping. The Ping/Pong RTT at
offset 18 (and per entity below) has no tick wait in it; that one *is* a ping.

### Entity — 156 bytes each

| Offset | Type | Field |
|---|---|---|
| 0 | `u32` | id |
| 4 | `u16` | flags |
| 6 | `u16` | padding |
| 8 | `char[24]` | name |
| 32 | `char[24]` | car; empty = unknown, the receiver picks a fallback model |
| 56 | `Transform` | pose |
| 104 | `Telemetry` | telemetry |
| 148 | `u32` | the client time of the sample this pose came from |
| 152 | `u16` | this player's server-measured round trip, ms, clamped to 65535; 0 = not yet measured |
| 154 | `u16` | padding (zero), keeps the array 4-byte aligned |

| Flag | Meaning |
|---|---|
| `0x0001` | echo — reserved, unused by the reference server (see below) |

The echo flag and the id-high-bit scheme it implies (`id = player id \|
0x80000000`, so an echo entity could never collide with a real player's id)
are wire-format leftovers from a self-echo/replay feature: the server used to
feed each client's own state back to it on a delay, so the whole loop —
latency, interpolation, how a remote car actually looks — could be measured
and watched with one machine. It was removed for being more complexity than
it was worth (see `internal/server/server.go`, which never sets `FlagEcho`);
`Welcome.EchoDelayMs` is hard-coded to `0` for the same reason. The flag bit
stays defined, unused, so the version field doesn't have to bump again if a
similar feature comes back.

A client never appears in its own snapshot.

## Bye (5) — client → server

Header only. The server forgets the client immediately; without it, the client
is dropped after the timeout (5 s by default).

## Chat (6) — both directions

One line of text. The same layout travels both ways:

| offset | size | type | field |
|---|---|---|---|
| 8  | 4   | u32 | playerId |
| 12 | 24  | chars | name, NUL-padded |
| 36 | 128 | chars | text, NUL-padded (`ChatTextLen`) |

A client sends it with whatever id/name it has; the server **overwrites both
from the session it knows** and rebroadcasts the line to every connected
client — the sender included, so a message appears for its author exactly when
everyone else sees it (no local echo path in the client). The server drops
empty lines, lines from addresses without a session, and anything faster than
one line per 300 ms per client. Like everything else here it is fire-and-forget
UDP: a lost chat line is simply lost.

Join/leave notices are NOT a message type: clients derive them from entity ids
appearing in and disappearing from snapshots.

## Ping (7) — server → client

| Offset | Type | Field |
|---|---|---|
| 8 | `u32` | token — opaque to the client (the server uses its own monotonic ms) |

12 bytes. Sent to every client roughly every 2 s; an unanswered ping is
written off as lost after 2 s and resent. The client's only job is to echo the
token back in a Pong, immediately and verbatim. It must not interpret the
token.

## Pong (8) — client → server

| Offset | Type | Field |
|---|---|---|
| 8 | `u32` | token, echoed verbatim from the Ping |

12 bytes. On a matching token from a known address, the server records
`now − ping sent` as that client's round trip, which then appears in every
snapshot as `SelfPingMs` (for the client itself) and `Entity.PingMs` (for
everyone else). Because the token is the server's clock, the measurement needs
no synchronisation and cannot be improved by a lying client beyond making its
own ping look worse.

## Kick (9) — server → client

| Offset | Type | Field |
|---|---|---|
| 8 | `char[64]` | reason, NUL-padded; long reasons are truncated and still NUL-terminated |

72 bytes. Sent when an admin kicks or bans the player (the datagram is
fire-and-forget UDP, so the server sends it three times back-to-back), and the
session is deleted either way. For 10 s afterwards the address's Hello/State
are silently dropped, so the client's automatic re-register does not put the
player straight back; a kicked client should stop sending and tell the user
the reason.

A **banned** address is different: every datagram it sends is dropped before
any decode, and at most once per 5 s the server replies with a Kick whose
reason is prefixed `banned: ` — so a banned client learns why it hears
nothing, without being able to make the server chatty.

## Not in version 3

Stage identity (you see every player on the server, whichever stage they are
on), damage, lap and timing data, reliability or ordering for anything, and
any form of authentication. The version field exists so these can be added
without guessing.
