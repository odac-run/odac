// Package tnef decodes Microsoft's Transport Neutral Encapsulation Format
// ([MS-OXTNEF]), the application/ms-tnef part Outlook and Exchange send as
// winmail.dat. Every client outside Outlook shows that part as one opaque
// file, so the attachments and the body inside it never reach the recipient.
//
// Decode runs on untrusted input inside odac-mail, where a panic becomes a
// respawn loop: every length is checked against the bytes left before it is
// used, no count read from the stream sizes an allocation, the only output
// that can outgrow its input (compressed RTF) has a budget, and a panic is
// recovered into an error anyway.
package tnef

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"mime"
	"strings"
	"unicode"
)

var (
	// ErrNotTNEF reports that the data does not start with the TNEF
	// signature, so a caller can tell a mislabeled part from a damaged one.
	ErrNotTNEF = errors.New("tnef: missing signature")

	// ErrMalformed reports a stream that cannot be decoded completely. The
	// decoder never returns a partial result, because a caller replacing
	// winmail.dat with its contents would silently drop what it missed.
	ErrMalformed = errors.New("tnef: malformed stream")

	// ErrLimit reports a stream that exceeds the Limits it was decoded with.
	ErrLimit = errors.New("tnef: limit exceeded")
)

const signature = 0x223E9F78

const (
	levelMessage    = 0x01
	levelAttachment = 0x02
)

// Attribute ids are the low word of the attribute tag; the high word only
// repeats the value type.
const (
	attSubject        = 0x8004
	attMessageClass   = 0x8008
	attBody           = 0x800C
	attAttachData     = 0x800F
	attAttachTitle    = 0x8010
	attAttachRenddata = 0x9002
	attMsgProps       = 0x9003
	attAttachment     = 0x9005
	attOemCodepage    = 0x9007
)

// attrHeaderLen is level, tag and length; a checksum follows the data.
const attrHeaderLen = 1 + 4 + 4

const renderOLE = 0x0002

// iidMessage is IID_IMessage as stored in the stream: an attachment object
// carrying it holds a whole Outlook message, itself a TNEF stream.
var iidMessage = []byte{0x07, 0x03, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}

// Limits bounds the work one Decode may do. The stream is already bounded by
// the SMTP size ceiling; these cap only what decoding can add on top of it.
// A zero field takes its default.
type Limits struct {
	// MaxDepth caps the nesting of attached Outlook messages, each of which
	// is decoded as a TNEF stream of its own.
	MaxDepth int
	// MaxRTFBytes caps decompressed RTF across the whole stream, nesting
	// included. LZFu expands up to eight times, so this is the one output
	// that can outgrow its input.
	MaxRTFBytes int
}

const (
	defaultMaxDepth    = 8
	defaultMaxRTFBytes = 32 << 20
)

func (l Limits) withDefaults() Limits {
	if l.MaxDepth <= 0 {
		l.MaxDepth = defaultMaxDepth
	}
	if l.MaxRTFBytes <= 0 {
		l.MaxRTFBytes = defaultMaxRTFBytes
	}
	return l
}

// Message is the decoded content of one TNEF stream. Byte slices alias the
// input of Decode, which is why decoding a large winmail.dat copies none of
// its attachments.
type Message struct {
	// Class is the MAPI message class (IPM.Note, IPM.Schedule.Meeting.Request,
	// ...). A caller decides from it whether a stream is plain mail.
	Class   string
	Subject string
	// Body is the plain text body, converted to UTF-8.
	Body string
	// HTML is the HTML body in its original encoding, named by HTMLCharset.
	// It is not transcoded because the document may declare its charset in
	// a meta tag, which a converted copy would contradict.
	HTML        []byte
	HTMLCharset string
	// RTF is the decompressed RTF body. Outlook often sends only this, with
	// any HTML encapsulated inside the RTF rather than in HTML.
	RTF []byte
	// TransportHeaders are the RFC 5322 headers of an attached message as it
	// was received, when Outlook kept them.
	TransportHeaders string
	Attachments      []Attachment
}

// AttachMethod says what an attachment's content is, since only some kinds
// are files a mail client can offer to save.
type AttachMethod uint8

const (
	// ByValue is an ordinary file in Data.
	ByValue AttachMethod = iota
	// Embedded is an attached Outlook message, decoded into Message.
	Embedded
	// OLE is an OLE object, whose Data is a compound file only Windows
	// applications can open.
	OLE
)

// Attachment is one attachment of a Message.
type Attachment struct {
	// Name is a bare file name: any directory part a sender included is
	// removed, along with control characters.
	Name string
	// ContentType is the MIME type Outlook recorded, empty when it recorded
	// none or one that does not parse.
	ContentType string
	ContentID   string
	// Hidden marks an attachment Outlook does not list, usually an image
	// the HTML body references by ContentID.
	Hidden  bool
	Method  AttachMethod
	Data    []byte
	Message *Message
}

// Decode decodes a TNEF stream. It returns either the complete content or an
// error, never part of it.
func Decode(data []byte, lim Limits) (msg *Message, err error) {
	defer func() {
		if r := recover(); r != nil {
			msg, err = nil, fmt.Errorf("%w: %v", ErrMalformed, r)
		}
	}()
	return decode(data, lim)
}

// decode is Decode without the recover, so the fuzz target sees a panic.
func decode(data []byte, lim Limits) (*Message, error) {
	lim = lim.withDefaults()
	d := &decoder{maxDepth: lim.MaxDepth, rtfBudget: lim.MaxRTFBytes}
	return d.message(data, 0)
}

type decoder struct {
	maxDepth  int
	rtfBudget int
}

// rawAttachment collects one attachment's attributes until the stream ends,
// because the code page they are written in may come later.
type rawAttachment struct {
	title      []byte
	data       []byte
	renderType uint16
	props      props
}

func (d *decoder) message(data []byte, depth int) (*Message, error) {
	r := reader{b: data}
	if sig, ok := r.u32(); !ok || sig != signature {
		return nil, ErrNotTNEF
	}
	if _, ok := r.u16(); !ok {
		return nil, malformed("legacy key truncated")
	}

	var (
		oemCP                uint32
		class, subject, body []byte
		msgProps             props
		cur                  *rawAttachment
		atts                 []*rawAttachment
		err                  error
	)
	for r.remaining() > 0 {
		if r.remaining() < attrHeaderLen && isTrailer(r.b[r.off:]) {
			break
		}
		level, _ := r.u8()
		tag, ok1 := r.u32()
		n, ok2 := r.u32()
		if !ok1 || !ok2 {
			return nil, malformed("attribute header truncated")
		}
		val, ok := r.bytes(uint64(n))
		if !ok {
			return nil, malformed("attribute %#08x truncated", tag)
		}
		sum, ok := r.u16()
		if !ok {
			return nil, malformed("attribute %#08x checksum truncated", tag)
		}
		if checksum(val) != sum {
			return nil, malformed("attribute %#08x checksum mismatch", tag)
		}

		id := uint16(tag)
		switch level {
		case levelMessage:
			switch id {
			case attOemCodepage:
				if len(val) >= 4 {
					oemCP = binary.LittleEndian.Uint32(val)
				}
			case attMessageClass:
				class = val
			case attSubject:
				subject = val
			case attBody:
				body = val
			case attMsgProps:
				if msgProps, err = parseProps(val); err != nil {
					return nil, err
				}
			}
		case levelAttachment:
			if id == attAttachRenddata || cur == nil {
				cur = &rawAttachment{}
				atts = append(atts, cur)
			}
			switch id {
			case attAttachRenddata:
				if len(val) >= 2 {
					cur.renderType = binary.LittleEndian.Uint16(val)
				}
			case attAttachTitle:
				cur.title = val
			case attAttachData:
				cur.data = val
			case attAttachment:
				if cur.props, err = parseProps(val); err != nil {
					return nil, err
				}
			}
		}
	}

	oem := lookupCodepage(oemCP)
	msgCP := oem
	if cp, ok := msgProps.u32(prMessageCodepage); ok {
		msgCP = lookupCodepage(cp)
	} else if cp, ok := msgProps.u32(prInternetCPID); ok {
		msgCP = lookupCodepage(cp)
	}

	m := &Message{Class: oem.decode(class)}
	if m.Subject = msgProps.str(prSubject, msgCP); m.Subject == "" {
		m.Subject = oem.decode(subject)
	}
	if m.Body = msgProps.str(prBody, msgCP); m.Body == "" {
		m.Body = oem.decode(body)
	}
	m.TransportHeaders = msgProps.str(prTransportMessageHeaders, msgCP)

	if p, ok := msgProps[prBodyHTML]; ok && len(p.vals) > 0 {
		htmlCP := msgCP
		if cp, ok := msgProps.u32(prInternetCPID); ok {
			htmlCP = lookupCodepage(cp)
		}
		switch p.typ {
		case ptypString:
			m.HTML, m.HTMLCharset = []byte(decodeUTF16(p.vals[0])), "utf-8"
		case ptypString8:
			m.HTML, m.HTMLCharset = trimNUL(p.vals[0]), htmlCP.name
		default:
			m.HTML, m.HTMLCharset = p.vals[0], htmlCP.name
		}
	}
	if raw, ok := msgProps.bin(prRTFCompressed); ok {
		if m.RTF, err = decompressRTF(raw, &d.rtfBudget); err != nil {
			return nil, err
		}
	}

	if len(atts) > 0 {
		m.Attachments = make([]Attachment, 0, len(atts))
	}
	for _, ra := range atts {
		a, err := d.attachment(ra, oem, msgCP, depth)
		if err != nil {
			return nil, err
		}
		m.Attachments = append(m.Attachments, a)
	}
	return m, nil
}

func (d *decoder) attachment(ra *rawAttachment, oem, msgCP codepage, depth int) (Attachment, error) {
	p := ra.props
	a := Attachment{
		ContentType: cleanMediaType(p.str(prAttachMimeTag, msgCP)),
		ContentID:   strings.TrimSpace(p.str(prAttachContentID, msgCP)),
		Data:        ra.data,
	}
	for _, name := range []string{
		p.str(prAttachLongFilename, msgCP),
		p.str(prAttachFilename, msgCP),
		oem.decode(ra.title),
		p.str(prDisplayName, msgCP),
	} {
		if a.Name = cleanName(name); a.Name != "" {
			break
		}
	}
	if h, ok := p.u32(prAttachmentHidden); ok && h != 0 {
		a.Hidden = true
	}
	method, _ := p.u32(prAttachMethod)
	if method == attachOLE || ra.renderType == renderOLE {
		a.Method = OLE
	}

	obj, ok := p[prAttachData]
	if !ok || len(obj.vals) == 0 {
		return a, nil
	}
	if obj.typ != ptypObject {
		a.Data = obj.vals[0]
		return a, nil
	}
	v := obj.vals[0]
	if len(v) < len(iidMessage) {
		return a, malformed("attachment object shorter than its interface id")
	}
	iid, inner := v[:len(iidMessage)], v[len(iidMessage):]
	if !bytes.Equal(iid, iidMessage) || len(inner) < 4 || binary.LittleEndian.Uint32(inner) != signature {
		a.Data = inner
		return a, nil
	}
	if depth+1 > d.maxDepth {
		return a, fmt.Errorf("%w: attached messages nested deeper than %d", ErrLimit, d.maxDepth)
	}
	nested, err := d.message(inner, depth+1)
	if err != nil {
		return a, err
	}
	a.Method, a.Message, a.Data = Embedded, nested, nil
	if a.Name == "" {
		a.Name = cleanName(nested.Subject)
	}
	return a, nil
}

// isTrailer reports bytes a gateway appended after the last attribute, such as
// a line break. Anything else that short is a truncated attribute, which must
// fail rather than drop what it held.
func isTrailer(b []byte) bool {
	for _, c := range b {
		if c != '\r' && c != '\n' && c != ' ' && c != 0 {
			return false
		}
	}
	return true
}

func checksum(b []byte) uint16 {
	var s uint16
	for _, c := range b {
		s += uint16(c)
	}
	return s
}

func malformed(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrMalformed}, args...)...)
}

// cleanName reduces a sender-chosen name to a bare file name, so a writer of
// the attachment never sees a directory part.
func cleanName(s string) string {
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if s == "." || s == ".." {
		return ""
	}
	return s
}

func cleanMediaType(s string) string {
	mt, _, err := mime.ParseMediaType(strings.TrimSpace(s))
	if err != nil || !strings.Contains(mt, "/") {
		return ""
	}
	return mt
}

// reader is a bounds-checked little-endian cursor. Every method reports
// whether the bytes were there instead of panicking.
type reader struct {
	b   []byte
	off int
}

func (r *reader) remaining() int { return len(r.b) - r.off }

func (r *reader) bytes(n uint64) ([]byte, bool) {
	if n > uint64(r.remaining()) {
		return nil, false
	}
	v := r.b[r.off : r.off+int(n) : r.off+int(n)]
	r.off += int(n)
	return v, true
}

func (r *reader) u8() (byte, bool) {
	v, ok := r.bytes(1)
	if !ok {
		return 0, false
	}
	return v[0], true
}

func (r *reader) u16() (uint16, bool) {
	v, ok := r.bytes(2)
	if !ok {
		return 0, false
	}
	return binary.LittleEndian.Uint16(v), true
}

func (r *reader) u32() (uint32, bool) {
	v, ok := r.bytes(4)
	if !ok {
		return 0, false
	}
	return binary.LittleEndian.Uint32(v), true
}
