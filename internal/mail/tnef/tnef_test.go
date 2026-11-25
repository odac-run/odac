package tnef

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	tt "odac/internal/mail/tnef/tneftest"
)

func TestDecodeAttachments(t *testing.T) {
	pdf := []byte("%PDF-1.7 fake")
	png := []byte("\x89PNG fake")
	logo := []byte("\x89PNG logo")

	first := (&tt.Props{}).
		Unicode(tt.PrAttachLongFilename, `C:\Users\x\Rapor ş.pdf`).
		String8(tt.PrAttachMimeTag, []byte("application/pdf")).
		Int32(tt.PrAttachMethod, 1)
	inline := (&tt.Props{}).
		Unicode(tt.PrAttachLongFilename, "logo.png").
		String8(tt.PrAttachMimeTag, []byte("image/png")).
		String8(tt.PrAttachContentID, []byte("logo@01D0")).
		Boolean(tt.PrAttachmentHidden, true)

	s := tt.New().
		Codepage(1254).
		Attr(tt.LevelMessage, tt.AttMessageClass, []byte("IPM.Note\x00")).
		Attr(tt.LevelAttachment, tt.AttAttachRenddata, tt.Renddata()).
		Attr(tt.LevelAttachment, tt.AttAttachTitle, []byte("RAPOR~1.PDF\x00")).
		Attr(tt.LevelAttachment, tt.AttAttachData, pdf).
		Attr(tt.LevelAttachment, tt.AttAttachment, first.Bytes()).
		Attr(tt.LevelAttachment, tt.AttAttachRenddata, tt.Renddata()).
		Attr(tt.LevelAttachment, tt.AttAttachData, png).
		Attr(tt.LevelAttachment, tt.AttAttachTitle, []byte("\xe7izim.png\x00")).
		Attr(tt.LevelAttachment, tt.AttAttachRenddata, tt.Renddata()).
		Attr(tt.LevelAttachment, tt.AttAttachData, logo).
		Attr(tt.LevelAttachment, tt.AttAttachment, inline.Bytes())

	m, err := Decode(s.Bytes(), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if m.Class != "IPM.Note" {
		t.Errorf("class = %q", m.Class)
	}
	want := []Attachment{
		{Name: "Rapor ş.pdf", ContentType: "application/pdf", Data: pdf},
		{Name: "çizim.png", Data: png},
		{Name: "logo.png", ContentType: "image/png", ContentID: "logo@01D0", Hidden: true, Data: logo},
	}
	if len(m.Attachments) != len(want) {
		t.Fatalf("got %d attachments, want %d", len(m.Attachments), len(want))
	}
	for i, w := range want {
		g := m.Attachments[i]
		if g.Name != w.Name || g.ContentType != w.ContentType || g.ContentID != w.ContentID ||
			g.Hidden != w.Hidden || g.Method != ByValue || !bytes.Equal(g.Data, w.Data) {
			t.Errorf("attachment %d = %+v, want %+v", i, g, w)
		}
	}
}

func TestDecodeBodies(t *testing.T) {
	html := []byte("<html><meta charset=windows-1254><p>G\xfcle g\xfcle</p></html>")
	mp := (&tt.Props{}).
		Int32(tt.PrInternetCPID, 1254).
		Unicode(tt.PrSubject, "Toplantı notları").
		Unicode(tt.PrBody, "Merhaba dünya").
		Binary(tt.PrBodyHTML, tt.TypeBinary, html).
		Binary(tt.PrRTFCompressed, tt.TypeBinary, tt.RTFExample1)
	// A named property and a multi-valued one sit between the ones read.
	mp.Tag(tt.TypeString, 0x8001).
		Raw(make([]byte, 16)).
		Raw(binary.LittleEndian.AppendUint32(nil, 1)).
		Raw(binary.LittleEndian.AppendUint32(nil, 8)).
		Raw([]byte("K\x00e\x00y\x00\x00\x00")).
		Value([]byte("v\x00\x00\x00"))
	mp.Tag(tt.TypeInteger16|tt.MultiValued, 0x0E07).
		Raw(binary.LittleEndian.AppendUint32(nil, 3)).
		Raw(make([]byte, 12))

	m, err := Decode(tt.New().Attr(tt.LevelMessage, tt.AttMsgProps, mp.Bytes()).Bytes(), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if m.Subject != "Toplantı notları" || m.Body != "Merhaba dünya" {
		t.Errorf("subject %q body %q", m.Subject, m.Body)
	}
	if !bytes.Equal(m.HTML, html) || m.HTMLCharset != "windows-1254" {
		t.Errorf("html %q charset %q", m.HTML, m.HTMLCharset)
	}
	if string(m.RTF) != tt.RTFExample1Text {
		t.Errorf("rtf = %q", m.RTF)
	}
}

func TestDecodeLegacyBodyFallsBack(t *testing.T) {
	s := tt.New().
		Codepage(1254).
		Attr(tt.LevelMessage, tt.AttSubject, []byte("\xdeubat\x00")).
		Attr(tt.LevelMessage, tt.AttBody, []byte("G\xfcnayd\xfdn"))
	m, err := Decode(s.Bytes(), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if m.Subject != "Şubat" || m.Body != "Günaydın" {
		t.Errorf("subject %q body %q", m.Subject, m.Body)
	}
}

func embed(inner []byte, name string) []byte { return tt.New().Embed(inner, name).Bytes() }

func TestDecodeEmbeddedMessage(t *testing.T) {
	innerProps := (&tt.Props{}).
		Unicode(tt.PrSubject, "Forwarded: report").
		Unicode(tt.PrTransportMessageHeaders, "From: a@example.com\r\n")
	inner := tt.New().
		Attr(tt.LevelMessage, tt.AttMsgProps, innerProps.Bytes()).
		Attr(tt.LevelAttachment, tt.AttAttachRenddata, tt.Renddata()).
		Attr(tt.LevelAttachment, tt.AttAttachTitle, []byte("deep.txt\x00")).
		Attr(tt.LevelAttachment, tt.AttAttachData, []byte("deep"))

	m, err := Decode(embed(inner.Bytes(), ""), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	a := m.Attachments[0]
	if a.Method != Embedded || a.Message == nil || a.Data != nil {
		t.Fatalf("attachment = %+v", a)
	}
	if a.Name != "Forwarded: report" || a.Message.TransportHeaders != "From: a@example.com\r\n" {
		t.Errorf("name %q headers %q", a.Name, a.Message.TransportHeaders)
	}
	if got := a.Message.Attachments; len(got) != 1 || string(got[0].Data) != "deep" {
		t.Errorf("inner attachments = %+v", got)
	}

	twice := embed(embed(inner.Bytes(), "x"), "y")
	if _, err := Decode(twice, Limits{MaxDepth: 1}); !errors.Is(err, ErrLimit) {
		t.Errorf("depth 2 under MaxDepth 1: err = %v, want ErrLimit", err)
	}
	if _, err := Decode(twice, Limits{MaxDepth: 2}); err != nil {
		t.Errorf("depth 2 under MaxDepth 2: %v", err)
	}
}

func TestDecodeOLEObject(t *testing.T) {
	storage := []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}
	iid := make([]byte, 16)
	p := (&tt.Props{}).Int32(tt.PrAttachMethod, tt.AttachOLE).Binary(tt.PrAttachData, tt.TypeObject, append(iid, storage...))
	s := tt.New().
		Attr(tt.LevelAttachment, tt.AttAttachRenddata, tt.Renddata()).
		Attr(tt.LevelAttachment, tt.AttAttachTitle, []byte("Untitled\x00")).
		Attr(tt.LevelAttachment, tt.AttAttachment, p.Bytes())
	m, err := Decode(s.Bytes(), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if a := m.Attachments[0]; a.Method != OLE || !bytes.Equal(a.Data, storage) {
		t.Errorf("attachment = %+v", a)
	}
}

func TestDecodeRejects(t *testing.T) {
	valid := tt.New().Attr(tt.LevelMessage, tt.AttSubject, []byte("hi\x00")).Bytes()

	badSum := bytes.Clone(valid)
	badSum[len(badSum)-1] ^= 0xFF

	lyingCount := binary.LittleEndian.AppendUint32(nil, 0xFFFFFFFF)

	unknownType := (&tt.Props{})
	unknownType.Tag(0x0001, 0x0E00)

	hugeRTF := bytes.Clone(tt.RTFExample1)
	binary.LittleEndian.PutUint32(hugeRTF[4:], 1<<30)

	cases := []struct {
		name string
		in   []byte
		want error
	}{
		{"empty", nil, ErrNotTNEF},
		{"wrong signature", []byte("PK\x03\x04 not tnef"), ErrNotTNEF},
		{"no legacy key", valid[:5], ErrMalformed},
		{"truncated attribute", valid[:len(valid)-4], ErrMalformed},
		{"checksum mismatch", badSum, ErrMalformed},
		{"property count lies", tt.New().Attr(tt.LevelMessage, tt.AttMsgProps, lyingCount).Bytes(), ErrMalformed},
		{"unknown property type", tt.New().Attr(tt.LevelMessage, tt.AttMsgProps, unknownType.Bytes()).Bytes(), ErrMalformed},
		{"rtf over budget", tt.New().Attr(tt.LevelMessage, tt.AttMsgProps,
			(&tt.Props{}).Binary(tt.PrRTFCompressed, tt.TypeBinary, hugeRTF).Bytes()).Bytes(), ErrLimit},
		{"object without iid", embedRaw([]byte{1, 2, 3}), ErrMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := Decode(tc.in, Limits{})
			if !errors.Is(err, tc.want) || m != nil {
				t.Errorf("Decode = %v, %v; want nil, %v", m, err, tc.want)
			}
		})
	}
}

func embedRaw(obj []byte) []byte {
	p := (&tt.Props{}).Binary(tt.PrAttachData, tt.TypeObject, obj)
	return tt.New().Attr(tt.LevelAttachment, tt.AttAttachment, p.Bytes()).Bytes()
}

func TestDecodeIgnoresShortTrailer(t *testing.T) {
	in := append(tt.New().Attr(tt.LevelMessage, tt.AttSubject, []byte("hi\x00")).Bytes(), '\r', '\n')
	m, err := Decode(in, Limits{})
	if err != nil || m.Subject != "hi" {
		t.Errorf("Decode = %+v, %v", m, err)
	}
}

func TestCleanName(t *testing.T) {
	for in, want := range map[string]string{
		`C:\Users\x\..\evil.exe`: "evil.exe",
		"../../etc/passwd":       "passwd",
		"..":                     "",
		"a\x00b\r\n.txt":         "ab.txt",
		"  report.pdf ":          "report.pdf",
	} {
		if got := cleanName(in); got != want {
			t.Errorf("cleanName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDecompressRTF(t *testing.T) {
	mela := append(binary.LittleEndian.AppendUint32(nil, 12+5), 5, 0, 0, 0, 'M', 'E', 'L', 'A', 0, 0, 0, 0)
	mela = append(mela, "{\\rtf"...)

	badCRC := bytes.Clone(tt.RTFExample1)
	badCRC[12] ^= 1

	short := bytes.Clone(tt.RTFExample1)
	binary.LittleEndian.PutUint32(short[4:], 10)

	cases := []struct {
		name string
		in   []byte
		want string
		err  error
	}{
		{"spec example 1", tt.RTFExample1, tt.RTFExample1Text, nil},
		{"spec example 2", tt.RTFExample2, tt.RTFExample2Text, nil},
		{"uncompressed", mela, "{\\rtf", nil},
		{"crc mismatch", badCRC, "", ErrMalformed},
		{"longer than declared", short, "", ErrMalformed},
		{"compsize past end", tt.RTFExample1[:30], "", ErrMalformed},
		{"header truncated", tt.RTFExample1[:8], "", ErrMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			budget := 1 << 20
			got, err := decompressRTF(tc.in, &budget)
			if !errors.Is(err, tc.err) || string(got) != tc.want {
				t.Errorf("decompressRTF = %q, %v; want %q, %v", got, err, tc.want, tc.err)
			}
			if err == nil && budget != 1<<20-len(tc.want) {
				t.Errorf("budget = %d, want %d", budget, 1<<20-len(tc.want))
			}
		})
	}

	budget := len(tt.RTFExample1Text) - 1
	if _, err := decompressRTF(tt.RTFExample1, &budget); !errors.Is(err, ErrLimit) {
		t.Errorf("over budget: err = %v, want ErrLimit", err)
	}
}

func TestPrebufLength(t *testing.T) {
	if len(rtfPrebuf) != 207 {
		t.Fatalf("prebuf is %d bytes, [MS-OXRTFCP] defines 207", len(rtfPrebuf))
	}
}

func FuzzDecode(f *testing.F) {
	mp := (&tt.Props{}).
		Unicode(tt.PrBody, "body").
		Binary(tt.PrRTFCompressed, tt.TypeBinary, tt.RTFExample2).
		Int32(tt.PrInternetCPID, 65001)
	f.Add(tt.New().Codepage(1252).Attr(tt.LevelMessage, tt.AttMsgProps, mp.Bytes()).Bytes())
	f.Add(embed(embed(tt.New().Attr(tt.LevelAttachment, tt.AttAttachData, []byte("x")).Bytes(), "a"), "b"))
	f.Add(embedRaw(make([]byte, 20)))
	f.Fuzz(func(t *testing.T, in []byte) {
		m, err := decode(in, Limits{MaxRTFBytes: 1 << 20})
		if (m == nil) == (err == nil) {
			t.Fatalf("decode returned %v and %v", m, err)
		}
	})
}

func FuzzDecompressRTF(f *testing.F) {
	f.Add(tt.RTFExample1)
	f.Add(tt.RTFExample2)
	f.Fuzz(func(t *testing.T, in []byte) {
		budget := 1 << 20
		out, err := decompressRTF(in, &budget)
		if err == nil && budget+len(out) != 1<<20 {
			t.Fatalf("budget %d not charged for %d bytes", budget, len(out))
		}
	})
}
