// Package tneftest builds TNEF streams for tests, laid out the way Outlook
// writes them. It encodes [MS-OXTNEF] on its own rather than borrowing the
// decoder's constants, so a test built with it checks the decoder against the
// specification and not against itself.
package tneftest

import (
	"bytes"
	"encoding/binary"
	"unicode/utf16"
)

// Attribute levels.
const (
	LevelMessage    byte = 0x01
	LevelAttachment byte = 0x02
)

// Attribute ids, the low word of the attribute tag.
const (
	AttSubject        uint16 = 0x8004
	AttMessageClass   uint16 = 0x8008
	AttBody           uint16 = 0x800C
	AttAttachData     uint16 = 0x800F
	AttAttachTitle    uint16 = 0x8010
	AttAttachRenddata uint16 = 0x9002
	AttMsgProps       uint16 = 0x9003
	AttAttachment     uint16 = 0x9005
	AttOemCodepage    uint16 = 0x9007
)

// MAPI property ids.
const (
	PrSubject                 uint16 = 0x0037
	PrTransportMessageHeaders uint16 = 0x007D
	PrBody                    uint16 = 0x1000
	PrRTFCompressed           uint16 = 0x1009
	PrBodyHTML                uint16 = 0x1013
	PrDisplayName             uint16 = 0x3001
	PrAttachData              uint16 = 0x3701
	PrAttachFilename          uint16 = 0x3704
	PrAttachMethod            uint16 = 0x3705
	PrAttachLongFilename      uint16 = 0x3707
	PrAttachMimeTag           uint16 = 0x370E
	PrAttachContentID         uint16 = 0x3712
	PrInternetCPID            uint16 = 0x3FDE
	PrMessageCodepage         uint16 = 0x3FFD
	PrAttachmentHidden        uint16 = 0x7FFE
)

// MAPI property types.
const (
	TypeInteger16 uint16 = 0x0002
	TypeInteger32 uint16 = 0x0003
	TypeBoolean   uint16 = 0x000B
	TypeObject    uint16 = 0x000D
	TypeString8   uint16 = 0x001E
	TypeString    uint16 = 0x001F
	TypeBinary    uint16 = 0x0102
	MultiValued   uint16 = 0x1000
)

// Attach methods.
const (
	AttachByValue  uint32 = 1
	AttachEmbedded uint32 = 5
	AttachOLE      uint32 = 6
)

// IIDMessage prefixes an attachment object holding an Outlook message.
var IIDMessage = []byte{0x07, 0x03, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}

// RTFExample1 is example 1 of [MS-OXRTFCP] 3.1.1, which decompresses to
// RTFExample1Text.
var RTFExample1 = []byte{
	0x2d, 0x00, 0x00, 0x00, 0x2b, 0x00, 0x00, 0x00, 0x4c, 0x5a, 0x46, 0x75, 0xf1, 0xc5, 0xc7, 0xa7,
	0x03, 0x00, 0x0a, 0x00, 0x72, 0x63, 0x70, 0x67, 0x31, 0x32, 0x35, 0x42, 0x32, 0x0a, 0xf3, 0x20,
	0x68, 0x65, 0x6c, 0x09, 0x00, 0x20, 0x62, 0x77, 0x05, 0xb0, 0x6c, 0x64, 0x7d, 0x0a, 0x80, 0x0f,
	0xa0,
}

// RTFExample1Text is what RTFExample1 decompresses to.
const RTFExample1Text = "{\\rtf1\\ansi\\ansicpg1252\\pard hello world}\r\n"

// RTFExample2 is example 2 of [MS-OXRTFCP] 3.1.2: a reference that overlaps
// its own output.
var RTFExample2 = []byte{
	0x1a, 0x00, 0x00, 0x00, 0x1c, 0x00, 0x00, 0x00, 0x4c, 0x5a, 0x46, 0x75, 0xe2, 0xd4, 0x4b, 0x51,
	0x41, 0x00, 0x04, 0x20, 0x57, 0x58, 0x59, 0x5a, 0x0d, 0x6e, 0x7d, 0x01, 0x0e, 0xb0,
}

// RTFExample2Text is what RTFExample2 decompresses to.
const RTFExample2Text = "{\\rtf1 WXYZWXYZWXYZWXYZWXYZ}"

// Stream is a TNEF stream under construction.
type Stream struct{ buf bytes.Buffer }

// New starts a stream with its signature and legacy key.
func New() *Stream {
	s := &Stream{}
	s.buf.Write(binary.LittleEndian.AppendUint32(nil, 0x223E9F78))
	s.buf.Write(binary.LittleEndian.AppendUint16(nil, 0x0001))
	return s
}

// Attr appends one attribute with its checksum.
func (s *Stream) Attr(level byte, id uint16, data []byte) *Stream {
	s.buf.WriteByte(level)
	s.buf.Write(binary.LittleEndian.AppendUint32(nil, uint32(0x0006)<<16|uint32(id)))
	s.buf.Write(binary.LittleEndian.AppendUint32(nil, uint32(len(data))))
	s.buf.Write(data)
	var sum uint16
	for _, c := range data {
		sum += uint16(c)
	}
	s.buf.Write(binary.LittleEndian.AppendUint16(nil, sum))
	return s
}

// Codepage appends attOemCodepage.
func (s *Stream) Codepage(cp uint32) *Stream {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b, cp)
	return s.Attr(LevelMessage, AttOemCodepage, b)
}

// Class appends attMessageClass.
func (s *Stream) Class(class string) *Stream {
	return s.Attr(LevelMessage, AttMessageClass, append([]byte(class), 0))
}

// File appends a by-value attachment.
func (s *Stream) File(name, mimeType string, data []byte) *Stream {
	p := (&Props{}).Unicode(PrAttachLongFilename, name).Int32(PrAttachMethod, AttachByValue)
	if mimeType != "" {
		p.String8(PrAttachMimeTag, []byte(mimeType))
	}
	return s.Attr(LevelAttachment, AttAttachRenddata, Renddata()).
		Attr(LevelAttachment, AttAttachData, data).
		Attr(LevelAttachment, AttAttachment, p.Bytes())
}

// Embed appends an attached Outlook message whose stream is inner.
func (s *Stream) Embed(inner []byte, name string) *Stream {
	p := (&Props{}).Int32(PrAttachMethod, AttachEmbedded)
	if name != "" {
		p.Unicode(PrDisplayName, name)
	}
	p.Binary(PrAttachData, TypeObject, append(bytes.Clone(IIDMessage), inner...))
	return s.Attr(LevelAttachment, AttAttachRenddata, Renddata()).
		Attr(LevelAttachment, AttAttachment, p.Bytes())
}

// Bytes returns the stream so far.
func (s *Stream) Bytes() []byte { return s.buf.Bytes() }

// Renddata is an attAttachRenddata value for an ordinary file.
func Renddata() []byte { return []byte{0x01, 0x00, 0xFF, 0xFF, 0xFF, 0xFF, 0, 0, 0, 0, 0, 0, 0, 0} }

// Props is a MAPI property list under construction.
type Props struct {
	n    uint32
	body bytes.Buffer
}

// Tag starts a property. Its value is written with Value or Raw.
func (p *Props) Tag(typ, id uint16) *Props {
	p.n++
	p.body.Write(binary.LittleEndian.AppendUint16(nil, typ))
	p.body.Write(binary.LittleEndian.AppendUint16(nil, id))
	return p
}

// Value writes a single variable-length value with its count, size and
// padding.
func (p *Props) Value(v []byte) *Props {
	p.body.Write(binary.LittleEndian.AppendUint32(nil, 1))
	p.body.Write(binary.LittleEndian.AppendUint32(nil, uint32(len(v))))
	p.body.Write(v)
	p.body.Write(make([]byte, (4-len(v)%4)%4))
	return p
}

// Raw writes bytes as they are, for layouts the other methods do not cover.
func (p *Props) Raw(b []byte) *Props {
	p.body.Write(b)
	return p
}

// Unicode adds a PtypString property.
func (p *Props) Unicode(id uint16, s string) *Props {
	var b []byte
	for _, c := range utf16.Encode([]rune(s)) {
		b = binary.LittleEndian.AppendUint16(b, c)
	}
	return p.Tag(TypeString, id).Value(append(b, 0, 0))
}

// String8 adds a PtypString8 property.
func (p *Props) String8(id uint16, b []byte) *Props {
	return p.Tag(TypeString8, id).Value(append(bytes.Clone(b), 0))
}

// Binary adds a variable-length property of type typ.
func (p *Props) Binary(id, typ uint16, b []byte) *Props {
	return p.Tag(typ, id).Value(b)
}

// Int32 adds a PtypInteger32 property.
func (p *Props) Int32(id uint16, v uint32) *Props {
	return p.Tag(TypeInteger32, id).Raw(binary.LittleEndian.AppendUint32(nil, v))
}

// Boolean adds a PtypBoolean property.
func (p *Props) Boolean(id uint16, v bool) *Props {
	var b uint32
	if v {
		b = 1
	}
	return p.Tag(TypeBoolean, id).Raw(binary.LittleEndian.AppendUint32(nil, b))
}

// Bytes returns the encoded list.
func (p *Props) Bytes() []byte {
	return append(binary.LittleEndian.AppendUint32(nil, p.n), p.body.Bytes()...)
}
