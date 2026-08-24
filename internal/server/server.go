// Package server is the RBR multiplayer server: a UDP relay that also knows
// how to replay a client back to itself on a delay.
package server

import (
	"fmt"
	"log"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lukaszryczko/rbr-mp-server/internal/protocol"
)

// Config is everything the server can be told.
type Config struct {
	Addr     string        // ":40100"
	TickAt   time.Duration // how often snapshots go out
	Timeout  time.Duration // drop a client that has gone quiet this long
	Stale    time.Duration // stop relaying a player whose state is older than this
	Verbose  bool
	Stats    time.Duration // how often to print a line; 0 = never
	BansPath string        // JSON file the ban list persists to
}

// DefaultConfig is what the binary runs with when given no flags.
func DefaultConfig() Config {
	return Config{
		Addr:     ":40100",
		TickAt:   time.Second / 30,
		Timeout:  5 * time.Second,
		Stale:    2 * time.Second,
		Stats:    10 * time.Second,
		BansPath: "bans.json",
	}
}

// pingEvery is how often the server measures each client's round trip, and
// also how long an unanswered ping waits before it is written off as lost
// and resent.
const pingEvery = 2 * time.Second

// kickWindow is how long after a kick the address's Hello/State datagrams
// are silently dropped, so the client's automatic re-register does not put
// the player straight back.
const kickWindow = 10 * time.Second

// banNoticeEvery rate-limits the Kick reply a banned address gets, so a
// banned client still learns why but cannot make the server chatty.
const banNoticeEvery = 5 * time.Second

type client struct {
	id       uint32
	name     string
	car      string // Cars\<folder> the player drives; empty until a Hello says
	addr     *net.UDPAddr
	addrKey  string
	joinedAt time.Time
	lastSeen time.Time

	history *History

	lastClientTimeMs uint32 // echoed back so the client can measure its RTT
	lastSeq          uint32
	packets          uint64
	dropped          uint64 // States that arrived out of order

	lastChat time.Time // flood guard: one line per 300 ms per client

	// Ping measurement. The server sends a Ping every pingEvery, the client
	// echoes the token in a Pong, and rttMs is the measured round trip.
	rttMs      uint32
	pingToken  uint32
	pingSentAt time.Time // zero = no ping outstanding
	lastPingAt time.Time
}

// Server is a running instance. Use New then Run.
type Server struct {
	cfg  Config
	conn *net.UDPConn
	log  *log.Logger

	mu      sync.Mutex
	clients map[string]*client
	nextID  uint32

	bans *banList
	// recentlyKicked holds addresses (addr.String()) whose Hello/State are
	// dropped for kickWindow after a kick, so the client's auto-re-register
	// does not instantly re-add the player.
	recentlyKicked map[string]time.Time
	// lastBanNotice rate-limits the Kick reply sent to banned addresses.
	lastBanNotice map[string]time.Time

	started  time.Time
	rxCount  uint64
	txCount  uint64
	rxBytes  uint64
	txBytes  uint64
	echoedMs int64
}

// New binds the socket.
func New(cfg Config, logger *log.Logger) (*Server, error) {
	if cfg.TickAt <= 0 {
		cfg.TickAt = time.Second / 30
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.Stale <= 0 {
		cfg.Stale = 2 * time.Second
	}
	if cfg.BansPath == "" {
		cfg.BansPath = "bans.json"
	}
	bans, err := loadBans(cfg.BansPath)
	if err != nil {
		return nil, err
	}
	if len(bans.entries) == 0 {
		logger.Printf("no bans loaded from %s", cfg.BansPath)
	} else {
		logger.Printf("%d ban(s) loaded from %s", len(bans.entries), cfg.BansPath)
	}
	addr, err := net.ResolveUDPAddr("udp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", cfg.Addr, err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %q: %w", cfg.Addr, err)
	}
	return &Server{
		cfg:            cfg,
		conn:           conn,
		log:            logger,
		clients:        make(map[string]*client),
		nextID:         1,
		bans:           bans,
		recentlyKicked: make(map[string]time.Time),
		lastBanNotice:  make(map[string]time.Time),
		started:        time.Now(),
	}, nil
}

// LocalAddr is the address actually bound, which matters when the config asked
// for port 0.
func (s *Server) LocalAddr() net.Addr { return s.conn.LocalAddr() }

// Close stops the server. Run returns shortly after.
func (s *Server) Close() error { return s.conn.Close() }

// Run serves until the connection is closed. It is not expected to return
// otherwise.
func (s *Server) Run() error {
	s.log.Printf("listening on %s, tick %v, timeout %v",
		s.conn.LocalAddr(), s.cfg.TickAt, s.cfg.Timeout)

	done := make(chan struct{})
	go s.tickLoop(done)
	if s.cfg.Stats > 0 {
		go s.statsLoop(done)
	}
	defer close(done)

	buf := make([]byte, 2048)
	for {
		n, addr, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			return err // the socket was closed, or something worse
		}
		s.mu.Lock()
		s.rxCount++
		s.rxBytes += uint64(n)
		s.mu.Unlock()
		s.handle(buf[:n], addr, time.Now())
	}
}

func (s *Server) handle(b []byte, addr *net.UDPAddr, now time.Time) {
	// Bans first, before any decode: one map lookup on the IP string, and
	// everything from a banned address is dropped. At most once per
	// banNoticeEvery the address is also told why, with a Kick.
	s.mu.Lock()
	if ban, banned := s.bans.get(addr.IP.String()); banned {
		notify := now.Sub(s.lastBanNotice[addr.String()]) >= banNoticeEvery
		if notify {
			s.lastBanNotice[addr.String()] = now
		}
		s.mu.Unlock()
		if notify {
			s.send(addr, protocol.EncodeKick(protocol.Kick{Reason: "banned: " + ban.Reason}))
		}
		return
	}
	s.mu.Unlock()

	msgType, err := protocol.ParseHeader(b)
	if err != nil {
		if s.cfg.Verbose {
			s.log.Printf("%s: %v", addr, err)
		}
		return
	}

	switch msgType {
	case protocol.TypeHello:
		if s.kickedRecently(addr, now) {
			return
		}
		h, err := protocol.DecodeHello(b)
		if err != nil {
			return
		}
		s.join(addr, h, now)

	case protocol.TypeState:
		if s.kickedRecently(addr, now) {
			return
		}
		st, err := protocol.DecodeState(b)
		if err != nil {
			return
		}
		s.state(addr, st, now)

	case protocol.TypePong:
		p, err := protocol.DecodePong(b)
		if err != nil {
			return
		}
		s.pong(addr, p, now)

	case protocol.TypeChat:
		ch, err := protocol.DecodeChat(b)
		if err != nil {
			return
		}
		s.chat(addr, ch, now)

	case protocol.TypeBye:
		s.mu.Lock()
		if c, ok := s.clients[addr.String()]; ok {
			delete(s.clients, c.addrKey)
			s.log.Printf("player %d (%s) left", c.id, c.name)
		}
		s.mu.Unlock()
	}
}

// kickedRecently reports whether the address was kicked inside the last
// kickWindow, during which its Hello/State are silently dropped.
func (s *Server) kickedRecently(addr *net.UDPAddr, now time.Time) bool {
	s.mu.Lock()
	at, ok := s.recentlyKicked[addr.String()]
	s.mu.Unlock()
	return ok && now.Sub(at) < kickWindow
}

// pong closes the loop a tick's Ping opened: a matching token from a known
// address turns into that client's measured round trip.
func (s *Server) pong(addr *net.UDPAddr, p protocol.Pong, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clients[addr.String()]
	if !ok || c.pingSentAt.IsZero() || p.Token != c.pingToken {
		return
	}
	rtt := now.Sub(c.pingSentAt).Milliseconds()
	if rtt < 0 {
		rtt = 0
	}
	c.rttMs = uint32(rtt)
	c.pingSentAt = time.Time{}
}

// clientLocked finds or creates the client for an address. The caller must
// hold s.mu. Every field the rest of the server reads without the lock (id,
// name, addr) is set here, before the client is published into the map.
func (s *Server) clientLocked(addr *net.UDPAddr, name string, now time.Time) (*client, bool) {
	key := addr.String()
	if c, ok := s.clients[key]; ok {
		return c, false
	}
	if name == "" {
		name = fmt.Sprintf("Player %d", s.nextID)
	}
	c := &client{
		id:       s.nextID,
		name:     name,
		addr:     addr,
		addrKey:  key,
		joinedAt: now,
		// A healthy margin of history, so a late tick still finds the
		// samples it needs.
		history: NewHistory(3 * time.Second),
	}
	s.nextID++
	s.clients[key] = c
	s.log.Printf("player %d (%s) joined from %s", c.id, c.name, addr)
	return c, true
}

func (s *Server) sendWelcome(c *client, now time.Time) {
	s.send(c.addr, protocol.EncodeWelcome(protocol.Welcome{
		PlayerID:     c.id,
		TickRateHz:   uint16(time.Second / s.cfg.TickAt),
		EchoDelayMs:  0, // the field stays for wire compatibility; the echo player is gone
		ServerTimeMs: s.uptimeMs(now),
	}))
}

func (s *Server) join(addr *net.UDPAddr, h protocol.Hello, now time.Time) {
	s.mu.Lock()
	c, created := s.clientLocked(addr, h.Name, now)
	c.lastSeen = now
	// A repeated Hello is how a client announces a name or car change.
	if h.Name != "" && h.Name != c.name {
		c.name = h.Name
	}
	if h.Car != c.car {
		c.car = h.Car
		if !created && h.Car != "" {
			s.log.Printf("player %d (%s) is driving %s", c.id, c.name, c.car)
		}
	}
	s.mu.Unlock()
	s.sendWelcome(c, now)
}

func (s *Server) state(addr *net.UDPAddr, st protocol.State, now time.Time) {
	s.mu.Lock()
	// A State without a Hello registers the client anyway, so a client that
	// was already running when the server started does not have to notice.
	c, created := s.clientLocked(addr, "", now)

	// The client is plainly alive, so it counts as seen whatever we do with
	// this particular packet - otherwise the reaper below could take out
	// someone who is merely sending out of order.
	c.lastSeen = now

	accept := true
	if c.history.Len() > 0 {
		switch delta := int32(st.Seq - c.lastSeq); {
		case delta > 0:
			// The normal case: newer than anything held.
		case delta < -1000:
			// A huge jump backwards is a client that restarted and began its
			// sequence again. Its old timeline is meaningless now.
			s.log.Printf("player %d (%s) restarted its sequence, history cleared", c.id, c.name)
			c.history = NewHistory(3 * time.Second)
		default:
			// UDP reordered a datagram. Inserting a sample older than the
			// newest held would corrupt the replay timeline, so drop it; the
			// next one is a few milliseconds away.
			c.dropped++
			accept = false
		}
	}

	if accept {
		c.lastSeq = st.Seq
		c.lastClientTimeMs = st.ClientTimeMs
		c.packets++
		c.history.Add(Sample{
			RecvAt:       now,
			ClientTimeMs: st.ClientTimeMs,
			Transform:    st.Transform,
			Telemetry:    st.Telemetry,
		})
	}
	s.mu.Unlock()

	if created {
		s.sendWelcome(c, now)
	}
}

// chat rebroadcasts one line of text to every connected client, the sender
// included - a message "arrives" for its author the same way it does for
// everyone else, so the client needs no local echo path. Identity comes from
// the session, never from the datagram: whatever id/name the sender claimed
// is overwritten before anyone hears it.
func (s *Server) chat(addr *net.UDPAddr, ch protocol.Chat, now time.Time) {
	if ch.Text == "" {
		return
	}

	type outgoing struct {
		addr *net.UDPAddr
		data []byte
	}
	var out []outgoing

	s.mu.Lock()
	c, ok := s.clients[addr.String()]
	if !ok || now.Sub(c.lastChat) < 300*time.Millisecond {
		s.mu.Unlock()
		return // no session, or typing faster than any human: drop it
	}
	c.lastChat = now
	c.lastSeen = now
	data := protocol.EncodeChat(protocol.Chat{PlayerID: c.id, Name: c.name, Text: ch.Text})
	for _, other := range s.clients {
		out = append(out, outgoing{addr: other.addr, data: data})
	}
	s.log.Printf("chat %d (%s): %s", c.id, c.name, ch.Text)
	s.mu.Unlock()

	for _, o := range out {
		s.send(o.addr, o.data)
	}
}

func (s *Server) tickLoop(done <-chan struct{}) {
	t := time.NewTicker(s.cfg.TickAt)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case now := <-t.C:
			s.tick(now)
		}
	}
}

// tick builds and sends one snapshot per connected client.
func (s *Server) tick(now time.Time) {
	type outgoing struct {
		addr *net.UDPAddr
		data []byte
	}
	var out []outgoing

	s.mu.Lock()
	// Drop the silent ones first, so nobody is told about a player that left.
	for key, c := range s.clients {
		if now.Sub(c.lastSeen) > s.cfg.Timeout {
			delete(s.clients, key)
			s.log.Printf("player %d (%s) timed out after %v", c.id, c.name, s.cfg.Timeout)
		}
	}

	// Forget kicks and ban notices old enough not to matter any more.
	for key, at := range s.recentlyKicked {
		if now.Sub(at) >= kickWindow {
			delete(s.recentlyKicked, key)
		}
	}
	for key, at := range s.lastBanNotice {
		if now.Sub(at) >= banNoticeEvery {
			delete(s.lastBanNotice, key)
		}
	}

	// Ping whoever is due: last ping ≥ pingEvery ago, and either nothing
	// outstanding or the outstanding one is old enough to be written off as
	// lost and resent.
	for _, c := range s.clients {
		if now.Sub(c.lastPingAt) < pingEvery {
			continue
		}
		if !c.pingSentAt.IsZero() && now.Sub(c.pingSentAt) <= pingEvery {
			continue
		}
		c.pingToken = s.uptimeMs(now)
		c.pingSentAt = now
		c.lastPingAt = now
		out = append(out, outgoing{addr: c.addr, data: protocol.EncodePing(protocol.Ping{Token: c.pingToken})})
	}

	live := make([]*client, 0, len(s.clients))
	for _, c := range s.clients {
		live = append(live, c)
	}
	// Stable order, so a snapshot's entity list does not shuffle between ticks.
	sort.Slice(live, func(i, j int) bool { return live[i].id < live[j].id })

	for _, me := range live {
		entities := make([]protocol.Entity, 0, len(live))

		// Every other player, at their most recent known pose.
		for _, other := range live {
			if other.id == me.id {
				continue
			}
			// A player who stopped sending is not worth relaying: the others
			// would draw a frozen car for the whole Timeout window. Dropping
			// them from snapshots early is what lets clients despawn quickly;
			// the full Timeout still governs forgetting the session itself.
			if now.Sub(other.lastSeen) > s.cfg.Stale {
				continue
			}
			if sample, ok := other.history.Latest(); ok {
				entities = append(entities, protocol.Entity{
					ID:           other.id,
					Name:         other.name,
					Car:          other.car,
					Transform:    sample.Transform,
					Telemetry:    sample.Telemetry,
					SampleTimeMs: sample.ClientTimeMs,
					PingMs:       clampPing(other.rttMs),
				})
			}
		}

		data := protocol.EncodeSnapshot(protocol.Snapshot{
			ServerTimeMs:     s.uptimeMs(now),
			LastClientTimeMs: me.lastClientTimeMs,
			SelfPingMs:       clampPing(me.rttMs),
			Entities:         entities,
		})
		out = append(out, outgoing{addr: me.addr, data: data})
	}
	s.mu.Unlock()

	for _, o := range out {
		s.send(o.addr, o.data)
	}
}

// clampPing squeezes a measured RTT into the u16 the wire carries.
func clampPing(ms uint32) uint16 {
	if ms > 65535 {
		return 65535
	}
	return uint16(ms)
}

// findClientLocked resolves "3" or "alice" (case-insensitive) to a session.
// The caller must hold s.mu.
func (s *Server) findClientLocked(idOrName string) *client {
	if id, err := strconv.ParseUint(idOrName, 10, 32); err == nil {
		for _, c := range s.clients {
			if c.id == uint32(id) {
				return c
			}
		}
	}
	for _, c := range s.clients {
		if strings.EqualFold(c.name, idOrName) {
			return c
		}
	}
	return nil
}

// Kick removes a connected player by id or name: the client is told why (a
// few times, since UDP), the session is deleted, and the address is ignored
// for kickWindow so the client's auto-re-register does not put it straight
// back. Safe to call from any goroutine (RCON does).
func (s *Server) Kick(idOrName, reason string) error {
	if reason == "" {
		reason = "kicked"
	}
	s.mu.Lock()
	c := s.findClientLocked(idOrName)
	if c == nil {
		s.mu.Unlock()
		return fmt.Errorf("no connected player matches %q", idOrName)
	}
	delete(s.clients, c.addrKey)
	s.recentlyKicked[c.addrKey] = time.Now()
	s.log.Printf("player %d (%s) kicked: %s", c.id, c.name, reason)
	s.mu.Unlock()

	// Fire-and-forget UDP: three copies, so one lost datagram does not leave
	// the client wondering.
	data := protocol.EncodeKick(protocol.Kick{Reason: reason})
	for i := 0; i < 3; i++ {
		s.send(c.addr, data)
	}
	return nil
}

// Ban bans by connected player (id or name - they are kicked too) or, when
// nothing matches, by literal IP. The ban persists to the bans file.
func (s *Server) Ban(idOrNameOrIP, reason string) error {
	if reason == "" {
		reason = "banned"
	}
	s.mu.Lock()
	c := s.findClientLocked(idOrNameOrIP)
	s.mu.Unlock()

	ip, name := "", ""
	if c != nil {
		ip, name = c.addr.IP.String(), c.name
	} else {
		parsed := net.ParseIP(idOrNameOrIP)
		if parsed == nil {
			return fmt.Errorf("%q is neither a connected player nor an IP", idOrNameOrIP)
		}
		ip = parsed.String()
	}

	s.mu.Lock()
	s.bans.add(BanEntry{
		IP:       ip,
		Name:     name,
		Reason:   reason,
		BannedAt: time.Now().UTC(),
		BannedBy: "rcon",
	})
	err := s.bans.save()
	s.log.Printf("banned %s (%s): %s", ip, name, reason)
	s.mu.Unlock()

	if c != nil {
		if kerr := s.Kick(idOrNameOrIP, "banned: "+reason); kerr != nil && err == nil {
			err = kerr
		}
	}
	return err
}

// Unban removes an IP from the ban list and persists the change.
func (s *Server) Unban(ip string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.bans.remove(ip) {
		return fmt.Errorf("%s is not banned", ip)
	}
	s.log.Printf("unbanned %s", ip)
	return s.bans.save()
}

// Bans returns the current ban list, oldest first.
func (s *Server) Bans() []BanEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bans.list()
}

// Say broadcasts a chat line to every client as the server itself:
// player id 0, name "SERVER" - an id no real player ever gets.
func (s *Server) Say(text string) {
	if text == "" {
		return
	}
	s.mu.Lock()
	data := protocol.EncodeChat(protocol.Chat{PlayerID: 0, Name: "SERVER", Text: text})
	addrs := make([]*net.UDPAddr, 0, len(s.clients))
	for _, c := range s.clients {
		addrs = append(addrs, c.addr)
	}
	s.log.Printf("chat 0 (SERVER): %s", text)
	s.mu.Unlock()

	for _, a := range addrs {
		s.send(a, data)
	}
}

func (s *Server) send(addr *net.UDPAddr, b []byte) {
	n, err := s.conn.WriteToUDP(b, addr)
	if err != nil {
		if s.cfg.Verbose {
			s.log.Printf("send to %s: %v", addr, err)
		}
		return
	}
	s.mu.Lock()
	s.txCount++
	s.txBytes += uint64(n)
	s.mu.Unlock()
}

func (s *Server) uptimeMs(now time.Time) uint32 {
	return uint32(now.Sub(s.started).Milliseconds())
}

func (s *Server) statsLoop(done <-chan struct{}) {
	t := time.NewTicker(s.cfg.Stats)
	defer t.Stop()
	var lastRx, lastTx, lastRxB, lastTxB uint64
	for {
		select {
		case <-done:
			return
		case <-t.C:
			s.mu.Lock()
			n := len(s.clients)
			rx, tx, rxB, txB := s.rxCount, s.txCount, s.rxBytes, s.txBytes
			s.mu.Unlock()

			secs := s.cfg.Stats.Seconds()
			s.log.Printf("%d client(s)  in %.0f pkt/s %.1f kB/s  out %.0f pkt/s %.1f kB/s",
				n,
				float64(rx-lastRx)/secs, float64(rxB-lastRxB)/secs/1024,
				float64(tx-lastTx)/secs, float64(txB-lastTxB)/secs/1024)
			lastRx, lastTx, lastRxB, lastTxB = rx, tx, rxB, txB
		}
	}
}
