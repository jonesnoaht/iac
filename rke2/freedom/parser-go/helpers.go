package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
)

func readAll(r io.Reader) []byte {
	b, _ := io.ReadAll(r)
	return b
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// validateFile decodes a single local .lcd and prints its metrics — a local
// harness to confirm the Go decode matches the Python reference. Invoked with:
//
//	go run . validate path/to/file.lcd
func validateFile(path string) {
	blob, err := os.ReadFile(path)
	if err != nil {
		fmt.Println("read:", err)
		os.Exit(1)
	}
	streams, err := oleStreams(blob)
	if err != nil {
		fmt.Println("ole:", err)
		os.Exit(1)
	}
	m, chans, err := computeMetrics(streams)
	if err != nil {
		fmt.Println("metrics:", err)
		os.Exit(1)
	}
	pf := func(name string, v *float64) {
		if v == nil {
			fmt.Printf("  %-11s null\n", name)
		} else {
			fmt.Printf("  %-11s %g\n", name, *v)
		}
	}
	fmt.Printf("system_id: %s\n", m.SystemID)
	if m.AcqAt != nil {
		fmt.Printf("acq_at:     %s\n", *m.AcqAt)
	}
	pf("run_min", m.RunMin)
	pf("p_start", m.PStart)
	pf("p_max", m.PMax)
	pf("p_min", m.PMin)
	pf("p_2min", m.P2Min)
	pf("ripple", m.Ripple)
	pf("max_drop", m.MaxDrop)
	pf("stroke_amp", m.StrokeAmp)
	pf("flow_med", m.FlowMed)
	pf("flow_std", m.FlowStd)
	pf("oven_med", m.OvenMed)
	npz, _ := npzBytes(chans)
	fmt.Printf("npz bytes:  %d\n", len(npz))
}
