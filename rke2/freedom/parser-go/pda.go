package main

// PDA 3D chromatogram extraction — port of hplc-tools src/lcd_parse.py
// (read_wavelengths / read_axis / read_pda). The full time × wavelength matrix
// is stored per run so consumers (the run-detail dash, fault detectors) load
// one artifact and slice a chromatogram at any wavelength: matrix[:, iλ].
//
// Derived artifact: chromatogram/<raw key>.npz with members
//   times_min      <f8 (nrows,)
//   wavelengths_nm <f8 (nlambda,)
//   matrix         <i4 (nrows, nlambda)   row-major, detector counts
// int32 is exact: the detector ADC ceiling is 4,000,000 counts.

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"

	"github.com/minio/minio-go/v7"
)

const pdaDir = "PDA 3D Raw Data"

func readPDAWavelengths(streams map[string][]byte) ([]float64, error) {
	d, ok := streams[pdaDir+"/Wavelength Table"]
	if !ok || len(d) < 4 {
		return nil, fmt.Errorf("wavelength table absent/short")
	}
	n := int(binary.LittleEndian.Uint32(d[0:4]))
	if n <= 0 || len(d) < 4+4*n {
		return nil, fmt.Errorf("wavelength table truncated (n=%d, len=%d)", n, len(d))
	}
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		out[i] = float64(int32(binary.LittleEndian.Uint32(d[4+4*i:]))) / 100.0
	}
	return out, nil
}

// readPDAAxis returns (interval_ms, nrows) from the Max Plot stream header.
func readPDAAxis(streams map[string][]byte) (int, int, error) {
	d, ok := streams[pdaDir+"/Max Plot"]
	if !ok || len(d) < 12 {
		return 0, 0, fmt.Errorf("max plot absent/short")
	}
	interval := int(int32(binary.LittleEndian.Uint32(d[4:8])))
	nrows := int(int32(binary.LittleEndian.Uint32(d[8:12])))
	return interval, nrows, nil
}

// readPDA decodes the full 3D matrix. Mirrors lcd_parse.read_pda: one
// delta-encoded block per time point, read until the stream is exhausted
// (tolerating up to nrows+10 rows, same guard as the Python).
func readPDA(streams map[string][]byte) (times, lambdas []float64, mat []int32, nrows, nlambda int, err error) {
	lambdas, err = readPDAWavelengths(streams)
	if err != nil {
		return
	}
	interval, declared, err2 := readPDAAxis(streams)
	if err2 != nil {
		err = err2
		return
	}
	raw, ok := streams[pdaDir+"/3D Raw Data"]
	if !ok {
		err = fmt.Errorf("3d raw data absent")
		return
	}
	nlambda = len(lambdas)
	r := bytes.NewReader(raw)
	for r.Len() > 0 && nrows < declared+10 {
		row := decodeBlock(r, nlambda)
		for _, v := range row {
			mat = append(mat, int32(v))
		}
		nrows++
	}
	times = make([]float64, nrows)
	for i := range times {
		times[i] = float64(i) * float64(interval) / 60000.0
	}
	return
}

// npyI32Matrix encodes a row-major (rows, cols) int32 matrix as a .npy blob.
func npyI32Matrix(rows, cols int, data []int32) []byte {
	var buf bytes.Buffer
	header := fmt.Sprintf("{'descr': '<i4', 'fortran_order': False, 'shape': (%d, %d), }", rows, cols)
	prefix := 10 // magic(6)+version(2)+headerlen(2)
	total := prefix + len(header) + 1
	pad := (64 - total%64) % 64
	header += string(bytes.Repeat([]byte{' '}, pad)) + "\n"
	buf.Write([]byte("\x93NUMPY"))
	buf.WriteByte(1)
	buf.WriteByte(0)
	binary.Write(&buf, binary.LittleEndian, uint16(len(header)))
	buf.WriteString(header)
	binary.Write(&buf, binary.LittleEndian, data)
	return buf.Bytes()
}

func chromNpz(times, lambdas []float64, mat []int32, nrows, nlambda int) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	members := []struct {
		name string
		blob []byte
	}{
		{"times_min.npy", npyBytes(times)},
		{"wavelengths_nm.npy", npyBytes(lambdas)},
		{"matrix.npy", npyI32Matrix(nrows, nlambda, mat)},
	}
	for _, m := range members {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: m.name, Method: zip.Deflate})
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(m.blob); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// putChromatogram extracts the PDA matrix and writes it to the derived bucket.
// Returns "" (no error) when the file simply has no PDA data — chrom_key stays
// NULL and the run is still valid for pressure metrics.
func putChromatogram(ctx context.Context, cfg Config, s3 *minio.Client, key string, streams map[string][]byte) (string, error) {
	if _, ok := streams[pdaDir+"/3D Raw Data"]; !ok {
		return "", nil
	}
	times, lambdas, mat, nrows, nlambda, err := readPDA(streams)
	if err != nil {
		return "", err
	}
	if nrows == 0 || nlambda == 0 {
		return "", fmt.Errorf("empty pda matrix")
	}
	npz, err := chromNpz(times, lambdas, mat, nrows, nlambda)
	if err != nil {
		return "", err
	}
	ckey := "chromatogram/" + key + ".npz"
	if _, err := s3.PutObject(ctx, cfg.Derived, ckey, bytesReader(npz), int64(len(npz)),
		minio.PutObjectOptions{ContentType: "application/octet-stream"}); err != nil {
		return "", err
	}
	return ckey, nil
}

// pdaFile decodes a single local .lcd and prints matrix stats — the same
// numbers lcd_parse.py prints when run as a script, for cross-validation.
// With a second argument it also writes the chromatogram .npz for numeric
// comparison against the Python decoder:
//
//	go run . pda path/to/file.lcd [out.npz]
func pdaFile(path string) {
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
	times, lambdas, mat, nrows, nlambda, err := readPDA(streams)
	if err != nil {
		fmt.Println("pda:", err)
		os.Exit(1)
	}
	fmt.Printf("matrix: %d timepoints x %d wavelengths\n", nrows, nlambda)
	fmt.Printf("time axis: %.3f -> %.3f min\n", times[0], times[nrows-1])
	fmt.Printf("wavelengths: %.1f -> %.1f nm\n", lambdas[0], lambdas[nlambda-1])
	// nearest channel to 214 nm — the lab's detection wavelength
	idx := 0
	for i, l := range lambdas {
		if abs(l-214) < abs(lambdas[idx]-214) {
			idx = i
		}
	}
	lo, hi, arg := mat[idx], mat[idx], 0
	for i := 0; i < nrows; i++ {
		v := mat[i*nlambda+idx]
		if v < lo {
			lo = v
		}
		if v > hi {
			hi, arg = v, i
		}
	}
	fmt.Printf("214nm channel: min=%d max=%d argmax at %.2f min\n", lo, hi, times[arg])
	npz, _ := chromNpz(times, lambdas, mat, nrows, nlambda)
	fmt.Printf("npz bytes: %d\n", len(npz))
	if len(os.Args) >= 4 {
		if err := os.WriteFile(os.Args[3], npz, 0o644); err != nil {
			fmt.Println("write npz:", err)
			os.Exit(1)
		}
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
