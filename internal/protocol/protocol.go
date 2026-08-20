// Package protocol is the wire format shared by the server and the game
// client. It is deliberately a fixed-layout, little-endian binary format with
// no framing beyond the UDP datagram: the client side is C++ inside a 32-bit
// game process, and the less it has to parse the better.
//
// docs/PROTOCOL.md describes the same thing in prose. Keep the two in step.
package protocol

import (
	"encoding/binary"
	"errors"
	"math"
)

// Magic is "ORMP" read little-endian, so it appears as those four ASCII bytes
// at the start of every datagram.
const (
	Magic   uint32 = 0x504D524F
	Version uint16 = 2
)

// Message types.
const (
	TypeHello    uint16 = 1 // client -> server, once, to join
	TypeWelcome  uint16 = 2 // server -> client, assigns the player id
	TypeState    uint16 = 3 // client -> server, every tick
	TypeSnapshot uint16 = 4 // server -> client, every tick
	TypeBye      uint16 = 5 // client -> server, on a clean disconnect
	TypeChat     uint16 = 6 // client -> server a line of text; server -> everyone (sender included)
)

// ChatTextLen is the fixed size of a chat line on the wire, NUL-padded.
// Anything longer is truncated by the encoder.
const ChatTextLen = 128

// NameLen is the fixed size of every name field, NUL-padded. Car identity
// (the game's Cars\<folder> name) uses the same size.
const NameLen = 24

// HeaderSize is the 8 bytes every message starts with.
const HeaderSize = 8

var (
	ErrShort   = errors.New("protocol: datagram too short")
	ErrMagic   = errors.New("protocol: bad magic")
	ErrVersion = errors.New("protocol: unsupported version")
)

// Header is the common prefix of every message.
type Header struct {
	Magic   uint32
	Version uint16
	Type    uint16
}

// Transform is a car's pose: a world position in metres (Z-up, right-handed)
// and its orientation as the car's local axes expressed as world direction
// vectors, one axis per row.
//
// The 3x3 is sent verbatim rather than as a quaternion on purpose. It is
// exactly what the client reads out of the game and exactly what it renders
// with, so a round trip through this server cannot introduce a conversion bug
// - which matters when the whole point is to measure the transport.
type Transform struct {
	Pos [3]float32
	Rot [9]float32 // rows: local +X, +Y, +Z as world directions
}

// Wheel slots, the order every per-wheel array uses.
const (
	WheelLF = 0
	WheelRF = 1
	WheelLB = 2
	WheelRB = 3
)

// Telemetry is what a car is doing, beyond where it is. It exists so remote
// cars can be animated (wheels spinning and steering, engine audio pitched by
// RPM) and extrapolated (velocity-based dead reckoning) instead of gliding
// statues snapped to the last received pose.
type Telemetry struct {
	Vel        [3]float32 // world velocity, m/s - drives extrapolation
	Speed      float32    // m/s
	RPM        float32    // engine speed
	Steer      float32    // steering input, -1..+1, positive = right
	Gear       int32      // 0 = reverse, 1 = neutral, 2.. = 1st..
	WheelOmega [4]float32 // wheel angular velocity, rad/s, LF RF LB RB
}

// TelemetrySize is the fixed on-the-wire size of a Telemetry.
const TelemetrySize = 12 + 4 + 4 + 4 + 4 + 16

// Hello joins the session. Safe to repeat - the client re-sends it when its
// car changes, and the server answers every one with a Welcome.
type Hello struct {
	Name string
	Car  string // Cars\<folder> the player drives; may be empty
}

// Welcome answers a Hello.
type Welcome struct {
	PlayerID     uint32
	TickRateHz   uint16
	EchoDelayMs  uint16 // 0 when the echo player is disabled
	ServerTimeMs uint32
}

// State is one sample of what a client's car is doing. ClientTimeMs is the
// client's own clock: the server never interprets it, it only stores it and
// hands it back, so the client can measure the true age of anything it
// receives without the two clocks having to agree.
type State struct {
	PlayerID     uint32
	Seq          uint32
	ClientTimeMs uint32
	Transform
	Telemetry
}

// Entity flags.
const (
	// FlagEcho marks the entity as a replay of the receiving client's own car,
	// delayed by the server. It is not another player.
	FlagEcho uint16 = 1 << 0
)

// Entity is one car in a snapshot.
type Entity struct {
	ID    uint32
	Flags uint16
	_     uint16
	Name  string // NameLen bytes on the wire
	Car   string // NameLen bytes on the wire; empty = unknown
	Transform
	Telemetry
	// SampleTimeMs is the ClientTimeMs of the sample this pose came from.
	// For an echo entity that is the receiving client's own clock, so
	// now - SampleTimeMs is the exact end-to-end delay of the loop.
	SampleTimeMs uint32
}

// EntitySize is the fixed on-the-wire size of one Entity.
const EntitySize = 4 + 2 + 2 + NameLen + NameLen + 48 + TelemetrySize + 4

// Chat is one line of text. The same shape travels both ways: a client sends
// it with its own id and whatever name it likes, the server overwrites both
// from the session it knows and rebroadcasts to every client - including the
// sender, whose message thereby appears exactly when everyone else sees it.
type Chat struct {
	PlayerID uint32
	Name     string // NameLen bytes on the wire
	Text     string // ChatTextLen bytes on the wire
}

// SnapshotHeaderSize is the snapshot's own header, after the common one.
const SnapshotHeaderSize = 4 + 4 + 2 + 2

// Snapshot is what every client receives each tick.
type Snapshot struct {
	ServerTimeMs uint32
	// LastClientTimeMs is the ClientTimeMs of the most recent State the server
	// received from this client, echoed back. now - LastClientTimeMs is the
	// round trip time, measured with one clock and no synchronisation.
	LastClientTimeMs uint32
	Entities         []Entity
}

// ---------------------------------------------------------------------------
// encoding

type writer struct{ b []byte }

func (w *writer) u16(v uint16) { w.b = binary.LittleEndian.AppendUint16(w.b, v) }
func (w *writer) u32(v uint32) { w.b = binary.LittleEndian.AppendUint32(w.b, v) }
func (w *writer) i32(v int32)  { w.u32(uint32(v)) }
func (w *writer) f32(v float32) {
	w.b = binary.LittleEndian.AppendUint32(w.b, math.Float32bits(v))
}

func (w *writer) name(s string) {
	var buf [NameLen]byte
	copy(buf[:], s) // truncates, and leaves the tail NUL
	if len(s) >= NameLen {
		buf[NameLen-1] = 0
	}
	w.b = append(w.b, buf[:]...)
}

func (w *writer) chatText(s string) {
	var buf [ChatTextLen]byte
	copy(buf[:], s) // truncates, and leaves the tail NUL
	if len(s) >= ChatTextLen {
		buf[ChatTextLen-1] = 0
	}
	w.b = append(w.b, buf[:]...)
}

func (w *writer) transform(t Transform) {
	for _, v := range t.Pos {
		w.f32(v)
	}
	for _, v := range t.Rot {
		w.f32(v)
	}
}

func (w *writer) telemetry(t Telemetry) {
	for _, v := range t.Vel {
		w.f32(v)
	}
	w.f32(t.Speed)
	w.f32(t.RPM)
	w.f32(t.Steer)
	w.i32(t.Gear)
	for _, v := range t.WheelOmega {
		w.f32(v)
	}
}

func (w *writer) header(t uint16) {
	w.u32(Magic)
	w.u16(Version)
	w.u16(t)
}

type reader struct {
	b   []byte
	i   int
	err error
}

func (r *reader) need(n int) bool {
	if r.err != nil {
		return false
	}
	if len(r.b)-r.i < n {
		r.err = ErrShort
		return false
	}
	return true
}

func (r *reader) u16() uint16 {
	if !r.need(2) {
		return 0
	}
	v := binary.LittleEndian.Uint16(r.b[r.i:])
	r.i += 2
	return v
}

func (r *reader) u32() uint32 {
	if !r.need(4) {
		return 0
	}
	v := binary.LittleEndian.Uint32(r.b[r.i:])
	r.i += 4
	return v
}

func (r *reader) i32() int32   { return int32(r.u32()) }
func (r *reader) f32() float32 { return math.Float32frombits(r.u32()) }

func (r *reader) name() string {
	if !r.need(NameLen) {
		return ""
	}
	raw := r.b[r.i : r.i+NameLen]
	r.i += NameLen
	for j, c := range raw {
		if c == 0 {
			return string(raw[:j])
		}
	}
	return string(raw)
}

func (r *reader) chatText() string {
	if !r.need(ChatTextLen) {
		return ""
	}
	raw := r.b[r.i : r.i+ChatTextLen]
	r.i += ChatTextLen
	for j, c := range raw {
		if c == 0 {
			return string(raw[:j])
		}
	}
	return string(raw)
}

func (r *reader) transform() Transform {
	var t Transform
	for j := range t.Pos {
		t.Pos[j] = r.f32()
	}
	for j := range t.Rot {
		t.Rot[j] = r.f32()
	}
	return t
}

func (r *reader) telemetry() Telemetry {
	var t Telemetry
	for j := range t.Vel {
		t.Vel[j] = r.f32()
	}
	t.Speed = r.f32()
	t.RPM = r.f32()
	t.Steer = r.f32()
	t.Gear = r.i32()
	for j := range t.WheelOmega {
		t.WheelOmega[j] = r.f32()
	}
	return t
}

// ParseHeader validates the common prefix and returns the message type.
func ParseHeader(b []byte) (uint16, error) {
	if len(b) < HeaderSize {
		return 0, ErrShort
	}
	if binary.LittleEndian.Uint32(b) != Magic {
		return 0, ErrMagic
	}
	if binary.LittleEndian.Uint16(b[4:]) != Version {
		return 0, ErrVersion
	}
	return binary.LittleEndian.Uint16(b[6:]), nil
}

// EncodeHello builds a Hello datagram.
func EncodeHello(h Hello) []byte {
	w := &writer{}
	w.header(TypeHello)
	w.name(h.Name)
	w.name(h.Car)
	return w.b
}

// DecodeHello parses a Hello datagram (header included).
func DecodeHello(b []byte) (Hello, error) {
	r := &reader{b: b, i: HeaderSize}
	h := Hello{Name: r.name(), Car: r.name()}
	return h, r.err
}

// EncodeWelcome builds a Welcome datagram.
func EncodeWelcome(v Welcome) []byte {
	w := &writer{}
	w.header(TypeWelcome)
	w.u32(v.PlayerID)
	w.u16(v.TickRateHz)
	w.u16(v.EchoDelayMs)
	w.u32(v.ServerTimeMs)
	return w.b
}

// DecodeWelcome parses a Welcome datagram.
func DecodeWelcome(b []byte) (Welcome, error) {
	r := &reader{b: b, i: HeaderSize}
	v := Welcome{
		PlayerID:    r.u32(),
		TickRateHz:  r.u16(),
		EchoDelayMs: r.u16(),
	}
	v.ServerTimeMs = r.u32()
	return v, r.err
}

// EncodeState builds a State datagram.
func EncodeState(s State) []byte {
	w := &writer{}
	w.header(TypeState)
	w.u32(s.PlayerID)
	w.u32(s.Seq)
	w.u32(s.ClientTimeMs)
	w.transform(s.Transform)
	w.telemetry(s.Telemetry)
	return w.b
}

// DecodeState parses a State datagram.
func DecodeState(b []byte) (State, error) {
	r := &reader{b: b, i: HeaderSize}
	s := State{
		PlayerID:     r.u32(),
		Seq:          r.u32(),
		ClientTimeMs: r.u32(),
	}
	s.Transform = r.transform()
	s.Telemetry = r.telemetry()
	return s, r.err
}

// EncodeChat builds a Chat datagram.
func EncodeChat(c Chat) []byte {
	w := &writer{}
	w.header(TypeChat)
	w.u32(c.PlayerID)
	w.name(c.Name)
	w.chatText(c.Text)
	return w.b
}

// DecodeChat parses a Chat datagram.
func DecodeChat(b []byte) (Chat, error) {
	r := &reader{b: b, i: HeaderSize}
	c := Chat{PlayerID: r.u32(), Name: r.name(), Text: r.chatText()}
	return c, r.err
}

// EncodeSnapshot builds a Snapshot datagram.
func EncodeSnapshot(s Snapshot) []byte {
	w := &writer{b: make([]byte, 0, HeaderSize+SnapshotHeaderSize+len(s.Entities)*EntitySize)}
	w.header(TypeSnapshot)
	w.u32(s.ServerTimeMs)
	w.u32(s.LastClientTimeMs)
	w.u16(uint16(len(s.Entities)))
	w.u16(0) // padding, keeps the entity array 4-byte aligned
	for _, e := range s.Entities {
		w.u32(e.ID)
		w.u16(e.Flags)
		w.u16(0)
		w.name(e.Name)
		w.name(e.Car)
		w.transform(e.Transform)
		w.telemetry(e.Telemetry)
		w.u32(e.SampleTimeMs)
	}
	return w.b
}

// DecodeSnapshot parses a Snapshot datagram.
func DecodeSnapshot(b []byte) (Snapshot, error) {
	r := &reader{b: b, i: HeaderSize}
	s := Snapshot{
		ServerTimeMs:     r.u32(),
		LastClientTimeMs: r.u32(),
	}
	n := int(r.u16())
	_ = r.u16()
	if r.err != nil {
		return s, r.err
	}
	s.Entities = make([]Entity, 0, n)
	for i := 0; i < n; i++ {
		var e Entity
		e.ID = r.u32()
		e.Flags = r.u16()
		_ = r.u16()
		e.Name = r.name()
		e.Car = r.name()
		e.Transform = r.transform()
		e.Telemetry = r.telemetry()
		e.SampleTimeMs = r.u32()
		if r.err != nil {
			return s, r.err
		}
		s.Entities = append(s.Entities, e)
	}
	return s, nil
}
