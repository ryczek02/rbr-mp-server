package server

import (
	"math"
	"testing"
	"time"

	"github.com/lukaszryczko/rbr-mp-server/internal/protocol"
)

func identity() [9]float32 {
	return [9]float32{1, 0, 0, 0, 1, 0, 0, 0, 1}
}

// A car driving in a straight line at 10 m/s, sampled at 60 Hz.
func straightLine(base time.Time, n int) *History {
	h := NewHistory(10 * time.Second)
	for i := 0; i < n; i++ {
		at := base.Add(time.Duration(i) * time.Second / 60)
		h.Add(Sample{
			RecvAt:       at,
			ClientTimeMs: uint32(i * 1000 / 60),
			Transform: protocol.Transform{
				Pos: [3]float32{float32(i) * 10.0 / 60.0, 0, 0},
				Rot: identity(),
			},
			Speed: 10,
		})
	}
	return h
}

func TestAtEmpty(t *testing.T) {
	h := NewHistory(time.Second)
	if _, ok := h.At(time.Now()); ok {
		t.Fatal("empty history returned a sample")
	}
}

func TestAtBeforeStartIsNotReady(t *testing.T) {
	base := time.Now()
	h := straightLine(base, 10)
	// This is the case that matters: for the first `delay` after a client
	// connects there is nothing to replay, and the echo player must simply not
	// exist rather than appear at the origin.
	if _, ok := h.At(base.Add(-time.Second)); ok {
		t.Fatal("a time before the first sample should not resolve")
	}
}

func TestAtClampsToNewest(t *testing.T) {
	base := time.Now()
	h := straightLine(base, 10)
	s, ok := h.At(base.Add(time.Hour))
	if !ok {
		t.Fatal("a time after the last sample should clamp, not fail")
	}
	if want := float32(9) * 10.0 / 60.0; s.Transform.Pos[0] != want {
		t.Fatalf("got x=%v, want the newest sample's %v", s.Transform.Pos[0], want)
	}
}

func TestAtInterpolatesBetweenSamples(t *testing.T) {
	base := time.Now()
	h := straightLine(base, 60)

	// Exactly half a frame between sample 10 and 11.
	at := base.Add(10*time.Second/60 + time.Second/120)
	s, ok := h.At(at)
	if !ok {
		t.Fatal("lookup inside the range failed")
	}
	want := float32(10.5) * 10.0 / 60.0
	if math.Abs(float64(s.Transform.Pos[0]-want)) > 1e-4 {
		t.Fatalf("got x=%v, want %v (should be interpolated, not snapped)", s.Transform.Pos[0], want)
	}
	// The client clock must be interpolated too, or the measured delay jitters
	// by a whole frame.
	if wantMs := uint32(10*1000/60) + 8; s.ClientTimeMs < wantMs-2 || s.ClientTimeMs > wantMs+2 {
		t.Fatalf("got clientTime=%v, want about %v", s.ClientTimeMs, wantMs)
	}
}

// The point of the whole exercise: asking for "one second ago" gives back
// where the car actually was one second ago.
func TestAtOneSecondBehindIsWhereTheCarWas(t *testing.T) {
	base := time.Now()
	const seconds = 5
	h := straightLine(base, seconds*60)

	now := base.Add(seconds * time.Second)
	s, ok := h.At(now.Add(-time.Second))
	if !ok {
		t.Fatal("one second back should be in range")
	}
	// 10 m/s for 4 seconds.
	want := float32(40)
	if math.Abs(float64(s.Transform.Pos[0]-want)) > 0.2 {
		t.Fatalf("echo is at x=%v, want about %v", s.Transform.Pos[0], want)
	}
}

func TestAddIgnoresOutOfOrder(t *testing.T) {
	base := time.Now()
	h := NewHistory(time.Second)
	h.Add(Sample{RecvAt: base.Add(time.Second), Transform: protocol.Transform{Rot: identity()}})
	h.Add(Sample{RecvAt: base, Transform: protocol.Transform{Rot: identity()}}) // older
	if h.Len() != 1 {
		t.Fatalf("out-of-order sample was kept: len=%d", h.Len())
	}
}

func TestOldSamplesAreDropped(t *testing.T) {
	base := time.Now()
	h := NewHistory(time.Second)
	for i := 0; i < 300; i++ { // 5 seconds at 60 Hz
		h.Add(Sample{
			RecvAt:    base.Add(time.Duration(i) * time.Second / 60),
			Transform: protocol.Transform{Rot: identity()},
		})
	}
	// One second of 60 Hz plus the one kept for interpolation.
	if h.Len() > 64 {
		t.Fatalf("history grew past its window: %d samples", h.Len())
	}
	if h.Len() < 55 {
		t.Fatalf("history dropped too much: %d samples", h.Len())
	}
}

func TestOrthonormalizeRepairsALerpedRotation(t *testing.T) {
	// Halfway between identity and a 90 degree yaw, lerped component-wise -
	// which is not a rotation until it is fixed up.
	yaw90 := [9]float32{0, 1, 0, -1, 0, 0, 0, 0, 1}
	var m [9]float32
	id := identity()
	for i := range m {
		m[i] = (id[i] + yaw90[i]) / 2
	}
	orthonormalize(&m)

	rows := [3][3]float32{
		{m[0], m[1], m[2]},
		{m[3], m[4], m[5]},
		{m[6], m[7], m[8]},
	}
	for i, r := range rows {
		if l := math.Sqrt(float64(dot(r, r))); math.Abs(l-1) > 1e-5 {
			t.Errorf("row %d has length %v, want 1", i, l)
		}
	}
	for i := 0; i < 3; i++ {
		for j := i + 1; j < 3; j++ {
			if d := math.Abs(float64(dot(rows[i], rows[j]))); d > 1e-5 {
				t.Errorf("rows %d and %d are not perpendicular (dot %v)", i, j, d)
			}
		}
	}
	// And it must still be a rotation, not a reflection: det > 0.
	det := float64(rows[0][0]*(rows[1][1]*rows[2][2]-rows[1][2]*rows[2][1]) -
		rows[0][1]*(rows[1][0]*rows[2][2]-rows[1][2]*rows[2][0]) +
		rows[0][2]*(rows[1][0]*rows[2][1]-rows[1][1]*rows[2][0]))
	if math.Abs(det-1) > 1e-5 {
		t.Errorf("determinant %v, want +1", det)
	}
}

func TestOrthonormalizeLeavesDegenerateInputAlone(t *testing.T) {
	zero := [9]float32{}
	m := zero
	orthonormalize(&m)
	if m != zero {
		t.Fatalf("a degenerate matrix should be left as-is, got %v", m)
	}
}
