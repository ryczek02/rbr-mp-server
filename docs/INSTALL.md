# Running a local server

This is the "play tonight" guide: get a server running on your own PC, get the
client into the game, and drive. For putting a server on a VPS see
[DEPLOYMENT.md](DEPLOYMENT.md); for the wire format see
[PROTOCOL.md](PROTOCOL.md).

Nothing here requires Go, Docker or a compiler — everything is downloaded
prebuilt from the release pages.

## What you need

* Richard Burns Rally (an RSF install works fine).
* **RBRMPClient.dll** — from the client's
  [latest release](https://github.com/ryczek02/rbr-mp-client/releases/latest).
* **rbrmp-server-windows-amd64.exe** — from the server's
  [latest release](https://github.com/ryczek02/rbr-mp-server/releases/latest).

## 1. Start the server

Put `rbrmp-server-windows-amd64.exe` anywhere and double-click it, or run it
from a terminal:

```
rbrmp-server-windows-amd64.exe
```

That is a complete server: it listens on **UDP port 40100** and sends 30
snapshots per second to everyone connected.

The first launch will pop the Windows Defender Firewall dialog; allow access
(private networks is enough for LAN play). Keep the window open — closing it
stops the server. It prints a traffic line every 10 s so you can see clients
come and go.

Useful flags:

```
-addr string      UDP address to listen on (default ":40100")
-tick int         snapshots per second sent to each client (default 30)
-timeout duration drop a client silent for this long (default 5s)
-stats duration   how often to print a traffic line, 0 = never (default 10s)
-v                log malformed datagrams and send errors
```

## 2. Install the client

Copy `RBRMPClient.dll` into the game's **`Plugins\`** folder — the same place
RSF's own plugins live, e.g.:

```
C:\Games\Richard Burns Rally\Plugins\RBRMPClient.dll
```

That's the whole install. The DLL is a native RBR plugin, so the game loads it
by itself at startup; there is no injector or launcher to run.

## 3. Connect and drive

1. Start RBR.
2. Open the RBR-MP panel in game (Multiplayer tab).
3. The server address defaults to `127.0.0.1:40100` — correct for a server on
   the same PC. Type a name and tick **Connect**.
4. Load a stage and drive. Testing alone? `rbrmp-sim` from the server
   release simulates another player driving a circle around the start —
   run `rbrmp-sim-windows-amd64.exe` on the same PC and its car should
   appear on your stage.

## Running the server on macOS (Docker)

The server itself is Windows/Linux, but any Mac with
[Docker Desktop](https://www.docker.com/products/docker-desktop/) (or OrbStack)
can host it — the image builds natively on Apple Silicon and Intel alike:

```bash
git clone https://github.com/ryczek02/rbr-mp-server
cd rbr-mp-server
docker compose up -d
```

That builds the image and starts the server with **UDP 40100 published on
`0.0.0.0`** — meaning every interface at once: `127.0.0.1` for anything on the
Mac itself, and the Mac's Wi-Fi address for the gaming PCs on the same
network. macOS does not firewall Docker's published ports by default, so there
is nothing else to open.

Point the game clients at the Mac's LAN address, which you get with:

```bash
ipconfig getifaddr en0        # e.g. 192.168.1.42 -> connect to 192.168.1.42:40100
```

Watching it work, and stopping it:

```bash
docker compose logs -f        # the traffic line shows clients joining
docker compose down
```

Tuning goes through the `command:` line in `docker-compose.yml` (all the flags
above); after editing it, `docker compose up -d` again to apply.

## Playing over LAN

Everyone installs the DLL (step 2). One player runs the server and the others
connect to that PC:

1. The host finds their LAN address: `ipconfig` → IPv4 Address, e.g.
   `192.168.1.23`.
2. The host makes sure **UDP 40100** is allowed inbound in Windows Defender
   Firewall (the dialog from step 1 usually already did this).
3. Everyone else enters `192.168.1.23:40100` in the panel and connects.

## Playing over the internet from home

Same as LAN, but the host also forwards **UDP 40100** on their router to their
PC, and friends connect to the host's public IP (`whatsmyip` etc.). If your
ISP uses CGNAT, port forwarding won't work — put the server on a cheap VPS
instead, which is exactly what [DEPLOYMENT.md](DEPLOYMENT.md) covers.

## Troubleshooting

* **No panel in game** — the DLL isn't loading. Check it is really in
  `Plugins\` (not `Plugins\RBRMPClient\`), and that you downloaded the DLL
  from a release (it must be the 32-bit build; the release one is).
* **Panel connects to nothing / no ghost appears** — server not reachable.
  Confirm the server window shows your client in its stats line; if not, it's
  the firewall or the address. Remember the port is **UDP**, not TCP — a TCP
  port-checker website saying "closed" means nothing.
* **Ghost appears but stutters** — look at the server's stats output and the
  Debug tab in the panel; packet loss on Wi-Fi is the usual suspect.
* **Two copies of RBR on one PC** does not work for testing — the game
  refuses to run twice. Use `rbrmp-sim` as the second player instead.
