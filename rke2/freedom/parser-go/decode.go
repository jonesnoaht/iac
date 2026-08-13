package main

// Shimadzu .lcd (OLE compound file) decoding — a faithful port of hplc-tools
// src/lcd_parse.py (decode_block) and the StatusLog channel reader. The
// chunk-aware decode_block is load-bearing: the deprecated flat delta decoder
// silently corrupts exactly the volatile pump-fault traces we care about.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/richardlehane/mscfb"
)

// oleStreams reads an OLE2/CFB file into a map keyed by full stream path
// ("Parent/Child"), so callers can pull named streams without re-walking.
func oleStreams(data []byte) (map[string][]byte, error) {
	doc, err := mscfb.New(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte)
	for entry, err := doc.Next(); err == nil; entry, err = doc.Next() {
		if entry.Size == 0 {
			continue
		}
		key := entry.Name
		if len(entry.Path) > 0 {
			key = joinPath(entry.Path) + "/" + entry.Name
		}
		buf := make([]byte, entry.Size)
		n, _ := io.ReadFull(entry, buf)
		out[key] = buf[:n]
	}
	return out, nil
}

func joinPath(p []string) string {
	s := ""
	for i, seg := range p {
		if i > 0 {
			s += "/"
		}
		s += seg
	}
	return s
}

// decodeVal decodes one delta from a big-endian byte group where the high
// nibble is the sign/width digit. Port of _decode_val.
func decodeVal(buf []byte) int64 {
	var x int64
	for _, b := range buf {
		x = (x << 8) | int64(b)
	}
	valueBits := 8*len(buf) - 4
	sign := (x >> valueBits) & 0xF
	value := x & ((int64(1) << valueBits) - 1)
	if sign%2 == 1 {
		return value - (int64(1) << valueBits)
	}
	return value
}

// decodeBlock decodes one delta-encoded block of n values. Exact port of
// lcd_parse.decode_block: 24-byte segment header, then sub-blocks each prefixed
// by a 2-byte little-endian length and followed by a 2-byte end marker; the
// accumulator resets between sub-blocks.
func decodeBlock(r *bytes.Reader, n int) []float64 {
	skip := make([]byte, 24)
	io.ReadFull(r, skip)
	signal := make([]float64, n)
	count := 0
	var acc int64
	for count < n {
		hdr := make([]byte, 2)
		if _, err := io.ReadFull(r, hdr); err != nil {
			break
		}
		nBytes := int(binary.LittleEndian.Uint16(hdr))
		raw := make([]byte, nBytes)
		if _, err := io.ReadFull(r, raw); err != nil {
			break
		}
		pos := 0
		for pos < len(raw) {
			b := raw[pos]
			var delta int64
			switch {
			case b == 0x82:
				pos++
				continue
			case b == 0x00:
				delta = 0
				pos++
			default:
				hi := b >> 4
				switch {
				case hi == 0:
					delta = int64(b)
					pos++
				case hi == 1:
					delta = decodeVal(raw[pos : pos+1])
					pos++
				default:
					extra := int(hi) / 2
					end := pos + 1 + extra
					if end > len(raw) {
						end = len(raw)
					}
					delta = decodeVal(raw[pos:end])
					pos = end
				}
			}
			acc += delta
			if count < n {
				signal[count] = float64(acc)
			}
			count++
		}
		end := make([]byte, 2) // end marker (== nBytes)
		io.ReadFull(r, end)
		acc = 0
	}
	return signal
}

// readStatusChannel returns (times_min, values) for StatusLog Ch<ch>, decoded
// with the chunk-aware decoder. Port of statuslog.read_statuslog / read_ch.
func readStatusChannel(streams map[string][]byte, ch int) ([]float64, []float64, error) {
	key := fmt.Sprintf("LSS Raw Data/StatusLog Ch%d", ch)
	d, ok := streams[key]
	if !ok || len(d) < 25 {
		return nil, nil, fmt.Errorf("channel %d absent/short", ch)
	}
	interval := int32(binary.LittleEndian.Uint32(d[4:8]))
	nvals := int32(binary.LittleEndian.Uint32(d[8:12]))
	vals := decodeBlock(bytes.NewReader(d), int(nvals))
	times := make([]float64, len(vals))
	for i := range vals {
		times[i] = float64(i) * float64(interval) / 60000.0
	}
	return times, vals, nil
}
