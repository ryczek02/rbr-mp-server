// Command rbrmp-sim is a fake game client: it drives a car in a circle, sends
// it to the server exactly like the mod does, and reports what comes back.
//
// It exists so the server can be tested - and the delay measured - without
// starting Richard Burns Rally at all. Run the server in one terminal and this
// in another:
//
//	rbrmp-server
//	rbrmp-sim
//
// It prints a line a second: the round trip time, and how far behind the echo
// player really is, both in milliseconds and in metres of track.
package main

import (
	"flag"
	"fmt"
	"math"
	"net"
	"os"
	"time"

	"github.com/lukaszryczko/rbr-mp-server/internal/protocol"
)

func main() {
	addr := flag.String("server", "127.0.0.1:40100", "server address")
	name := flag.String("name", "sim", "player name")
	car := flag.String("car", "XSARA", "car folder name announced to the server")
	rate := flag.Int("rate", 60, "state packets per second")
	speed := flag.Float64("speed", 20, "m/s the fake car drives at")
	radius := flag.Float64("radius", 50, "radius of the circle it drives, metres")
	at := flag.String("at", "0,0,0",
		"world position the circle starts at, metres, \"x,y,z\" - read your own\n"+
			"position off the client panel (Debug > Car physics readout) to have\n"+
			"the fake car drive right where you are standing")
	run := flag.Duration("for", 0, "stop after this long (0 = until Ctrl-C)")
	quiet := flag.Bool("q", false, "only print the summary")
	flag.Parse()

	var origin [3]float32
	if n, err := fmt.Sscanf(*at, "%f,%f,%f", &origin[0], &origin[1], &origin[2]); n != 3 || err != nil {
		fmt.Fprintln(os.Stderr, `-at must be "x,y,z", e.g. -at "412.3,-88.1,102.5"`)
		os.Exit(2)
	}

	conn, err := net.Dial("udp", *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer conn.Close()

	if _, err := conn.Write(protocol.EncodeHello(protocol.Hello{Name: *name, Car: *car})); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	start := time.Now()
	clientMs := func() uint32 { return uint32(time.Since(start).Milliseconds()) }

	// Receiving runs on its own goroutine; the main loop keeps a steady send
	// cadence, which is what the game does too.
	type report struct {
		rttMs      int64
		echoAgeMs  int64
		echoLagM   float64
		others     int
		haveEcho   bool
		playerID   uint32
		echoDelay  uint16
		gotWelcome bool
	}
	reports := make(chan report, 64)

	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			msgType, err := protocol.ParseHeader(buf[:n])
			if err != nil {
				continue
			}
			switch msgType {
			case protocol.TypeWelcome:
				w, err := protocol.DecodeWelcome(buf[:n])
				if err != nil {
					continue
				}
				reports <- report{gotWelcome: true, playerID: w.PlayerID, echoDelay: w.EchoDelayMs}

			case protocol.TypeSnapshot:
				s, err := protocol.DecodeSnapshot(buf[:n])
				if err != nil {
					continue
				}
				now := int64(clientMs())
				r := report{rttMs: now - int64(s.LastClientTimeMs)}
				for _, e := range s.Entities {
					if e.Flags&protocol.FlagEcho != 0 {
						r.haveEcho = true
						r.echoAgeMs = now - int64(e.SampleTimeMs)
						// Where we were when that sample was taken, versus
						// where we are now: the lag expressed as track.
						nowPos := carAt(time.Since(start), *speed, *radius, origin)
						r.echoLagM = dist(nowPos, e.Transform.Pos)
					} else {
						r.others++
					}
				}
				reports <- r
			}
		}
	}()

	send := time.NewTicker(time.Second / time.Duration(*rate))
	defer send.Stop()
	print := time.NewTicker(time.Second)
	defer print.Stop()

	var seq uint32
	var sumRtt, sumAge, sumLag float64
	var samples, echoSamples int
	var totalRtt, totalAge float64
	var totalSamples, totalEchoSamples int
	var playerID uint32
	var echoDelay uint16
	deadline := time.Time{}
	if *run > 0 {
		deadline = start.Add(*run)
	}

	for {
		select {
		case <-send.C:
			if !deadline.IsZero() && time.Now().After(deadline) {
				summarise(totalRtt, totalAge, totalSamples, totalEchoSamples, echoDelay)
				return
			}
			seq++
			pos := carAt(time.Since(start), *speed, *radius, origin)
			conn.Write(protocol.EncodeState(protocol.State{
				PlayerID:     playerID,
				Seq:          seq,
				ClientTimeMs: clientMs(),
				Transform: protocol.Transform{
					Pos: pos,
					Rot: headingAt(time.Since(start), *speed, *radius),
				},
				Telemetry: telemetryAt(time.Since(start), *speed, *radius),
			}))

		case r := <-reports:
			if r.gotWelcome {
				playerID, echoDelay = r.playerID, r.echoDelay
				if !*quiet {
					fmt.Printf("joined as player %d; server echo delay is %d ms\n",
						playerID, echoDelay)
				}
				continue
			}
			samples++
			sumRtt += float64(r.rttMs)
			totalRtt += float64(r.rttMs)
			totalSamples++
			if r.haveEcho {
				echoSamples++
				totalEchoSamples++
				sumAge += float64(r.echoAgeMs)
				sumLag += r.echoLagM
				totalAge += float64(r.echoAgeMs)
			}

		case <-print.C:
			if *quiet || samples == 0 {
				samples, echoSamples, sumRtt, sumAge, sumLag = 0, 0, 0, 0, 0
				continue
			}
			// This is the round trip PLUS however long the state sat waiting
			// for the next server tick, which on loopback is most of it. That
			// is the number that actually matters - it is how stale the
			// server's idea of you is - but it is not a ping.
			line := fmt.Sprintf("round trip %.1f ms over %d snapshots",
				sumRtt/float64(samples), samples)
			if echoSamples > 0 {
				line += fmt.Sprintf("   echo %.0f ms behind (%.1f m of track)",
					sumAge/float64(echoSamples), sumLag/float64(echoSamples))
			} else {
				line += "   echo: not visible yet"
			}
			fmt.Println(line)
			samples, echoSamples, sumRtt, sumAge, sumLag = 0, 0, 0, 0, 0
		}
	}
}

func summarise(totalRtt, totalAge float64, n, echoN int, echoDelay uint16) {
	if n == 0 {
		fmt.Println("no snapshots received - is the server running?")
		return
	}
	fmt.Printf("\n%d snapshots: average round trip %.1f ms\n", n, totalRtt/float64(n))
	if echoN == 0 {
		fmt.Println("the echo never appeared - run for longer than the echo delay")
		return
	}
	// Averaged over the snapshots that HAD an echo. Dividing by every snapshot
	// would fold in the first `delay` of the run, when there is nothing to
	// replay yet, and quietly report a delay shorter than the configured one.
	fmt.Printf("%d of them carried the echo: average age %.0f ms (server was set to %d ms)\n",
		echoN, totalAge/float64(echoN), echoDelay)
}

// carAt is where the fake car is after t: a circle at constant speed starting
// at `origin`, which exercises interpolation far better than a straight line
// would. Pass the position of a real stage spot via -at and the car drives
// there, visible from the game.
func carAt(t time.Duration, speed, radius float64, origin [3]float32) [3]float32 {
	if radius <= 0 {
		return [3]float32{origin[0] + float32(speed*t.Seconds()), origin[1], origin[2]}
	}
	a := speed * t.Seconds() / radius // radians travelled
	return [3]float32{
		origin[0] + float32(radius*math.Sin(a)),
		origin[1] + float32(radius*(1-math.Cos(a))),
		origin[2],
	}
}

// headingAt is the car's orientation on that circle, in the same layout the
// game publishes: rows are the car's local +X, +Y and +Z as world directions.
// Forward is local -Y, so row 1 is the reverse of the direction of travel.
func headingAt(t time.Duration, speed, radius float64) [9]float32 {
	if radius <= 0 {
		return [9]float32{0, -1, 0, -1, 0, 0, 0, 0, 1}
	}
	a := speed * t.Seconds() / radius
	fx, fy := math.Cos(a), math.Sin(a) // tangent to the circle
	// left = up x forward, with up = +Z
	lx, ly := -fy, fx
	return [9]float32{
		float32(lx), float32(ly), 0,
		float32(-fx), float32(-fy), 0,
		0, 0, 1,
	}
}

// telemetryAt fakes the rest of a State so a client can be seen animating:
// velocity tangent to the circle, wheels rolling at speed/radius, a constant
// steering hold, and revs that rise and fall so engine audio has something to
// track.
func telemetryAt(t time.Duration, speed, radius float64) protocol.Telemetry {
	const wheelRadius = 0.31 // metres, a typical rally tyre
	omega := float32(speed / wheelRadius)

	var vx, vy float64
	if radius <= 0 {
		vx, vy = speed, 0
	} else {
		a := speed * t.Seconds() / radius
		vx, vy = speed*math.Cos(a), speed*math.Sin(a)
	}

	steer := float32(0)
	if radius > 0 {
		steer = -0.25 // holding a constant left turn
	}
	return protocol.Telemetry{
		Vel:        [3]float32{float32(vx), float32(vy), 0},
		Speed:      float32(speed),
		RPM:        float32(4500 + 1500*math.Sin(t.Seconds()*2)),
		Steer:      steer,
		Gear:       4,
		WheelOmega: [4]float32{omega, omega, omega, omega},
	}
}

func dist(a, b [3]float32) float64 {
	dx := float64(a[0] - b[0])
	dy := float64(a[1] - b[1])
	dz := float64(a[2] - b[2])
	return math.Sqrt(dx*dx + dy*dy + dz*dz)
}
