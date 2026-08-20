package server

import (
	"io"
	"log"
	"net"
	"testing"
	"time"

	"github.com/lukaszryczko/rbr-mp-server/internal/protocol"
)

// start brings up a server on a free port and returns it with a dialled client.
func start(t *testing.T, cfg Config) (*Server, func()) {
	t.Helper()
	cfg.Addr = "127.0.0.1:0"
	cfg.Stats = 0
	logger := log.New(io.Discard, "", 0)
	srv, err := New(cfg, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	go srv.Run()
	return srv, func() { srv.Close() }
}

func dial(t *testing.T, srv *Server) *net.UDPConn {
	t.Helper()
	conn, err := net.DialUDP("udp", nil, srv.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// readSnapshot waits for the next snapshot, ignoring anything else.
func readSnapshot(t *testing.T, conn *net.UDPConn, within time.Duration) (protocol.Snapshot, bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(deadline)
		n, err := conn.Read(buf)
		if err != nil {
			return protocol.Snapshot{}, false
		}
		msgType, err := protocol.ParseHeader(buf[:n])
		if err != nil || msgType != protocol.TypeSnapshot {
			continue
		}
		s, err := protocol.DecodeSnapshot(buf[:n])
		if err != nil {
			t.Fatalf("decode snapshot: %v", err)
		}
		return s, true
	}
	return protocol.Snapshot{}, false
}

func stateAt(id uint32, seq uint32, clientMs uint32, x float32) protocol.State {
	return protocol.State{
		PlayerID:     id,
		Seq:          seq,
		ClientTimeMs: clientMs,
		Transform: protocol.Transform{
			Pos: [3]float32{x, 0, 0},
			Rot: [9]float32{1, 0, 0, 0, 1, 0, 0, 0, 1},
		},
		Telemetry: protocol.Telemetry{Speed: 10},
	}
}

func TestHelloGetsWelcome(t *testing.T) {
	srv, stop := start(t, Config{TickAt: 10 * time.Millisecond,
		Timeout: time.Second})
	defer stop()
	conn := dial(t, srv)

	conn.Write(protocol.EncodeHello(protocol.Hello{Name: "tester"}))
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("no reply: %v", err)
	}
	msgType, err := protocol.ParseHeader(buf[:n])
	if err != nil || msgType != protocol.TypeWelcome {
		t.Fatalf("first reply is type %d (%v), want Welcome", msgType, err)
	}
	w, err := protocol.DecodeWelcome(buf[:n])
	if err != nil {
		t.Fatalf("decode welcome: %v", err)
	}
	if w.PlayerID == 0 {
		t.Error("player id 0 was handed out; ids should start at 1")
	}
	if w.TickRateHz != 100 {
		t.Errorf("welcome says %d Hz, want 100", w.TickRateHz)
	}
}

// A lone player gets empty snapshots: there is no echo player, and nobody
// else to relay.
func TestLonePlayerGetsNoEntities(t *testing.T) {
	srv, stop := start(t, Config{TickAt: 10 * time.Millisecond, Timeout: 2 * time.Second})
	defer stop()
	conn := dial(t, srv)

	conn.Write(protocol.EncodeHello(protocol.Hello{Name: "tester"}))
	start := time.Now()
	var seq uint32
	for time.Since(start) < 300*time.Millisecond {
		seq++
		conn.Write(protocol.EncodeState(stateAt(1, seq, uint32(time.Since(start).Milliseconds()), 0)))
		if s, ok := readSnapshot(t, conn, 10*time.Millisecond); ok {
			if len(s.Entities) != 0 {
				t.Fatalf("got %d entities for a lone player, want none", len(s.Entities))
			}
		}
	}
}

// Two real clients must see each other.
func TestTwoClientsSeeEachOther(t *testing.T) {
	srv, stop := start(t, Config{TickAt: 10 * time.Millisecond, Timeout: 2 * time.Second})
	defer stop()
	a, b := dial(t, srv), dial(t, srv)

	a.Write(protocol.EncodeHello(protocol.Hello{Name: "alice"}))
	b.Write(protocol.EncodeHello(protocol.Hello{Name: "bob"}))

	deadline := time.Now().Add(2 * time.Second)
	var seq uint32
	for time.Now().Before(deadline) {
		seq++
		a.Write(protocol.EncodeState(stateAt(1, seq, uint32(seq*10), 100)))
		b.Write(protocol.EncodeState(stateAt(2, seq, uint32(seq*10), 200)))

		s, ok := readSnapshot(t, a, 20*time.Millisecond)
		if !ok {
			continue
		}
		for _, e := range s.Entities {
			if e.Name == "bob" {
				if e.Transform.Pos[0] != 200 {
					t.Fatalf("bob is at x=%v, want 200", e.Transform.Pos[0])
				}
				return // alice saw bob where bob said he was
			}
		}
	}
	t.Fatal("alice never saw bob")
}

func TestClientTimeIsEchoedForRttMeasurement(t *testing.T) {
	srv, stop := start(t, Config{TickAt: 10 * time.Millisecond, Timeout: 2 * time.Second})
	defer stop()
	conn := dial(t, srv)

	conn.Write(protocol.EncodeHello(protocol.Hello{Name: "tester"}))
	const stamp = 123456
	deadline := time.Now().Add(time.Second)
	var seq uint32
	for time.Now().Before(deadline) {
		seq++
		conn.Write(protocol.EncodeState(stateAt(1, seq, stamp, 0)))
		if s, ok := readSnapshot(t, conn, 20*time.Millisecond); ok && s.LastClientTimeMs == stamp {
			return
		}
	}
	t.Fatal("the client's own timestamp never came back, so RTT cannot be measured")
}

func TestSilentClientIsDropped(t *testing.T) {
	const timeout = 200 * time.Millisecond
	srv, stop := start(t, Config{TickAt: 10 * time.Millisecond, Timeout: timeout})
	defer stop()
	conn := dial(t, srv)

	conn.Write(protocol.EncodeHello(protocol.Hello{Name: "tester"}))
	conn.Write(protocol.EncodeState(stateAt(1, 1, 0, 0)))
	time.Sleep(50 * time.Millisecond)

	srv.mu.Lock()
	n := len(srv.clients)
	srv.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d clients after a hello, want 1", n)
	}

	time.Sleep(timeout + 200*time.Millisecond)
	srv.mu.Lock()
	n = len(srv.clients)
	srv.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d clients after going silent for %v, want 0", n, timeout)
	}
}

func TestStateWithoutHelloStillRegisters(t *testing.T) {
	srv, stop := start(t, Config{TickAt: 10 * time.Millisecond, Timeout: time.Second})
	defer stop()
	conn := dial(t, srv)

	// No Hello at all: a client that was already running when the server
	// started should be picked up anyway.
	conn.Write(protocol.EncodeState(stateAt(1, 1, 0, 0)))
	time.Sleep(100 * time.Millisecond)

	srv.mu.Lock()
	n := len(srv.clients)
	srv.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d clients, want 1", n)
	}
}
