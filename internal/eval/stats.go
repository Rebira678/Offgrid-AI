package eval

import (
	"fmt"
	"math"
	"sort"
)

// Proportion is "K out of N", for example 27 of 30 cases correct.
type Proportion struct {
	K, N int
}

// Rate is K/N, or 0 when there were no cases.
func (p Proportion) Rate() float64 {
	if p.N == 0 {
		return 0
	}
	return float64(p.K) / float64(p.N)
}

// Wilson returns the 95% Wilson score confidence interval for the true rate.
//
// Why this exists: with 30 test cases, "90% accurate" does not mean the model
// is 90% accurate. It means it got 27 of 30, and the plausible range for its
// real accuracy is roughly 74% to 97%. Two models at 83% and 90% cannot be told
// apart with 30 cases, and claiming "B beats A" would be wrong. The interval
// shows how much a number can be trusted. Wilson is used instead of the simpler
// textbook formula because it behaves sensibly near 0% and 100%, where small
// samples live.
func (p Proportion) Wilson() (lo, hi float64) {
	if p.N == 0 {
		return 0, 0
	}
	const z = 1.96 // 95% confidence
	n := float64(p.N)
	phat := p.Rate()
	denom := 1 + z*z/n
	centre := (phat + z*z/(2*n)) / denom
	half := z * math.Sqrt(phat*(1-phat)/n+z*z/(4*n*n)) / denom
	return math.Max(0, centre-half), math.Min(1, centre+half)
}

// String renders "27/30 = 90% [74-97]".
func (p Proportion) String() string {
	if p.N == 0 {
		return "n/a"
	}
	lo, hi := p.Wilson()
	return fmt.Sprintf("%d/%d = %.0f%% [%.0f-%.0f]", p.K, p.N, 100*p.Rate(), 100*lo, 100*hi)
}

func median(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}
