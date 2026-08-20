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

func TestDecodeRejectsTruncatedState(t *testing.T) {
	b := EncodeState(State{PlayerID: 1, Transform: sampleTransform()})
	if _, err := DecodeState(b[:HeaderSize+4]); err == nil {
		t.Fatal("a truncated state decoded without error")
	}
}
