package main

// Minimal numpy .npz writer: a ZIP (deflate) of .npy members, so the derived
// trace blobs are read back by numpy.load exactly like the Python parser's
// savez_compressed output. Each channel is a 1-D little-endian float64 array.

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"fmt"
)

type Channel struct {
	Name string
	Data []float64
}

func npyBytes(a []float64) []byte {
	var buf bytes.Buffer
	header := fmt.Sprintf("{'descr': '<f8', 'fortran_order': False, 'shape': (%d,), }", len(a))
	// total header must be aligned so data starts on a 64-byte boundary
	prefix := 10 // magic(6)+version(2)+headerlen(2)
	total := prefix + len(header) + 1
	pad := (64 - total%64) % 64
	header += string(bytes.Repeat([]byte{' '}, pad)) + "\n"
	buf.Write([]byte("\x93NUMPY"))
	buf.WriteByte(1)
	buf.WriteByte(0)
	hl := uint16(len(header))
	binary.Write(&buf, binary.LittleEndian, hl)
	buf.WriteString(header)
	for _, v := range a {
		binary.Write(&buf, binary.LittleEndian, v)
	}
	return buf.Bytes()
}

func npzBytes(chans []Channel) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, c := range chans {
		w, err := zw.CreateHeader(&zip.FileHeader{
			Name:   c.Name + ".npy",
			Method: zip.Deflate,
		})
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(npyBytes(c.Data)); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
