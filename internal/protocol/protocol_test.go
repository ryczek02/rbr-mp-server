package protocol

import (
	"reflect"
	"strings"
	"testing"
)

func sampleTransform() Transform {
	return Transform{
		Pos: [3]float32{123.5, -67.25, 8.125},
		Rot: [9]float32{1, 0, 0, 0, 1, 0, 0, 0, 1},
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	b := EncodeHello(Hello{Name: "tester"})
	got, err := ParseHeader(b)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if got != TypeHello {
		t.Fatalf("type %d, want %d", got, TypeHello)
	}
}

func TestMagicIsFourAsciiBytes(t *testing.T) {
	// The client side reads this in a debugger often enough that it is worth
	// keeping it human-readable in a hex dump.
	b := EncodeHello(Hello{})
	if string(b[:4]) != "ORMP" {
		t.Fatalf("magic reads as %q, want \"ORMP\"", b[:4])
	}
}

func TestParseHeaderRejectsRubbish(t *testing.T) {
	if _, err := ParseHeader([]byte{1, 2, 3}); err != ErrShort {
		t.Errorf("short datagram: got %v, want ErrShort", err)
	}
	bad := EncodeHello(Hello{Name: "x"})
	bad[0] ^= 0xFF
	if _, err := ParseHeader(bad); err != ErrMagic {
		t.Errorf("bad magic: got %v, want ErrMagic", err)
	}
	wrongVer := EncodeHello(Hello{Name: "x"})
	wrongVer[4] = 99
	if _, err := ParseHeader(wrongVer); err != ErrVersion {
		t.Errorf("bad version: got %v, want ErrVersion", err)
	}
}

func TestHelloRoundTrip(t *testing.T) {
	in := Hello{Name: "Łukasz", Car: "XSARA"}
	out, err := DecodeHello(EncodeHello(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Name != in.Name {
		t.Fatalf("name %q, want %q", out.Name, in.Name)
	}
	if out.Car != in.Car {
		t.Fatalf("car %q, want %q", out.Car, in.Car)
	}
}

func TestNameIsTruncatedNotOverflowed(t *testing.T) {
	long := strings.Repeat("x", NameLen*2)
	b := EncodeHello(Hello{Name: long})
	if len(b) != HeaderSize+2*NameLen {
		t.Fatalf("datagram is %d bytes, want %d", len(b), HeaderSize+2*NameLen)
	}
	out, err := DecodeHello(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Name) >= NameLen {
		t.Fatalf("name came back %d bytes, must be NUL-terminated within %d",
			len(out.Name), NameLen)
	}
}

func TestChatRoundTrip(t *testing.T) {
	in := Chat{PlayerID: 3, Name: "Łukasz", Text: "gg, see you at the split"}
	out, err := DecodeChat(EncodeChat(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out != in {
		t.Fatalf("got %+v, want %+v", out, in)
	}
	if got, want := len(EncodeChat(in)), HeaderSize+4+NameLen+ChatTextLen; got != want {
		t.Fatalf("datagram is %d bytes, want %d", got, want)
	}
}

func TestChatTextIsTruncatedNotOverflowed(t *testing.T) {
	long := strings.Repeat("y", ChatTextLen*2)
	b := EncodeChat(Chat{Text: long})
	if len(b) != HeaderSize+4+NameLen+ChatTextLen {
		t.Fatalf("datagram is %d bytes, want %d", len(b), HeaderSize+4+NameLen+ChatTextLen)
	}
	out, err := DecodeChat(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Text) >= ChatTextLen {
		t.Fatalf("text came back %d bytes, must be NUL-terminated within %d",
			len(out.Text), ChatTextLen)
	}
}

func TestWelcomeRoundTrip(t *testing.T) {
	in := Welcome{PlayerID: 7, TickRateHz: 30, EchoDelayMs: 1000, ServerTimeMs: 123456}
	out, err := DecodeWelcome(EncodeWelcome(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out != in {
		t.Fatalf("got %+v, want %+v", out, in)
	}
}

func sampleTelemetry() Telemetry {
	return Telemetry{
		Vel:        [3]float32{27.5, -1.25, 0.5},
		Speed:      27.5,
		RPM:        6450,
		Steer:      -0.35,
		Gear:       4,
		WheelOmega: [4]float32{88, 88.5, 91, 90.5},
	}
}

func TestStateRoundTrip(t *testing.T) {
	in := State{
		PlayerID:     3,
		Seq:          4242,
		ClientTimeMs: 999999,
		Transform:    sampleTransform(),
		Telemetry:    sampleTelemetry(),
	}
	out, err := DecodeState(EncodeState(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("got %+v, want %+v", out, in)
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	in := Snapshot{
		ServerTimeMs:     5000,
		LastClientTimeMs: 4321,
		Entities: []Entity{
			{ID: 1, Name: "Player 1", Car: "XSARA", Transform: sampleTransform(),
				Telemetry: sampleTelemetry(), SampleTimeMs: 100},
			{ID: 1 | 0x8000_0000, Flags: FlagEcho, Name: "Player 1 (echo)", Car: "XSARA",
				Transform: sampleTransform(), Telemetry: sampleTelemetry(), SampleTimeMs: 50},
		},
	}
	b := EncodeSnapshot(in)
	if want := HeaderSize + SnapshotHeaderSize + 2*EntitySize; len(b) != want {
		t.Fatalf("snapshot is %d bytes, want %d - the C++ client assumes the fixed layout",
			len(b), want)
	}
	out, err := DecodeSnapshot(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("got %+v, want %+v", out, in)
	}
}

func TestEmptySnapshotRoundTrip(t *testing.T) {
	out, err := DecodeSnapshot(EncodeSnapshot(Snapshot{ServerTimeMs: 1}))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Entities) != 0 {
		t.Fatalf("got %d entities, want none", len(out.Entities))
	}
}

func TestDecodeRejectsTruncatedSnapshot(t *testing.T) {
	b := EncodeSnapshot(Snapshot{Entities: []Entity{{ID: 1, Transform: sampleTransform()}}})
	if _, err := DecodeSnapshot(b[:len(b)-4]); err == nil {
		t.Fatal("a truncated snapshot decoded without error")
	}
}

func TestPingPongRoundTrip(t *testing.T) {
	pb := EncodePing(Ping{Token: 0xDEADBEEF})
	if len(pb) != HeaderSize+4 {
		t.Fatalf("ping is %d bytes, want %d", len(pb), HeaderSize+4)
	}
	p, err := DecodePing(pb)
	if err != nil || p.Token != 0xDEADBEEF {
		t.Fatalf("ping came back %+v (%v), want token 0xDEADBEEF", p, err)
	}

	gb := EncodePong(Pong{Token: 42})
	if len(gb) != HeaderSize+4 {
		t.Fatalf("pong is %d bytes, want %d", len(gb), HeaderSize+4)
	}
	if msgType, _ := ParseHeader(gb); msgType != TypePong {
		t.Fatalf("pong header type %d, want %d", msgType, TypePong)
	}
	g, err := DecodePong(gb)
	if err != nil || g.Token != 42 {
		t.Fatalf("pong came back %+v (%v), want token 42", g, err)
	}
}

func TestKickRoundTrip(t *testing.T) {
	b := EncodeKick(Kick{Reason: "banned: bad manners"})
	if len(b) != HeaderSize+KickReasonLen {
		t.Fatalf("kick is %d bytes, want %d", len(b), HeaderSize+KickReasonLen)
	}
	k, err := DecodeKick(b)
	if err != nil || k.Reason != "banned: bad manners" {
		t.Fatalf("kick came back %+v (%v)", k, err)
	}

	long := strings.Repeat("z", KickReasonLen*2)
	b = EncodeKick(Kick{Reason: long})
	if len(b) != HeaderSize+KickReasonLen {
		t.Fatalf("long kick is %d bytes, want %d", len(b), HeaderSize+KickReasonLen)
	}
	k, err = DecodeKick(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(k.Reason) >= KickReasonLen {
		t.Fatalf("reason came back %d bytes, must be NUL-terminated within %d",
			len(k.Reason), KickReasonLen)
	}
}

// The C++ client reads the snapshot with fixed offsets, so the exact byte
// layout is the contract. Build the expected bytes by hand and compare.
func TestSnapshotWireOffsets(t *testing.T) {
	if EntitySize != 156 {
		t.Fatalf("EntitySize is %d, want 156", EntitySize)
	}
	e := Entity{ID: 7, Flags: 0, Name: "n", Car: "c",
		SampleTimeMs: 0x11223344, PingMs: 0xABCD}
	b := EncodeSnapshot(Snapshot{
		ServerTimeMs: 0x01020304, LastClientTimeMs: 0x05060708,
		SelfPingMs: 0x1234, Entities: []Entity{e},
	})
	if want := HeaderSize + SnapshotHeaderSize + EntitySize; len(b) != want {
		t.Fatalf("snapshot is %d bytes, want %d", len(b), want)
	}
	// Header: magic "ORMP", version 3, type 4.
	if string(b[:4]) != "ORMP" || b[4] != 3 || b[5] != 0 || b[6] != 4 || b[7] != 0 {
		t.Fatalf("header bytes are % x", b[:8])
	}
	// Snapshot header at 8: serverTime, lastClientTime, count, selfPing.
	check := func(off int, want []byte, what string) {
		t.Helper()
		got := b[off : off+len(want)]
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s at offset %d is % x, want % x", what, off, got, want)
			}
		}
	}
	check(8, []byte{0x04, 0x03, 0x02, 0x01}, "server time")
	check(12, []byte{0x08, 0x07, 0x06, 0x05}, "echoed client time")
	check(16, []byte{0x01, 0x00}, "entity count")
	check(18, []byte{0x34, 0x12}, "self ping")
	// Entity starts at 20: id 0, flags 4, pad 6, name 8, car 32,
	// transform 56, telemetry 104, sampleTime 148, ping 152, pad 154.
	const eb = 20
	check(eb+0, []byte{0x07, 0x00, 0x00, 0x00}, "entity id")
	check(eb+8, []byte{'n', 0x00}, "entity name")
	check(eb+32, []byte{'c', 0x00}, "entity car")
	check(eb+148, []byte{0x44, 0x33, 0x22, 0x11}, "entity sample time")
	check(eb+152, []byte{0xCD, 0xAB}, "entity ping")
	check(eb+154, []byte{0x00, 0x00}, "entity tail padding")

	out, err := DecodeSnapshot(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.SelfPingMs != 0x1234 || out.Entities[0].PingMs != 0xABCD {
		t.Fatalf("pings came back %d/%d, want 0x1234/0xABCD",
			out.SelfPingMs, out.Entities[0].PingMs)
	}
}

func TestDecodeRejectsTruncatedState(t *testing.T) {
	b := EncodeState(State{PlayerID: 1, Transform: sampleTransform()})
	if _, err := DecodeState(b[:HeaderSize+4]); err == nil {
		t.Fatal("a truncated state decoded without error")
	}
}
