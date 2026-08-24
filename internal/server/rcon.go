package server

import (
	"bufio"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

// RCON is a plain-TCP, password-protected admin console: one AUTH line, then
// text commands, one per line. Every response is zero or more payload lines,
// each prefixed "| ", followed by a terminator line that is exactly "OK" or
// "ERR <message>" - so a client never has to guess where a response ends.
//
// It is deliberately not encrypted: bind it to localhost or firewall it to
// admin IPs, and treat the password as an obstacle, not a wall.

const (
	rconIdleTimeout = 30 * time.Second
	rconMaxLine     = 4 * 1024
)

// ServeRCON accepts admin connections on l until l is closed. Password must
// be non-empty - the caller decides whether RCON is enabled at all.
func (s *Server) ServeRCON(l net.Listener, password string) {
	s.log.Printf("rcon listening on %s", l.Addr())
	for {
		conn, err := l.Accept()
		if err != nil {
			return // listener closed
		}
		go s.rconSession(conn, password)
	}
}

func (s *Server) rconSession(conn net.Conn, password string) {
	defer conn.Close()
	remote := conn.RemoteAddr()
	s.log.Printf("rcon connect from %s", remote)

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, rconMaxLine), rconMaxLine)
	readLine := func() (string, bool) {
		conn.SetDeadline(time.Now().Add(rconIdleTimeout))
		if !sc.Scan() {
			return "", false
		}
		return strings.TrimSpace(sc.Text()), true
	}

	// First line must be AUTH <password>. A wrong guess costs one second,
	// which is all the brute-force protection a hobby server needs.
	line, ok := readLine()
	if !ok {
		return
	}
	pw, isAuth := strings.CutPrefix(line, "AUTH ")
	if !isAuth || pw != password {
		s.log.Printf("rcon auth failed from %s", remote)
		time.Sleep(time.Second)
		fmt.Fprintf(conn, "ERR auth failed\n")
		return
	}
	fmt.Fprintf(conn, "OK\n")

	for {
		line, ok := readLine()
		if !ok {
			return
		}
		if line == "" {
			fmt.Fprintf(conn, "OK\n")
			continue
		}
		s.log.Printf("rcon %s: %s", remote, line)
		payload, err := s.rconCommand(line)
		for _, p := range payload {
			fmt.Fprintf(conn, "| %s\n", p)
		}
		if err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err)
			continue
		}
		fmt.Fprintf(conn, "OK\n")
		if line == "quit" {
			return
		}
	}
}

// rconCommand runs one admin command and returns the payload lines. An error
// becomes the ERR terminator; nil becomes OK.
func (s *Server) rconCommand(line string) ([]string, error) {
	fields := strings.Fields(line)
	cmd, args := strings.ToLower(fields[0]), fields[1:]

	switch cmd {
	case "players":
		return s.rconPlayers(), nil

	case "kick":
		if len(args) < 1 {
			return nil, fmt.Errorf("usage: kick <id|name> [reason...]")
		}
		return nil, s.Kick(args[0], strings.Join(args[1:], " "))

	case "ban":
		if len(args) < 1 {
			return nil, fmt.Errorf("usage: ban <id|name|ip> [reason...]")
		}
		return nil, s.Ban(args[0], strings.Join(args[1:], " "))

	case "unban":
		if len(args) != 1 {
			return nil, fmt.Errorf("usage: unban <ip>")
		}
		return nil, s.Unban(args[0])

	case "bans":
		var out []string
		for _, b := range s.Bans() {
			name := b.Name
			if name == "" {
				name = "-"
			}
			out = append(out, fmt.Sprintf("%s %s %q banned %s by %s",
				b.IP, name, b.Reason, b.BannedAt.Format(time.RFC3339), b.BannedBy))
		}
		return out, nil

	case "say":
		if len(args) == 0 {
			return nil, fmt.Errorf("usage: say <text...>")
		}
		s.Say(strings.Join(args, " "))
		return nil, nil

	case "status":
		return s.rconStatus(), nil

	case "quit":
		return nil, nil

	default:
		return nil, fmt.Errorf("unknown command %q", cmd)
	}
}

func (s *Server) rconPlayers() []string {
	now := time.Now()
	s.mu.Lock()
	list := make([]*client, 0, len(s.clients))
	for _, c := range s.clients {
		list = append(list, c)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].id < list[j].id })
	out := make([]string, 0, len(list))
	for _, c := range list {
		car := c.car
		if car == "" {
			car = "-"
		}
		up := now.Sub(c.joinedAt).Round(time.Second)
		out = append(out, fmt.Sprintf("%d %s %s %dms %s up:%02d:%02d",
			c.id, c.name, car, c.rttMs, c.addr,
			int(up.Minutes()), int(up.Seconds())%60))
	}
	s.mu.Unlock()
	return out
}

func (s *Server) rconStatus() []string {
	s.mu.Lock()
	n := len(s.clients)
	rx, tx := s.rxCount, s.txCount
	rxB, txB := s.rxBytes, s.txBytes
	s.mu.Unlock()
	return []string{
		fmt.Sprintf("addr %s", s.conn.LocalAddr()),
		fmt.Sprintf("uptime %s", time.Since(s.started).Round(time.Second)),
		fmt.Sprintf("tick %v (%d Hz)", s.cfg.TickAt, int(time.Second/s.cfg.TickAt)),
		fmt.Sprintf("clients %d", n),
		fmt.Sprintf("rx %d pkt %d bytes", rx, rxB),
		fmt.Sprintf("tx %d pkt %d bytes", tx, txB),
	}
}
