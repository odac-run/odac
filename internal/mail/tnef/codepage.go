package tnef

import (
	"bytes"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
)

// codepage pairs a Windows code page with its MIME charset name. A nil enc
// means the bytes are already UTF-8.
type codepage struct {
	name string
	enc  encoding.Encoding
}

// defaultCodepage is what Outlook writes when a stream names none.
var defaultCodepage = codepage{"windows-1252", charmap.Windows1252}

var codepages = map[uint32]codepage{
	437:   {"IBM437", charmap.CodePage437},
	850:   {"IBM850", charmap.CodePage850},
	852:   {"IBM852", charmap.CodePage852},
	855:   {"IBM855", charmap.CodePage855},
	858:   {"IBM00858", charmap.CodePage858},
	860:   {"IBM860", charmap.CodePage860},
	862:   {"IBM862", charmap.CodePage862},
	863:   {"IBM863", charmap.CodePage863},
	865:   {"IBM865", charmap.CodePage865},
	866:   {"IBM866", charmap.CodePage866},
	874:   {"windows-874", charmap.Windows874},
	932:   {"Shift_JIS", japanese.ShiftJIS},
	936:   {"GBK", simplifiedchinese.GBK},
	949:   {"EUC-KR", korean.EUCKR},
	950:   {"Big5", traditionalchinese.Big5},
	1250:  {"windows-1250", charmap.Windows1250},
	1251:  {"windows-1251", charmap.Windows1251},
	1252:  defaultCodepage,
	1253:  {"windows-1253", charmap.Windows1253},
	1254:  {"windows-1254", charmap.Windows1254},
	1255:  {"windows-1255", charmap.Windows1255},
	1256:  {"windows-1256", charmap.Windows1256},
	1257:  {"windows-1257", charmap.Windows1257},
	1258:  {"windows-1258", charmap.Windows1258},
	20127: {"us-ascii", charmap.Windows1252},
	20866: {"KOI8-R", charmap.KOI8R},
	21866: {"KOI8-U", charmap.KOI8U},
	28591: {"ISO-8859-1", charmap.ISO8859_1},
	28592: {"ISO-8859-2", charmap.ISO8859_2},
	28593: {"ISO-8859-3", charmap.ISO8859_3},
	28594: {"ISO-8859-4", charmap.ISO8859_4},
	28595: {"ISO-8859-5", charmap.ISO8859_5},
	28596: {"ISO-8859-6", charmap.ISO8859_6},
	28597: {"ISO-8859-7", charmap.ISO8859_7},
	28598: {"ISO-8859-8", charmap.ISO8859_8},
	28599: {"ISO-8859-9", charmap.ISO8859_9},
	28603: {"ISO-8859-13", charmap.ISO8859_13},
	28605: {"ISO-8859-15", charmap.ISO8859_15},
	50220: {"ISO-2022-JP", japanese.ISO2022JP},
	51932: {"EUC-JP", japanese.EUCJP},
	54936: {"GB18030", simplifiedchinese.GB18030},
	65001: {"utf-8", nil},
}

func lookupCodepage(cp uint32) codepage {
	if c, ok := codepages[cp]; ok {
		return c
	}
	return defaultCodepage
}

// decode converts an 8-bit string to UTF-8. The stream's strings are
// NUL-terminated inside their declared length.
func (c codepage) decode(b []byte) string {
	b = trimNUL(b)
	if c.enc != nil {
		if out, err := c.enc.NewDecoder().Bytes(b); err == nil {
			b = out
		}
	}
	if !utf8.Valid(b) {
		return strings.ToValidUTF8(string(b), "�")
	}
	return string(b)
}

func trimNUL(b []byte) []byte {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return b[:i]
	}
	return b
}

// decodeUTF16 converts a PtypString value, UTF-16LE up to its terminator.
func decodeUTF16(b []byte) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		c := uint16(b[i]) | uint16(b[i+1])<<8
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}
