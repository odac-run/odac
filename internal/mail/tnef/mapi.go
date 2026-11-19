package tnef

import "encoding/binary"

// MAPI property ids read from attMsgProps and attAttachment ([MS-OXPROPS]).
const (
	prSubject                 = 0x0037
	prTransportMessageHeaders = 0x007D
	prBody                    = 0x1000
	prRTFCompressed           = 0x1009
	prBodyHTML                = 0x1013
	prDisplayName             = 0x3001
	prAttachData              = 0x3701
	prAttachFilename          = 0x3704
	prAttachMethod            = 0x3705
	prAttachLongFilename      = 0x3707
	prAttachMimeTag           = 0x370E
	prAttachContentID         = 0x3712
	prInternetCPID            = 0x3FDE
	prMessageCodepage         = 0x3FFD
	prAttachmentHidden        = 0x7FFE
)

const attachOLE = 6

// Property types ([MS-OXCDATA] 2.11.1).
const (
	ptypInteger16  = 0x0002
	ptypInteger32  = 0x0003
	ptypFloating32 = 0x0004
	ptypFloating64 = 0x0005
	ptypCurrency   = 0x0006
	ptypFloatTime  = 0x0007
	ptypErrorCode  = 0x000A
	ptypBoolean    = 0x000B
	ptypObject     = 0x000D
	ptypInteger64  = 0x0014
	ptypString8    = 0x001E
	ptypString     = 0x001F
	ptypTime       = 0x0040
	ptypGUID       = 0x0048
	ptypServerID   = 0x00FB
	ptypBinary     = 0x0102

	mvFlag = 0x1000
)

// namedBase is the first property id that is a per-message mapping to a
// named property rather than a fixed meaning.
const namedBase = 0x8000

type prop struct {
	typ  uint16
	vals [][]byte
}

// props holds a property list by id. Named properties are parsed past but not
// kept: their ids mean nothing outside the message that mapped them.
type props map[uint16]prop

// parseProps reads a MAPI property list ([MS-OXTNEF] 2.1.3.4). Each value is
// padded to four bytes and every loop iteration consumes at least four, so a
// hostile count runs out of input instead of running forever.
func parseProps(b []byte) (props, error) {
	r := reader{b: b}
	count, ok := r.u32()
	if !ok {
		return nil, malformed("property count truncated")
	}
	out := props{}
	for i := uint32(0); i < count; i++ {
		typ, ok1 := r.u16()
		id, ok2 := r.u16()
		if !ok1 || !ok2 {
			return nil, malformed("property %d tag truncated", i)
		}
		if id >= namedBase {
			if err := skipPropName(&r); err != nil {
				return nil, err
			}
		}
		vals, err := readValues(&r, typ)
		if err != nil {
			return nil, err
		}
		if id < namedBase {
			out[id] = prop{typ: typ &^ mvFlag, vals: vals}
		}
	}
	return out, nil
}

func skipPropName(r *reader) error {
	if _, ok := r.bytes(16); !ok {
		return malformed("property name guid truncated")
	}
	kind, ok := r.u32()
	if !ok {
		return malformed("property name kind truncated")
	}
	switch kind {
	case 0:
		if _, ok := r.u32(); !ok {
			return malformed("property name id truncated")
		}
	case 1:
		n, ok := r.u32()
		if !ok {
			return malformed("property name length truncated")
		}
		if _, ok := r.bytes(pad4(n)); !ok {
			return malformed("property name truncated")
		}
	default:
		return malformed("property name kind %d", kind)
	}
	return nil
}

func readValues(r *reader, typ uint16) ([][]byte, error) {
	base := typ &^ mvFlag
	switch base {
	case ptypString8, ptypString, ptypBinary, ptypObject, ptypServerID:
		n, ok := r.u32()
		if !ok {
			return nil, malformed("value count truncated")
		}
		var vals [][]byte
		for i := uint32(0); i < n; i++ {
			size, ok := r.u32()
			if !ok {
				return nil, malformed("value size truncated")
			}
			v, ok := r.bytes(uint64(size))
			if !ok {
				return nil, malformed("value truncated")
			}
			if _, ok := r.bytes(pad4(size) - uint64(size)); !ok {
				return nil, malformed("value padding truncated")
			}
			vals = append(vals, v)
		}
		return vals, nil
	}

	size := fixedSize(base)
	if size == 0 {
		return nil, malformed("property type %#04x", typ)
	}
	n := uint32(1)
	if typ&mvFlag != 0 {
		var ok bool
		if n, ok = r.u32(); !ok {
			return nil, malformed("value count truncated")
		}
	}
	var vals [][]byte
	for i := uint32(0); i < n; i++ {
		v, ok := r.bytes(pad4(size))
		if !ok {
			return nil, malformed("value truncated")
		}
		vals = append(vals, v[:size])
	}
	return vals, nil
}

func fixedSize(base uint16) uint32 {
	switch base {
	case ptypInteger16, ptypBoolean:
		return 2
	case ptypInteger32, ptypFloating32, ptypErrorCode:
		return 4
	case ptypFloating64, ptypCurrency, ptypFloatTime, ptypInteger64, ptypTime:
		return 8
	case ptypGUID:
		return 16
	}
	return 0
}

func pad4(n uint32) uint64 { return (uint64(n) + 3) &^ 3 }

func (p props) str(id uint16, cp codepage) string {
	v, ok := p[id]
	if !ok || len(v.vals) == 0 {
		return ""
	}
	switch v.typ {
	case ptypString:
		return decodeUTF16(v.vals[0])
	case ptypString8:
		return cp.decode(v.vals[0])
	}
	return ""
}

func (p props) bin(id uint16) ([]byte, bool) {
	v, ok := p[id]
	if !ok || len(v.vals) == 0 || v.typ != ptypBinary {
		return nil, false
	}
	return v.vals[0], true
}

func (p props) u32(id uint16) (uint32, bool) {
	v, ok := p[id]
	if !ok || len(v.vals) == 0 {
		return 0, false
	}
	switch v.typ {
	case ptypInteger32, ptypErrorCode:
		return binary.LittleEndian.Uint32(v.vals[0]), true
	case ptypInteger16, ptypBoolean:
		return uint32(binary.LittleEndian.Uint16(v.vals[0])), true
	}
	return 0, false
}
