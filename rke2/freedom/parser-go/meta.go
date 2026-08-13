package main

// Metadata extraction — port of hplc-tools src/lcd_meta.py. Acquisition time
// from the File Property stream (binary FILETIME@548 or the XML SampleInfo
// variant); instrument identity from the SystemInformation stream.

import (
	"encoding/binary"
	"regexp"
	"strconv"
	"time"
	"unicode/utf16"
)

const filetimeEpochDelta = 11644473600 // seconds between 1601 and 1970

func ftToTime(low, high uint32) (time.Time, bool) {
	ft := (int64(high) << 32) | int64(low)
	secs := float64(ft)/1e7 - filetimeEpochDelta
	if secs < 0 || secs > 4102444800 { // rough 1970..2100 guard
		return time.Time{}, false
	}
	t := time.Unix(int64(secs), int64((secs-float64(int64(secs)))*1e9)).UTC()
	if t.Year() < 2000 || t.Year() > 2100 {
		return time.Time{}, false
	}
	return t, true
}

var (
	reXMLLo    = regexp.MustCompile(`<dwLowDateTime>(-?\d+)<`)
	reXMLHi    = regexp.MustCompile(`<dwHighDateTime>(-?\d+)<`)
	reXMLGenLo = regexp.MustCompile(`<dwLowGeneratedDateTime>(-?\d+)<`)
	reXMLGenHi = regexp.MustCompile(`<dwHighGeneratedDateTime>(-?\d+)<`)
	reSample   = regexp.MustCompile(`(?s)<SampleInfo>.*?</SampleInfo>`)
	reInstr    = regexp.MustCompile(`(DESKTOP-[A-Z0-9]+-Instrument\d|HPLC)`)
)

// acqTime returns acquisition time (UTC) from the File Property stream.
func acqTime(streams map[string][]byte) (time.Time, bool) {
	d, ok := streams["File Property"]
	if !ok {
		return time.Time{}, false
	}
	if len(d) >= 9 && string(d[4:9]) == "<?xml" {
		txt := string(d)
		scope := txt
		if m := reSample.FindString(txt); m != "" {
			scope = m
		}
		for _, pair := range [][2]*regexp.Regexp{{reXMLLo, reXMLHi}, {reXMLGenLo, reXMLGenHi}} {
			lo := firstSub(pair[0], scope, txt)
			hi := firstSub(pair[1], scope, txt)
			if lo != "" && hi != "" {
				loN, _ := strconv.ParseInt(lo, 10, 64)
				hiN, _ := strconv.ParseInt(hi, 10, 64)
				if t, ok := ftToTime(uint32(loN), uint32(hiN)); ok {
					return t, true
				}
			}
		}
		return time.Time{}, false
	}
	if len(d) >= 556 {
		lo := binary.LittleEndian.Uint32(d[548:552])
		hi := binary.LittleEndian.Uint32(d[552:556])
		return ftToTime(lo, hi)
	}
	return time.Time{}, false
}

func firstSub(re *regexp.Regexp, scope, full string) string {
	if m := re.FindStringSubmatch(scope); m != nil {
		return m[1]
	}
	if m := re.FindStringSubmatch(full); m != nil {
		return m[1]
	}
	return ""
}

// instrument returns the instrument/system identity string.
func instrument(streams map[string][]byte) string {
	d, ok := streams["GUMM_Information/GUMMSubStg/SystemInformation"]
	if !ok {
		return "?"
	}
	var txt string
	if len(d) >= 2 && d[1] == 0x00 {
		u16 := make([]uint16, 0, len(d)/2)
		for i := 0; i+1 < len(d); i += 2 {
			u16 = append(u16, binary.LittleEndian.Uint16(d[i:i+2]))
		}
		txt = string(utf16.Decode(u16))
	} else {
		txt = string(d)
	}
	if m := reInstr.FindString(txt); m != "" {
		return m
	}
	return "?"
}
