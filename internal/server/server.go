// Package server is the RBR multiplayer server: a UDP relay that also knows
// how to replay a client back to itself on a delay.
package server

import (
	"fmt"
	"log"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/lukaszryczko/rbr-mp-server/internal/protocol"
)

// Config is everything the server can be told.
type Config struct {
	Addr    string        // ":40100"
	TickAt  time.Duration // how often snapshots go out
	Echo    time.Duration // replay each client to itself this far behind; 0 = off
	Timeout time.Duration // drop a client that has gone quiet this long
	Stale   time.Duration // stop relaying a player whose state is older than this
	Verbose bool
	Stats   time.Duration // how often to print a line; 0 = never
}

// DefaultConfig is what the binary runs with when given no flags.
func DefaultConfig() Config {
	return Config{
		Addr:    ":40100",
		TickAt:  time.Second / 30,
		Echo:    time.Second,
		Timeout: 5 * time.Second,
		Stale:   2 * time.Second,
		Stats:   10 * time.Second,
	}
}

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
}

// Server is a running instance. Use New then Run.
type Server struct {
	cfg  Config
	conn *net.UDPConn
	log  *log.Logger

	mu      sync.Mutex
	clients map[string]*client
	nextID  uint32

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
	addr, err := net.ResolveUDPAddr("udp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", cfg.Addr, err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %q: %w", cfg.Addr, err)
	}
	return &Server{
		cfg:     cfg,
		conn:    conn,
		log:     logger,
		clients: make(map[string]*client),
		nextID:  1,
		started: time.Now(),
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
	s.log.Printf("listening on %s, tick %v, echo %v, timeout %v",
		s.conn.LocalAddr(), s.cfg.TickAt, s.cfg.Echo, s.cfg.Timeout)

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
	msgType, err := protocol.ParseHeader(b)
	if err != nil {
		if s.cfg.Verbose {
			s.log.Printf("%s: %v", addr, err)
		}
		return
	}

	switch msgType {
	case protocol.TypeHello:
		h, err := protocol.DecodeHello(b)
		if err != nil {
			return
		}
		s.join(addr, h, now)

	case protocol.TypeState:
		st, err := protocol.DecodeState(b)
		if err != nil {
			return
		}
		s.state(addr, st, now)

	case protocol.TypeBye:
		s.mu.Lock()
		if c, ok := s.clients[addr.String()]; ok {
			delete(s.clients, c.addrKey)
			s.log.Printf("player %d (%s) left", c.id, c.name)
		}
		s.mu.Unlock()
	}
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
		// Room for the echo delay plus a healthy margin, so a late tick still
		// finds both samples it needs to interpolate.
		history: NewHistory(s.cfg.Echo + 3*time.Second),
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
		EchoDelayMs:  uint16(s.cfg.Echo.Milliseconds()),
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
			c.history = NewHistory(s.cfg.Echo + 3*time.Second)
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
				})
			}
		}

		// The echo: this client's own car as it was `Echo` ago. Nothing is sent
		// until the history actually reaches back that far, so the ghost
		// appears one delay after joining rather than sitting at the origin.
		if s.cfg.Echo > 0 {
			if sample, ok := me.history.At(now.Add(-s.cfg.Echo)); ok {
				entities = append(entities, protocol.Entity{
					// High bit set: an echo id can never collide with a real
					// player id, so a client can key on it safely.
					ID:           me.id | 0x8000_0000,
					Flags:        protocol.FlagEcho,
					Name:         echoName(me.name),
					Car:          me.car,
					Transform:    sample.Transform,
					Telemetry:    sample.Telemetry,
					SampleTimeMs: sample.ClientTimeMs,
				})
			}
		}

		data := protocol.EncodeSnapshot(protocol.Snapshot{
			ServerTimeMs:     s.uptimeMs(now),
			LastClientTimeMs: me.lastClientTimeMs,
			Entities:         entities,
		})
		out = append(out, outgoing{addr: me.addr, data: data})
	}
	s.mu.Unlock()

	for _, o := range out {
		s.send(o.addr, o.data)
	}
}

func echoName(name string) string {
	const suffix = " (echo)"
	if len(name)+len(suffix) >= protocol.NameLen {
		name = name[:protocol.NameLen-len(suffix)-1]
	}
	return name + suffix
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
