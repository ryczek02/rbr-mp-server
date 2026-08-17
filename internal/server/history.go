package server

import (
	"math"
	"time"

	"github.com/lukaszryczko/rbr-mp-server/internal/protocol"
)

// Sample is one received pose, tagged with when the SERVER received it.
//
// Replay is driven by the server's own receive times, never by the client's
// clock: the two are not synchronised and a client could send anything. The
// client's timestamp is carried along untouched so it can be handed back, which
// is what lets the client measure the true age of what it gets without either
// side knowing the other's clock.
type Sample struct {
	RecvAt       time.Time
	ClientTimeMs uint32
	Transform    protocol.Transform
	Speed        float32
}

// History is a bounded, time-ordered ring of samples for one client.
type History struct {
	samples []Sample
	maxAge  time.Duration
	maxLen  int
}

// NewHistory keeps roughly maxAge worth of samples, hard-capped so a client
// that floods cannot grow it without bound.
func NewHistory(maxAge time.Duration) *History {
	return &History{maxAge: maxAge, maxLen: 4096}
}

// Add appends a sample and drops anything older than maxAge.
//
// Out-of-order datagrams (UDP reorders) are dropped rather than inserted: at
// 30-60 Hz the next one is along in a few milliseconds, and keeping the slice
// sorted is what makes the lookup below a clean binary search.
func (h *History) Add(s Sample) {
	if n := len(h.samples); n > 0 && !s.RecvAt.After(h.samples[n-1].RecvAt) {
		return
	}
	h.samples = append(h.samples, s)

	cutoff := s.RecvAt.Add(-h.maxAge)
	drop := 0
	for drop < len(h.samples) && h.samples[drop].RecvAt.Before(cutoff) {
		drop++
	}
	// Keep one sample older than the cutoff, so a lookup exactly at the cutoff
	// still has something to interpolate from.
	if drop > 0 {
		drop--
	}
	if over := len(h.samples) - h.maxLen; over > drop {
		drop = over
	}
	if drop > 0 {
		h.samples = append(h.samples[:0], h.samples[drop:]...)
	}
}

// Len is the number of samples held.
func (h *History) Len() int { return len(h.samples) }

// Latest returns the newest sample.
func (h *History) Latest() (Sample, bool) {
	if len(h.samples) == 0 {
		return Sample{}, false
	}
	return h.samples[len(h.samples)-1], true
}

// At returns the pose the client had at time `at`, interpolated between the two
// samples that bracket it.
//
// ok is false when `at` predates everything held - which is exactly what
// happens for the first `delay` after a client connects, and correctly means
// "this player does not exist yet" rather than "it is at the origin".
// A time later than the newest sample is clamped to the newest, so a client
// that stops sending freezes in place instead of disappearing.
func (h *History) At(at time.Time) (Sample, bool) {
	n := len(h.samples)
	if n == 0 {
		return Sample{}, false
	}
	if at.Before(h.samples[0].RecvAt) {
		return Sample{}, false
	}
	last := h.samples[n-1]
	if !at.Before(last.RecvAt) {
		return last, true
	}

	// Binary search for the first sample strictly after `at`; the one before
	// it is the other end of the bracket.
	lo, hi := 0, n-1
	for lo < hi {
		mid := (lo + hi) / 2
		if h.samples[mid].RecvAt.After(at) {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	b := h.samples[lo]
	a := h.samples[lo-1]

	span := b.RecvAt.Sub(a.RecvAt)
	if span <= 0 {
		return b, true
	}
	t := float32(at.Sub(a.RecvAt).Seconds() / span.Seconds())
	return lerpSample(a, b, t), true
}

func lerp(a, b, t float32) float32 { return a + (b-a)*t }

func lerpSample(a, b Sample, t float32) Sample {
	out := Sample{RecvAt: a.RecvAt.Add(time.Duration(float64(b.RecvAt.Sub(a.RecvAt)) * float64(t)))}

	// Wrap-safe: the client's clock is a uint32 of milliseconds and will wrap
	// after 49 days, so interpolate the DIFFERENCE, not the absolute values.
	delta := int32(b.ClientTimeMs - a.ClientTimeMs)
	out.ClientTimeMs = a.ClientTimeMs + uint32(float32(delta)*t)

	for i := range out.Transform.Pos {
		out.Transform.Pos[i] = lerp(a.Transform.Pos[i], b.Transform.Pos[i], t)
	}
	for i := range out.Transform.Rot {
		out.Transform.Rot[i] = lerp(a.Transform.Rot[i], b.Transform.Rot[i], t)
	}
	orthonormalize(&out.Transform.Rot)
	out.Speed = lerp(a.Speed, b.Speed, t)
	return out
}

// orthonormalize repairs a rotation that was interpolated component-wise.
//
// Lerping two rotation matrices does not produce a rotation - the result is
// slightly shrunk and skewed - and a car drawn with it would be subtly
// squashed. Gram-Schmidt puts it back: normalise the first row, make the
// second perpendicular to it, and take the third as their cross product. Over
// the few milliseconds between two samples the two rows are nearly parallel to
// their originals, so this is accurate and much cheaper than going through
// quaternions.
func orthonormalize(m *[9]float32) {
	x := [3]float32{m[0], m[1], m[2]}
	y := [3]float32{m[3], m[4], m[5]}

	if !normalize(&x) {
		return // degenerate input: leave it alone rather than invent a pose
	}
	d := dot(x, y)
	for i := range y {
		y[i] -= d * x[i]
	}
	if !normalize(&y) {
		return
	}
	z := cross(x, y)

	m[0], m[1], m[2] = x[0], x[1], x[2]
	m[3], m[4], m[5] = y[0], y[1], y[2]
	m[6], m[7], m[8] = z[0], z[1], z[2]
}

func normalize(v *[3]float32) bool {
	l := float32(math.Sqrt(float64(v[0]*v[0] + v[1]*v[1] + v[2]*v[2])))
	if l < 1e-6 {
		return false
	}
	v[0] /= l
	v[1] /= l
	v[2] /= l
	return true
}

func dot(a, b [3]float32) float32 { return a[0]*b[0] + a[1]*b[1] + a[2]*b[2] }

func cross(a, b [3]float32) [3]float32 {
	return [3]float32{
		a[1]*b[2] - a[2]*b[1],
		a[2]*b[0] - a[0]*b[2],
		a[0]*b[1] - a[1]*b[0],
	}
}
