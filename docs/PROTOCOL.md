# RBR-MP wire protocol, version 1

UDP, little-endian, fixed layout, one message per datagram. There is no framing
beyond the datagram itself and no reliability layer: state is sent continuously,
so a lost packet is replaced by the next one a few milliseconds later.

The layout is deliberately dull. The other end is C++ inside a 32-bit game
process, and the less parsing it has to do per frame the better.

`internal/protocol/protocol.go` is the implementation; if the two ever disagree,
the code is right and this document is stale.

## Common header

Every datagram starts with 8 bytes:

| Offset | Type | Field | Value |
|---|---|---|---|
| 0 | `u32` | magic | `0x504D524F`, which reads as `ORMP` in a hex dump |
| 4 | `u16` | version | `1` |
| 6 | `u16` | type | see below |

| Type | Name | Direction |
|---|---|---|
| 1 | Hello | client → server |
| 2 | Welcome | server → client |
| 3 | State | client → server |
| 4 | Snapshot | server → client |
| 5 | Bye | client → server |

A datagram with the wrong magic or version is dropped without a reply.

## Shared types

**Name** — 24 bytes, NUL-padded. Anything longer is truncated and still
NUL-terminated.

**Transform** — 48 bytes: a position and an orientation.

| Offset | Type | Field |
|---|---|---|
| 0 | `f32[3]` | position X, Y, Z |
| 12 | `f32[9]` | orientation, row-major |

Coordinates are **metres, Z-up, right-handed** — RBR's own physics frame,
unconverted. The orientation is the car's local axes as world direction
vectors, one axis per row: row 0 is local +X (left), row 1 local +Y
(backwards; forward is −Y), row 2 local +Z (up). The car's origin is the rear
axle at ground level.

The orientation is sent as a 3×3 rather than a quaternion on purpose: it is
exactly what the client reads out of the game and exactly what it renders with,
so a round trip through the server cannot introduce a conversion error. That
matters when the point of the exercise is to measure the transport itself.

## Hello (1) — client → server

| Offset | Type | Field |
|---|---|---|
| 8 | `char[24]` | name |

Sent on connect and safe to repeat; the server answers every one with a
Welcome. A client that only ever sends State is registered anyway, so a server
restart does not require the client to do anything.

## Welcome (2) — server → client

| Offset | Type | Field |
|---|---|---|
| 8 | `u32` | player id, ≥ 1 |
| 12 | `u16` | snapshot tick rate, Hz |
| 14 | `u16` | echo delay, ms (`0` = the echo player is off) |
| 16 | `u32` | server uptime, ms |

## State (3) — client → server

| Offset | Type | Field |
|---|---|---|
| 8 | `u32` | player id (as assigned; ignored by the server, which keys on the address) |
| 12 | `u32` | sequence number, incrementing |
| 16 | `u32` | client time, ms |
| 20 | `Transform` | pose |
| 68 | `f32` | speed, m/s |

72 bytes. At 60 Hz that is about 4.3 kB/s per client upstream, before UDP and
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
| 18 | `u16` | padding (zero), keeps the array 4-byte aligned |
| 20 | `Entity[]` | the entities |

`now − echoed client time` is the **round trip**, measured with one clock and no
synchronisation. Note what it includes: the trip out, however long that state
sat waiting for the next server tick, and the trip back. On a LAN the tick wait
is the largest part of it — at 30 Hz it averages ~17 ms — so this number is
"how stale the server's picture of me is", not a ping.

### Entity — 88 bytes each

| Offset | Type | Field |
|---|---|---|
| 0 | `u32` | id |
| 4 | `u16` | flags |
| 6 | `u16` | padding |
| 8 | `char[24]` | name |
| 32 | `Transform` | pose |
| 80 | `f32` | speed, m/s |
| 84 | `u32` | the client time of the sample this pose came from |

| Flag | Meaning |
|---|---|
| `0x0001` | echo: this is the receiving client's own car, replayed on a delay — not another player |

An echo entity's **id has the high bit set** (`id = player id | 0x80000000`), so
it can never collide with a real player's id.

An echo entity's **sample time is on the receiving client's own clock**, so
`now − sample time` is the exact end-to-end delay of the whole loop: the
configured delay, plus the round trip, plus however long the server's tick and
the client's frame took to line up. That single number is what makes this
measurable rather than a feeling.

A client never appears in its own snapshot as a normal entity.

## Bye (5) — client → server

Header only. The server forgets the client immediately; without it, the client
is dropped after the timeout (5 s by default).

## Not in version 1

Which car each player drives (everyone is rendered with whatever model the
client picked), stage identity, wheel state, damage, lap and timing data,
reliability or ordering for anything, and any form of authentication. The
version field exists so these can be added without guessing.
