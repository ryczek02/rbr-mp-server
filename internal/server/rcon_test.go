package server

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/lukaszryczko/rbr-mp-server/internal/protocol"
)

// startRCON puts an RCON listener in front of an already-running server.
func startRCON(t *testing.T, srv *Server, password string) net.Addr {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	go srv.ServeRCON(l, password)
	return l.Addr()
}

func dialRCON(t *testing.T, addr net.Addr) (net.Conn, *bufio.Scanner) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr.String(), time.Second)
	if err != nil {
		t.Fatalf("dial rcon: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, bufio.NewScanner(conn)
}

// rconResponse reads payload lines up to the OK/ERR terminator.
func rconResponse(t *testing.T, sc *bufio.Scanner) (payload []string, ok bool, errMsg string) {
	t.Helper()
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "OK":
			return payload, true, ""
		case strings.HasPrefix(line, "ERR "):
			return payload, false, strings.TrimPrefix(line, "ERR ")
		default:
			payload = append(payload, strings.TrimPrefix(line, "| "))
		}
	}
	t.Fatalf("connection closed mid-response (payload so far: %v)", payload)
	return nil, false, ""
}

func rconCmd(t *testing.T, conn net.Conn, sc *bufio.Scanner, line string) ([]string, bool, string) {
	t.Helper()
	fmt.Fprintf(conn, "%s\n", line)
	return rconResponse(t, sc)
}

func TestRCONAuthFailureCloses(t *testing.T) {
	srv, stop := start(t, Config{TickAt: 10 * time.Millisecond, Timeout: time.Second})
	defer stop()
	addr := startRCON(t, srv, "secret")
	conn, sc := dialRCON(t, addr)

	fmt.Fprintf(conn, "AUTH wrong\n")
	if !sc.Scan() {
		t.Fatal("no reply to a bad AUTH")
	}
	if got := sc.Text(); got != "ERR auth failed" {
		t.Fatalf("bad AUTH got %q, want %q", got, "ERR auth failed")
	}
	if sc.Scan() {
		t.Fatalf("connection stayed open after failed auth, read %q", sc.Text())
	}
}

func TestRCONCommands(t *testing.T) {
	srv, stop := start(t, Config{TickAt: 10 * time.Millisecond, Timeout: 5 * time.Second})
	defer stop()
	addr := startRCON(t, srv, "secret")

	// A player to administrate.
	game := dial(t, srv)
	game.Write(protocol.EncodeHello(protocol.Hello{Name: "alice", Car: "XSARA"}))
	time.Sleep(50 * time.Millisecond)

	conn, sc := dialRCON(t, addr)
	fmt.Fprintf(conn, "AUTH secret\n")
	if _, ok, msg := rconResponse(t, sc); !ok {
		t.Fatalf("auth failed: %s", msg)
	}

	// players
	payload, ok, msg := rconCmd(t, conn, sc, "players")
	if !ok {
		t.Fatalf("players: %s", msg)
	}
	if len(payload) != 1 || !strings.Contains(payload[0], "alice") ||
		!strings.Contains(payload[0], "XSARA") {
		t.Fatalf("players payload %v", payload)
	}

	// say reaches the game client as PlayerID 0 / SERVER.
	if _, ok, msg := rconCmd(t, conn, sc, "say hello drivers"); !ok {
		t.Fatalf("say: %s", msg)
	}
	c, got := readChat(t, game, time.Second)
	if !got {
		t.Fatal("the game client never received the say broadcast")
	}
	if c.PlayerID != 0 || c.Name != "SERVER" || c.Text != "hello drivers" {
		t.Fatalf("say arrived as %d/%q: %q", c.PlayerID, c.Name, c.Text)
	}

	// kick
	if _, ok, msg := rconCmd(t, conn, sc, "kick alice go away"); !ok {
		t.Fatalf("kick: %s", msg)
	}
	if k, got := readKick(t, game, time.Second); !got || k.Reason != "go away" {
		t.Fatalf("kick datagram missing or wrong: %v %v", k, got)
	}
	srv.mu.Lock()
	n := len(srv.clients)
	srv.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d clients after rcon kick, want 0", n)
	}

	// kicking a ghost is an ERR, not a dropped connection
	if _, ok, _ := rconCmd(t, conn, sc, "kick nobody"); ok {
		t.Fatal("kicking an unknown player returned OK")
	}

	// ban by IP, list, unban
	if _, ok, msg := rconCmd(t, conn, sc, "ban 203.0.113.7 smurf"); !ok {
		t.Fatalf("ban: %s", msg)
	}
	payload, ok, msg = rconCmd(t, conn, sc, "bans")
	if !ok {
		t.Fatalf("bans: %s", msg)
	}
	if len(payload) != 1 || !strings.Contains(payload[0], "203.0.113.7") ||
		!strings.Contains(payload[0], "smurf") {
		t.Fatalf("bans payload %v", payload)
	}
	if _, ok, msg := rconCmd(t, conn, sc, "unban 203.0.113.7"); !ok {
		t.Fatalf("unban: %s", msg)
	}
	if payload, _, _ := rconCmd(t, conn, sc, "bans"); len(payload) != 0 {
		t.Fatalf("bans after unban: %v", payload)
	}

	// unknown command
	if _, ok, msg := rconCmd(t, conn, sc, "frobnicate"); ok || !strings.Contains(msg, "unknown") {
		t.Fatalf("unknown command: ok=%v msg=%q", ok, msg)
	}

	// quit: OK, then the server hangs up - not the whole server, just us.
	if _, ok, _ := rconCmd(t, conn, sc, "quit"); !ok {
		t.Fatal("quit did not return OK")
	}
	if sc.Scan() {
		t.Fatalf("connection stayed open after quit, read %q", sc.Text())
	}
	if _, err := net.DialTimeout("tcp", addr.String(), time.Second); err != nil {
		t.Fatalf("rcon listener died after quit: %v", err)
	}
}
