package main

// Downsampled display traces for the run-detail dashboard. Full resolution
// lives in the pressure/ and chromatogram/ .npz artifacts; these compact arrays
// (~500 pts) go in Postgres so Grafana can plot a run's pressure and
// chromatogram without an S3 round-trip. Retention time is derived in SQL from
// run_min and the array index (both traces span ~0..run_min).

const traceN = 500

// downsample reduces x to at most n points by nearest-index striding,
// preserving the endpoints.
func downsample(x []float64, n int) []float64 {
	if len(x) <= n {
		return x
	}
	out := make([]float64, n)
	step := float64(len(x)-1) / float64(n-1)
	for i := 0; i < n; i++ {
		out[i] = x[int(float64(i)*step+0.5)]
	}
	return out
}

// nearestLambda returns the index of the wavelength closest to target nm.
func nearestLambda(lambdas []float64, target float64) int {
	idx := 0
	for i := range lambdas {
		if abs(lambdas[i]-target) < abs(lambdas[idx]-target) {
			idx = i
		}
	}
	return idx
}

// pressureTraceOf pulls the pressure channel out of the decoded channels and
// downsamples it for display.
func pressureTraceOf(chans []Channel) []float64 {
	for _, c := range chans {
		if c.Name == "p" {
			return downsample(c.Data, traceN)
		}
	}
	return nil
}

// dadSurface downsamples the full PDA matrix to a (wl, rt, z-flat rt-major)
// grid for the 3D DAD surface panel — full detector range, ~90 wl × ~60 rt.
func dadSurface(times, lambdas []float64, mat []int32, nrows, nlambda int) (wl, rt, z []float64) {
	ws := nlambda / 90
	if ws < 1 {
		ws = 1
	}
	ts := nrows / 60
	if ts < 1 {
		ts = 1
	}
	for j := 0; j < nlambda; j += ws {
		wl = append(wl, round(lambdas[j], 1))
	}
	for i := 0; i < nrows; i += ts {
		rt = append(rt, round(times[i], 3))
	}
	for i := 0; i < nrows; i += ts {
		for j := 0; j < nlambda; j += ws {
			z = append(z, round(float64(mat[i*nlambda+j]), 1))
		}
	}
	return
}
