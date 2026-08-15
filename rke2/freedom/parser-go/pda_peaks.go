package main

// LabSolutions peak-table extraction. Every processed .lcd carries the
// instrument's OWN integration in the "LSS Data Processing" storage — the same
// numbers that print on the CoA. We read them directly rather than re-integrate
// the raw signal (which never reproduces the analyst's manual peak selection):
//
//   CR-PDA…  Compound Results, XML. Per-peak retention time + area + height.
//            Doubles are '@DtoX@<hex>' (big-endian IEEE-754); rt is int, 1e-5 min.
//   PT-PDA…  Peak Table, binary. Header "VER1" + int32 peak count; fixed-stride
//            records with area at +28 and reported area-% at +212. The main
//            peak's area-% IS the reported purity (verified exact vs CoA, and
//            the per-peak area-% self-check to 100.0).
//
// rawHash fingerprints only the raw ACQUISITION streams, never the processing
// storage — so a re-integration (which rewrites only the peak table) leaves the
// hash unchanged, while a different run landing on the same key changes it.

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
)

// peaksFile decodes a local .lcd and prints the extracted peak table — a local
// harness to confirm the Go extractor matches the Python reference:
//
//	go run . peaks path/to/file.lcd
func peaksFile(path string) {
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
	peaks, purity, mainRT, mainArea, n, nIdent, integ := extractPeakTable(streams)
	fmt.Printf("integrated=%v purity=%.3f n_peaks=%d n_identified=%d main_rt=%.3f main_area=%.1f\n",
		integ, purity, n, nIdent, mainRT, mainArea)
	for _, p := range peaks {
		fmt.Printf("  idx=%d rt=%.3f area=%.1f height=%.1f area%%=%.3f\n",
			p.Idx, p.RT, p.Area, p.Height, p.AreaPct)
	}
}

// Peak is one integrated peak from the instrument's stored peak table.
type Peak struct {
	Idx     int
	RT      float64 // minutes
	Area    float64
	Height  float64
	AreaPct float64 // reported area-% (from PT); the main peak's = the CoA purity
}

// base returns the stream name after the last path separator.
func base(k string) string {
	if i := strings.LastIndexByte(k, '/'); i >= 0 {
		return k[i+1:]
	}
	return k
}

// dtox decodes a LabSolutions '@DtoX@<hex>' big-endian IEEE-754 double.
func dtox(s string) float64 {
	s = strings.TrimPrefix(strings.TrimSpace(s), "@DtoX@")
	if s == "" || s == "0" {
		return 0
	}
	for len(s) < 16 {
		s = "0" + s
	}
	b, err := hex.DecodeString(s)
	if err != nil || len(b) < 8 {
		return 0
	}
	return math.Float64frombits(binary.BigEndian.Uint64(b[:8]))
}

type crComp struct {
	RT int64  `xml:"rt"`
	A  string `xml:"a"`
	HT string `xml:"ht"`
}
type crRoot struct {
	Comps []crComp `xml:"COMPLIST"`
}

// crPeaks parses the Compound Results XML into the detected peak table.
func crPeaks(streams map[string][]byte) []Peak {
	var raw []byte
	// Match the stream basename prefix so "CR-PDA…" is not confused with the
	// empty "GPCR-PDA…" group-results stream (which also contains "CR-PDA").
	for k, v := range streams { // prefer "Original" — the full detected set
		if strings.HasPrefix(base(k), "CR-PDA") && strings.Contains(k, "Original") {
			raw = v
			break
		}
	}
	if raw == nil {
		for k, v := range streams {
			if strings.HasPrefix(base(k), "CR-PDA") {
				raw = v
				break
			}
		}
	}
	if raw == nil {
		return nil
	}
	var root crRoot
	if err := xml.Unmarshal(raw, &root); err != nil {
		return nil
	}
	var pk []Peak
	for _, c := range root.Comps {
		area := dtox(c.A)
		if area <= 0 {
			continue
		}
		pk = append(pk, Peak{RT: float64(c.RT) / 100000.0, Area: area, Height: dtox(c.HT)})
	}
	return pk
}

// ptPurity reads the binary Peak Table: reported purity (max area-%), peak
// count, and an area→area-% map to tag the CR peaks. ok is false unless the
// per-peak area-% sum to ~100 (the format self-check).
func ptPurity(streams map[string][]byte) (purity float64, nPeaks int, pctByArea map[float64]float64, ok bool) {
	var d []byte
	var bestKey string
	for k, v := range streams {
		if strings.HasPrefix(base(k), "PT-PDA") && !strings.Contains(k, "Original") {
			if k > bestKey { // highest suffix = the final result table
				bestKey, d = k, v
			}
		}
	}
	if len(d) < 232 || string(d[:4]) != "VER1" {
		return 0, 0, nil, false
	}
	n := int(int32(binary.LittleEndian.Uint32(d[4:8])))
	if n < 1 || n > 500 {
		return 0, 0, nil, false
	}
	stride := (len(d) - 20) / n
	if stride < 232 {
		stride = 1056
	}
	pct := make(map[float64]float64, n)
	var sum, maxPct float64
	got := 0
	for i := 0; i < n; i++ {
		b := i * stride
		if b+220 > len(d) {
			break
		}
		area := math.Float64frombits(binary.LittleEndian.Uint64(d[b+28 : b+36]))
		ap := math.Float64frombits(binary.LittleEndian.Uint64(d[b+212 : b+220]))
		if ap < 0 || ap > 100.001 || area <= 0 {
			continue
		}
		pct[area] = ap
		sum += ap
		if ap > maxPct {
			maxPct = ap
		}
		got++
	}
	if got == 0 || sum < 95 || sum > 105 {
		return 0, got, nil, false
	}
	return maxPct, n, pct, true
}

func nearestPct(pct map[float64]float64, area float64) float64 {
	best, bestd := 0.0, math.Inf(1)
	for a, p := range pct {
		if d := math.Abs(a - area); d < bestd {
			bestd, best = d, p
		}
	}
	if bestd <= area*0.01+1 { // only if the areas actually match
		return best
	}
	return 0
}

// crIdentified returns the analyst-designated target compounds (rt + area>0)
// from the non-Original CR-PDA stream. A targeted purity assay narrows this to
// one compound (the analyte); a blend/screen leaves many.
func crIdentified(streams map[string][]byte) []Peak {
	var raw []byte
	for k, v := range streams {
		if strings.HasPrefix(base(k), "CR-PDA") && !strings.Contains(k, "Original") {
			raw = v
			break
		}
	}
	if raw == nil {
		return nil
	}
	var root crRoot
	if err := xml.Unmarshal(raw, &root); err != nil {
		return nil
	}
	var out []Peak
	for _, c := range root.Comps {
		if a := dtox(c.A); a > 0 {
			out = append(out, Peak{RT: float64(c.RT) / 100000.0, Area: a})
		}
	}
	return out
}

func nearestRTIdx(peaks []Peak, rt float64) int {
	idx, best := -1, math.Inf(1)
	for i, p := range peaks {
		if d := math.Abs(p.RT - rt); d < best {
			best, idx = d, i
		}
	}
	return idx
}

// extractPeakTable merges CR (rt/area/height) with PT (reported area-%), sorted
// largest-area first. purity is the area-% of the peak that carries the reported
// value: the single designated target when there is exactly one (the CoA purity),
// otherwise the largest peak past the ~void cutoff (so the solvent front is never
// mistaken for the analyte). nIdent is the designated-target count — >1 marks a
// blend, which has no single purity. integrated is true when a PT table exists.
func extractPeakTable(streams map[string][]byte) (peaks []Peak, purity, mainRT, mainArea float64, nPeaks, nIdent int, integrated bool) {
	cr := crPeaks(streams)
	_, ptN, ptPct, ptOK := ptPurity(streams)
	if ptOK {
		for i := range cr {
			cr[i].AreaPct = nearestPct(ptPct, cr[i].Area)
		}
	}
	sort.Slice(cr, func(i, j int) bool { return cr[i].Area > cr[j].Area })
	for i := range cr {
		cr[i].Idx = i
	}
	peaks = cr

	ident := crIdentified(streams)
	nIdent = len(ident)

	// Pick the peak whose area-% is the reported purity.
	const voidCut = 1.0 // min; peaks before this are column void / solvent front
	pick := -1
	if nIdent == 1 {
		pick = nearestRTIdx(cr, ident[0].RT) // the one designated target
	} else {
		for i, p := range cr { // cr is area-desc; first past the void
			if p.RT >= voidCut {
				pick = i
				break
			}
		}
	}
	if pick < 0 && len(cr) > 0 {
		pick = 0 // fallback: largest peak
	}

	if pick >= 0 && pick < len(cr) {
		mainRT, mainArea = cr[pick].RT, cr[pick].Area
	}
	if ptOK {
		integrated, nPeaks = true, ptN
		if pick >= 0 && pick < len(cr) {
			purity = cr[pick].AreaPct
		}
	} else {
		nPeaks = len(cr)
	}
	return
}

// rawHash fingerprints the raw acquisition streams only. "Data Processing"
// storage (the re-integration output) is excluded, so a re-integration does not
// change the hash — but any change to the acquired data does.
func rawHash(streams map[string][]byte) string {
	keys := make([]string, 0, len(streams))
	for k := range streams {
		if strings.Contains(k, "Data Processing") {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	var lenbuf [8]byte
	for _, k := range keys {
		h.Write([]byte(k))
		binary.LittleEndian.PutUint64(lenbuf[:], uint64(len(streams[k])))
		h.Write(lenbuf[:])
		h.Write(streams[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}
