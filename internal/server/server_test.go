package server

import (
	"io"
	"log"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/lukaszryczko/rbr-mp-server/internal/protocol"
)

// start brings up a server on a free port and returns it with a dialled client.
func start(t *testing.T, cfg Config) (*Server, func()) {
	t.Helper()
	cfg.Addr = "127.0.0.1:0"
	cfg.Stats = 0
	if cfg.BansPath == "" {
		cfg.BansPath = filepath.Join(t.TempDir(), "bans.json")
	}
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

// readChat waits for the next chat line, ignoring anything else.
func readChat(t *testing.T, conn *net.UDPConn, within time.Duration) (protocol.Chat, bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(deadline)
		n, err := conn.Read(buf)
		if err != nil {
			return protocol.Chat{}, false
		}
		msgType, err := protocol.ParseHeader(buf[:n])
		if err != nil || msgType != protocol.TypeChat {
			continue
		}
		c, err := protocol.DecodeChat(buf[:n])
		if err != nil {
			t.Fatalf("decode chat: %v", err)
		}
		return c, true
	}
	return protocol.Chat{}, false
}

// A chat line reaches everyone - the sender included - stamped with the
// SERVER's identity for the sender, whatever the datagram claimed.
func TestChatIsRebroadcastToEveryoneWithServerIdentity(t *testing.T) {
	srv, stop := start(t, Config{TickAt: 10 * time.Millisecond, Timeout: 2 * time.Second})
	defer stop()
	a, b := dial(t, srv), dial(t, srv)

	a.Write(protocol.EncodeHello(protocol.Hello{Name: "alice"}))
	b.Write(protocol.EncodeHello(protocol.Hello{Name: "bob"}))
	time.Sleep(50 * time.Millisecond)

	// Alice lies about who she is; the server must not repeat the lie.
	a.Write(protocol.EncodeChat(protocol.Chat{PlayerID: 999, Name: "mallory", Text: "hello stage"}))

	for _, tc := range []struct {
		who  string
		conn *net.UDPConn
	}{{"bob", b}, {"alice (sender)", a}} {
		c, ok := readChat(t, tc.conn, 2*time.Second)
		if !ok {
			t.Fatalf("%s never received the chat line", tc.who)
		}
		if c.Text != "hello stage" {
			t.Fatalf("%s got text %q, want %q", tc.who, c.Text, "hello stage")
		}
		if c.Name != "alice" || c.PlayerID != 1 {
			t.Fatalf("%s got identity %d/%q, want 1/alice (server-stamped)", tc.who, c.PlayerID, c.Name)
		}
	}
}

// The flood guard: a burst collapses to what fits under one line per 300 ms.
func TestChatIsRateLimited(t *testing.T) {
	srv, stop := start(t, Config{TickAt: 10 * time.Millisecond, Timeout: 2 * time.Second})
	defer stop()
	a, b := dial(t, srv), dial(t, srv)

	a.Write(protocol.EncodeHello(protocol.Hello{Name: "alice"}))
	b.Write(protocol.EncodeHello(protocol.Hello{Name: "bob"}))
	time.Sleep(50 * time.Millisecond)

	for i := 0; i < 10; i++ {
		a.Write(protocol.EncodeChat(protocol.Chat{Text: "spam"}))
	}
	got := 0
	for {
		if _, ok := readChat(t, b, 400*time.Millisecond); !ok {
			break
		}
		got++
	}
	if got > 2 {
		t.Fatalf("a 10-line burst delivered %d lines, want at most 2", got)
	}
	if got == 0 {
		t.Fatal("the burst delivered nothing; the first line should pass")
	}
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

// answerPings echoes every Ping the connection sees back as a Pong, in the
// background, so tests that only care about the result can just wait.
func answerPings(t *testing.T, conn *net.UDPConn, done <-chan struct{}) {
	t.Helper()
	go func() {
		buf := make([]byte, 4096)
		for {
			select {
			case <-done:
				return
			default:
			}
			conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
			n, err := conn.Read(buf)
			if err != nil {
				continue
			}
			if msgType, err := protocol.ParseHeader(buf[:n]); err == nil && msgType == protocol.TypePing {
				p, err := protocol.DecodePing(buf[:n])
				if err == nil {
					conn.Write(protocol.EncodePong(protocol.Pong{Token: p.Token}))
				}
			}
		}
	}()
}

// A ping answered with a pong shows up as SelfPingMs in the answerer's own
// snapshots and as Entity.PingMs in everyone else's.
func TestPingRoundTripFillsSnapshotPings(t *testing.T) {
	srv, stop := start(t, Config{TickAt: 10 * time.Millisecond, Timeout: 2 * time.Second})
	defer stop()
	a, b := dial(t, srv), dial(t, srv)

	a.Write(protocol.EncodeHello(protocol.Hello{Name: "alice"}))
	b.Write(protocol.EncodeHello(protocol.Hello{Name: "bob"}))

	done := make(chan struct{})
	defer close(done)
	answerPings(t, b, done)

	// Loopback RTT rounds to 0 ms, which is indistinguishable from "not yet
	// measured" on the wire - so plant a nonzero measurement once the pong
	// path has run, and check it is what snapshots carry.
	deadline := time.Now().Add(2 * time.Second)
	measured := false
	for time.Now().Before(deadline) && !measured {
		srv.mu.Lock()
		for _, c := range srv.clients {
			if c.name == "bob" && c.pingSentAt.IsZero() && !c.lastPingAt.IsZero() {
				c.rttMs = 23 // a pong was processed; make the value visible
				measured = true
			}
		}
		srv.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	if !measured {
		t.Fatal("bob's pong never cleared the outstanding ping")
	}

	var seq uint32
	deadline = time.Now().Add(2 * time.Second)
	sawEntity := false
	for time.Now().Before(deadline) {
		seq++
		a.Write(protocol.EncodeState(stateAt(1, seq, seq*10, 100)))
		b.Write(protocol.EncodeState(stateAt(2, seq, seq*10, 200)))
		s, ok := readSnapshot(t, a, 20*time.Millisecond)
		if !ok {
			continue
		}
		for _, e := range s.Entities {
			if e.Name == "bob" && e.PingMs == 23 {
				sawEntity = true
			}
		}
		if sawEntity {
			return
		}
	}
	t.Fatal("bob's measured ping never appeared as Entity.PingMs in alice's snapshot")
}

func TestSelfPingMsInOwnSnapshot(t *testing.T) {
	srv, stop := start(t, Config{TickAt: 10 * time.Millisecond, Timeout: 2 * time.Second})
	defer stop()
	conn := dial(t, srv)

	conn.Write(protocol.EncodeHello(protocol.Hello{Name: "tester"}))
	time.Sleep(30 * time.Millisecond)
	srv.mu.Lock()
	for _, c := range srv.clients {
		c.rttMs = 41
	}
	srv.mu.Unlock()

	deadline := time.Now().Add(2 * time.Second)
	var seq uint32
	for time.Now().Before(deadline) {
		seq++
		conn.Write(protocol.EncodeState(stateAt(1, seq, seq*10, 0)))
		if s, ok := readSnapshot(t, conn, 20*time.Millisecond); ok && s.SelfPingMs == 41 {
			return
		}
	}
	t.Fatal("the client's own RTT never arrived as Snapshot.SelfPingMs")
}

// readKick waits for the next Kick, ignoring anything else.
func readKick(t *testing.T, conn *net.UDPConn, within time.Duration) (protocol.Kick, bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(deadline)
		n, err := conn.Read(buf)
		if err != nil {
			return protocol.Kick{}, false
		}
		msgType, err := protocol.ParseHeader(buf[:n])
		if err != nil || msgType != protocol.TypeKick {
			continue
		}
		k, err := protocol.DecodeKick(buf[:n])
		if err != nil {
			t.Fatalf("decode kick: %v", err)
		}
		return k, true
	}
	return protocol.Kick{}, false
}

func TestKickRemovesSessionAndBlocksReRegister(t *testing.T) {
	srv, stop := start(t, Config{TickAt: 10 * time.Millisecond, Timeout: 5 * time.Second})
	defer stop()
	conn := dial(t, srv)

	conn.Write(protocol.EncodeHello(protocol.Hello{Name: "alice"}))
	time.Sleep(50 * time.Millisecond)

	if err := srv.Kick("alice", "testing"); err != nil {
		t.Fatalf("Kick: %v", err)
	}
	k, ok := readKick(t, conn, time.Second)
	if !ok {
		t.Fatal("the kicked client never received a Kick datagram")
	}
	if k.Reason != "testing" {
		t.Fatalf("kick reason %q, want %q", k.Reason, "testing")
	}

	srv.mu.Lock()
	n := len(srv.clients)
	srv.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d clients after the kick, want 0", n)
	}

	// The auto-re-register must bounce off the kick window.
	conn.Write(protocol.EncodeHello(protocol.Hello{Name: "alice"}))
	conn.Write(protocol.EncodeState(stateAt(1, 1, 0, 0)))
	time.Sleep(100 * time.Millisecond)
	srv.mu.Lock()
	n = len(srv.clients)
	srv.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d clients: the kicked address re-registered inside the window", n)
	}

	if err := srv.Kick("nobody", "x"); err == nil {
		t.Fatal("kicking an unknown player reported no error")
	}
}

func TestBanDropsDatagramsAndPersists(t *testing.T) {
	bansPath := filepath.Join(t.TempDir(), "bans.json")
	srv, stop := start(t, Config{TickAt: 10 * time.Millisecond, Timeout: 5 * time.Second,
		BansPath: bansPath})
	defer stop()
	conn := dial(t, srv)

	conn.Write(protocol.EncodeHello(protocol.Hello{Name: "mallory"}))
	time.Sleep(50 * time.Millisecond)

	if err := srv.Ban("mallory", "cheating"); err != nil {
		t.Fatalf("Ban: %v", err)
	}
	k, ok := readKick(t, conn, time.Second)
	if !ok {
		t.Fatal("the banned client never received a Kick datagram")
	}
	if k.Reason != "banned: cheating" {
		t.Fatalf("kick reason %q, want %q", k.Reason, "banned: cheating")
	}

	// Everything from the banned IP is dropped: no session comes back.
	deadline := time.Now().Add(300 * time.Millisecond)
	var seq uint32 = 100
	for time.Now().Before(deadline) {
		seq++
		conn.Write(protocol.EncodeHello(protocol.Hello{Name: "mallory"}))
		conn.Write(protocol.EncodeState(stateAt(1, seq, 0, 0)))
		time.Sleep(20 * time.Millisecond)
	}
	srv.mu.Lock()
	n := len(srv.clients)
	srv.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d clients: a banned IP got a session", n)
	}

	// The ban survives a reload of the file.
	reloaded, err := loadBans(bansPath)
	if err != nil {
		t.Fatalf("loadBans: %v", err)
	}
	e, ok := reloaded.get("127.0.0.1")
	if !ok {
		t.Fatalf("127.0.0.1 missing from reloaded %s", bansPath)
	}
	if e.Reason != "cheating" || e.Name != "mallory" || e.BannedBy != "rcon" {
		t.Fatalf("reloaded entry %+v", e)
	}

	if err := srv.Unban("127.0.0.1"); err != nil {
		t.Fatalf("Unban: %v", err)
	}
	if got := srv.Bans(); len(got) != 0 {
		t.Fatalf("%d bans after unban, want 0", len(got))
	}
	if err := srv.Unban("10.0.0.9"); err == nil {
		t.Fatal("unbanning an unlisted IP reported no error")
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
