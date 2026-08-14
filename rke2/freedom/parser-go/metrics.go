package main

// Per-run scalar metrics — port of hplc-tools analysis/pressure_aug.py metrics
// plus stroke_amp (pump-stroke FFT amplitude). Numpy semantics reproduced
// exactly: convolve 'same' edge attenuation and rfft/normalization.

import (
	"math"
	"math/cmplx"
	"sort"

	"gonum.org/v1/gonum/dsp/fourier"
)

type Metrics struct {
	SystemID string // embedded LabSolutions system string (HPLC/DESKTOP-*) — demoted; instrument identity is the bucket folder (helsa/hope)
	AcqAt    *string
	RunMin     *float64
	PStart     *float64
	PMax       *float64
	PMin       *float64
	P2Min      *float64
	Ripple     *float64
	MaxDrop    *float64
	StrokeAmp  *float64
	FlowMed    *float64
	FlowStd    *float64
	OvenMed    *float64
}

func fptr(v float64) *float64 { return &v }

func mean(x []float64) float64 {
	if len(x) == 0 {
		return 0
	}
	s := 0.0
	for _, v := range x {
		s += v
	}
	return s / float64(len(x))
}

func stddev(x []float64) float64 { // population std (numpy ddof=0)
	if len(x) == 0 {
		return 0
	}
	m := mean(x)
	s := 0.0
	for _, v := range x {
		d := v - m
		s += d * d
	}
	return math.Sqrt(s / float64(len(x)))
}

func median(x []float64) float64 {
	if len(x) == 0 {
		return 0
	}
	c := append([]float64(nil), x...)
	sort.Float64s(c)
	n := len(c)
	if n%2 == 1 {
		return c[n/2]
	}
	return (c[n/2-1] + c[n/2]) / 2
}

func minMax(x []float64) (float64, float64) {
	lo, hi := x[0], x[0]
	for _, v := range x {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	return lo, hi
}

// smoothSame reproduces numpy.convolve(seg, ones(w)/w, 'same'): a centered
// w-wide moving average where edge samples divide by w without renormalizing
// (fewer terms → attenuated edges).
func smoothSame(seg []float64, w int) []float64 {
	out := make([]float64, len(seg))
	half := (w - 1) / 2
	for i := range seg {
		lo := i - half
		if lo < 0 {
			lo = 0
		}
		hi := i + half
		if hi > len(seg)-1 {
			hi = len(seg) - 1
		}
		s := 0.0
		for j := lo; j <= hi; j++ {
			s += seg[j]
		}
		out[i] = s / float64(w)
	}
	return out
}

func argMinAbs(t []float64, target float64) int {
	best, bi := math.Inf(1), 0
	for i, v := range t {
		if d := math.Abs(v - target); d < best {
			best, bi = d, i
		}
	}
	return bi
}

// segmentWhere returns p[i] for indices where lo < t[i] < hi.
func segmentWhere(t, p []float64, lo, hi float64) []float64 {
	var seg []float64
	for i := range t {
		if t[i] > lo && t[i] < hi {
			seg = append(seg, p[i])
		}
	}
	return seg
}

func diffMin(x []float64) float64 {
	m := math.Inf(1)
	for i := 1; i < len(x); i++ {
		if d := x[i] - x[i-1]; d < m {
			m = d
		}
	}
	return m
}

// strokeAmp: FFT amplitude of the 0.2-0.3 Hz pump-stroke band of the
// high-passed pressure in the steady 2-14 min window (~1 Hz sampling).
func strokeAmp(t, p []float64) *float64 {
	// window and its dt
	var seg, tw []float64
	for i := range t {
		if t[i] > 2 && t[i] < 14 {
			seg = append(seg, p[i])
			tw = append(tw, t[i])
		}
	}
	if len(seg) < 240 {
		return nil
	}
	dts := make([]float64, len(tw)-1)
	for i := 1; i < len(tw); i++ {
		dts[i-1] = tw[i] - tw[i-1]
	}
	dt := median(dts) * 60.0 // minutes -> seconds
	if dt <= 0 {
		return nil
	}
	fs := 1.0 / dt
	// high-pass: subtract 31-wide moving average
	sm := smoothSame(seg, 31)
	hp := make([]float64, len(seg))
	for i := range seg {
		hp[i] = seg[i] - sm[i]
	}
	n := len(hp)
	fft := fourier.NewFFT(n)
	coeff := fft.Coefficients(nil, hp)
	var maxAmp float64
	found := false
	for k := range coeff {
		f := fft.Freq(k) * fs // Hz
		if f >= 0.2 && f <= 0.3 {
			amp := cmplx.Abs(coeff[k]) / float64(n)
			if !found || amp > maxAmp {
				maxAmp = amp
				found = true
			}
		}
	}
	if !found {
		return nil
	}
	r := round(maxAmp, 3)
	return &r
}

func round(v float64, dp int) float64 {
	p := math.Pow(10, float64(dp))
	return math.Round(v*p) / p
}

// computeMetrics decodes channels and computes the scalar metric set.
func computeMetrics(streams map[string][]byte) (*Metrics, []Channel, error) {
	t, p, err := readStatusChannel(streams, 1)
	if err != nil || len(p) < 60 {
		return nil, nil, errNoPressure
	}
	_, pb, _ := readStatusChannel(streams, 3)
	_, fl, _ := readStatusChannel(streams, 5)
	_, ov, _ := readStatusChannel(streams, 6)

	m := &Metrics{SystemID: instrument(streams)}
	if at, ok := acqTime(streams); ok {
		s := at.Format("2006-01-02T15:04:05.000000Z07:00")
		m.AcqAt = &s
	}
	m.RunMin = fptr(round(t[len(t)-1], 2))
	m.PStart = fptr(round(mean(p[:30]), 1))
	lo, hi := minMax(p)
	m.PMax = fptr(round(hi, 1))
	m.PMin = fptr(round(lo, 1))

	i2 := argMinAbs(t, 2.0)
	a := i2 - 15
	if a < 0 {
		a = 0
	}
	b := i2 + 15
	if b > len(p) {
		b = len(p)
	}
	m.P2Min = fptr(round(mean(p[a:b]), 1))

	seg := segmentWhere(t, p, 2, 14)
	if len(seg) > 120 {
		sm := smoothSame(seg, 31)
		resid := make([]float64, len(seg))
		for i := range seg {
			resid[i] = seg[i] - sm[i]
		}
		m.Ripple = fptr(round(stddev(resid), 2))
		m.MaxDrop = fptr(round(diffMin(seg), 1))
	}
	m.StrokeAmp = strokeAmp(t, p)
	if len(fl) > 0 {
		m.FlowMed = fptr(round(median(fl), 1))
		m.FlowStd = fptr(round(stddev(fl), 2))
	}
	if len(ov) > 0 {
		m.OvenMed = fptr(round(median(ov), 1))
	}

	chans := []Channel{{"t", t}, {"p", p}, {"pb", pb}, {"flow", fl}, {"oven", ov}}
	return m, chans, nil
}
