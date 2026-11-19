package tnef

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

// rtfPrebuf seeds the LZFu dictionary ([MS-OXRTFCP] 2.1.2.1): compressed
// streams reference it from their first byte on.
const rtfPrebuf = "{\\rtf1\\ansi\\mac\\deff0\\deftab720{\\fonttbl;}{\\f0\\fnil \\froman " +
	"\\fswiss \\fmodern \\fscript \\fdecor MS Sans SerifSymbolArialTimes New Roman" +
	"Courier{\\colortbl\\red0\\green0\\blue0\r\n\\par \\pard\\plain\\f0\\fs20\\b\\i\\u\\tab\\tx"

const (
	rtfHeaderLen  = 16
	rtfDictSize   = 4096
	rtfCompressed = 0x75465A4C // "LZFu"
	rtfRaw        = 0x414C454D // "MELA"
	// rtfMaxRatio bounds LZFu expansion: one control byte and eight
	// two-byte references, 17 bytes, produce at most 8*17 bytes.
	rtfMaxRatio = 8
)

// decompressRTF decodes PR_RTF_COMPRESSED and charges the output against
// budget, which is shared by every stream of one Decode.
func decompressRTF(b []byte, budget *int) ([]byte, error) {
	if len(b) < rtfHeaderLen {
		return nil, malformed("rtf header truncated")
	}
	compSize := binary.LittleEndian.Uint32(b[0:])
	rawSize := binary.LittleEndian.Uint32(b[4:])
	kind := binary.LittleEndian.Uint32(b[8:])
	crc := binary.LittleEndian.Uint32(b[12:])

	// compSize counts everything after its own field.
	if uint64(compSize) < rtfHeaderLen-4 || uint64(compSize)+4 > uint64(len(b)) {
		return nil, malformed("rtf size %d does not fit %d bytes", compSize, len(b))
	}
	if uint64(rawSize) > uint64(*budget) {
		return nil, fmt.Errorf("%w: rtf of %d bytes exceeds the remaining %d", ErrLimit, rawSize, *budget)
	}
	in := b[rtfHeaderLen : compSize+4]

	switch kind {
	case rtfRaw:
		if uint64(rawSize) > uint64(len(in)) {
			return nil, malformed("uncompressed rtf truncated")
		}
		*budget -= int(rawSize)
		return in[:rawSize:rawSize], nil
	case rtfCompressed:
	default:
		return nil, malformed("rtf compression %#08x", kind)
	}

	if rtfCRC(in) != crc {
		return nil, malformed("rtf crc mismatch")
	}
	out, err := lzfu(in, int(rawSize))
	if err != nil {
		return nil, err
	}
	*budget -= len(out)
	return out, nil
}

func lzfu(in []byte, rawSize int) ([]byte, error) {
	var dict [rtfDictSize]byte
	w := copy(dict[:], rtfPrebuf)
	out := make([]byte, 0, min(rawSize, len(in)*rtfMaxRatio))

	put := func(c byte) bool {
		if len(out) == rawSize {
			return false
		}
		out = append(out, c)
		dict[w] = c
		w = (w + 1) % rtfDictSize
		return true
	}

	pos := 0
	for pos < len(in) {
		control := in[pos]
		pos++
		for bit := 0; bit < 8; bit++ {
			if control&(1<<bit) == 0 {
				if pos >= len(in) {
					break
				}
				if !put(in[pos]) {
					return nil, malformed("rtf longer than its declared %d bytes", rawSize)
				}
				pos++
				continue
			}
			if pos+2 > len(in) {
				return nil, malformed("rtf reference truncated")
			}
			ref := int(in[pos])<<8 | int(in[pos+1])
			pos += 2
			off, n := ref>>4, ref&0xF+2
			if off == w {
				if len(out) != rawSize {
					return nil, malformed("rtf ended at %d of %d bytes", len(out), rawSize)
				}
				return out, nil
			}
			for i := 0; i < n; i++ {
				if !put(dict[(off+i)%rtfDictSize]) {
					return nil, malformed("rtf longer than its declared %d bytes", rawSize)
				}
			}
		}
	}
	return nil, malformed("rtf end marker missing")
}

// rtfCRC is CRC-32 as [MS-OXRTFCP] 3.1.3.2 defines it: the IEEE table with
// neither the initial nor the final inversion Go's crc32 applies.
func rtfCRC(b []byte) uint32 {
	return ^crc32.Update(^uint32(0), crc32.IEEETable, b)
}
