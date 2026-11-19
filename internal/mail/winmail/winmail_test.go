package winmail

import (
	"bytes"
	"encoding/base64"
	"mime"
	"strings"
	"testing"

	"odac/internal/mail/message"
	"odac/internal/mail/mimetree"
	tt "odac/internal/mail/tnef/tneftest"
)

func b64(b []byte) string {
	var sb strings.Builder
	enc := base64.StdEncoding.EncodeToString(b)
	for len(enc) > 76 {
		sb.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	sb.WriteString(enc + "\r\n")
	return sb.String()
}

// outlook is the layout Outlook sends in RTF mode: a plain text copy of the
// body beside winmail.dat.
func outlook(stream []byte) []byte {
	return []byte("From: Ali <ali@example.com>\r\n" +
		"To: veli@example.org\r\n" +
		"Subject: Rapor\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"outer\"\r\n" +
		"\r\n" +
		"--outer\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"Merhaba, rapor ektedir.\r\n" +
		"--outer\r\n" +
		"Content-Type: application/ms-tnef; name=\"winmail.dat\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"Content-Disposition: attachment; filename=\"winmail.dat\"\r\n" +
		"\r\n" +
		b64(stream) +
		"--outer--\r\n")
}

// whole is a message Exchange sent as one TNEF entity.
func whole(stream []byte) []byte {
	return []byte("From: ali@example.com\r\n" +
		"Subject: Tek parça\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: application/ms-tnef; name=\"winmail.dat\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		b64(stream))
}

type file struct {
	name, ct, cid, disp string
	data                []byte
}

func files(t *testing.T, raw []byte) []file {
	t.Helper()
	var out []file
	for _, a := range mimetree.Parse(raw).Attachments() {
		p := a.Part
		disp, params, _ := mime.ParseMediaType(p.Header.Get("Content-Disposition"))
		out = append(out, file{
			name: params["filename"],
			ct:   p.MediaType(),
			cid:  p.ContentID,
			disp: disp,
			data: p.DecodedBody(raw),
		})
	}
	return out
}

func mustExpand(t *testing.T, raw []byte) []byte {
	t.Helper()
	out, err := Expand(raw)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil {
		t.Fatal("nothing converted")
	}
	if !bytes.HasPrefix(out, []byte(Header+": converted\r\n")) {
		t.Errorf("missing %s header:\n%s", Header, out)
	}
	return out
}

func TestExpandOutlookAttachments(t *testing.T) {
	pdf := []byte("%PDF-1.7 rapor")
	logo := []byte("\x89PNG logo")
	inline := (&tt.Props{}).
		Unicode(tt.PrAttachLongFilename, "logo.png").
		String8(tt.PrAttachMimeTag, []byte("image/png")).
		String8(tt.PrAttachContentID, []byte("logo@01D0")).
		Boolean(tt.PrAttachmentHidden, true)
	stream := tt.New().
		Class("IPM.Note").
		Attr(tt.LevelMessage, tt.AttMsgProps, (&tt.Props{}).
			Binary(tt.PrRTFCompressed, tt.TypeBinary, tt.RTFExample1).Bytes()).
		File("Rapor ş.pdf", "application/pdf", pdf).
		Attr(tt.LevelAttachment, tt.AttAttachRenddata, tt.Renddata()).
		Attr(tt.LevelAttachment, tt.AttAttachData, logo).
		Attr(tt.LevelAttachment, tt.AttAttachment, inline.Bytes()).
		Bytes()
	raw := outlook(stream)
	out := mustExpand(t, raw)

	// Everything before winmail.dat is spliced through untouched.
	head := raw[:bytes.Index(raw, []byte("Content-Type: application/ms-tnef"))]
	if !bytes.HasPrefix(out[len(Header+": converted\r\n"):], head) {
		t.Errorf("bytes before the TNEF part changed:\n%s", out)
	}

	if root := mimetree.Parse(out); len(root.Children) != 3 {
		t.Errorf("files not flattened into the outer multipart:\n%s", out)
	}
	got := files(t, out)
	want := []file{
		{name: "Rapor ş.pdf", ct: "application/pdf", disp: "attachment", data: pdf},
		{name: "logo.png", ct: "image/png", cid: "<logo@01D0>", disp: "inline", data: logo},
	}
	if len(got) != len(want) {
		t.Fatalf("attachments = %+v", got)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.name != w.name || g.ct != w.ct || g.cid != w.cid || g.disp != w.disp || !bytes.Equal(g.data, w.data) {
			t.Errorf("attachment %d = %+v, want %+v", i, g, w)
		}
	}

	// The body beside winmail.dat stays the only body: the RTF copy of it is
	// not added a second time.
	p := message.Parse(out)
	if p.Text != "Merhaba, rapor ektedir." || p.HTML != "" {
		t.Errorf("text %q html %q", p.Text, p.HTML)
	}
	if again, err := Expand(out); again != nil || err != nil {
		t.Errorf("second pass = %q, %v", again, err)
	}
}

func TestExpandRemovesEmptyWinmail(t *testing.T) {
	stream := tt.New().
		Class("IPM.Note").
		Attr(tt.LevelMessage, tt.AttMsgProps, (&tt.Props{}).
			Binary(tt.PrRTFCompressed, tt.TypeBinary, tt.RTFExample1).Bytes()).
		Bytes()
	out := mustExpand(t, outlook(stream))
	if got := files(t, out); len(got) != 0 {
		t.Errorf("attachments = %+v", got)
	}
	root := mimetree.Parse(out)
	if len(root.Children) != 1 || root.Children[0].MediaType() != "text/plain" {
		t.Fatalf("structure:\n%s", out)
	}
	if !bytes.HasSuffix(out, []byte("Merhaba, rapor ektedir.\r\n--outer--\r\n")) {
		t.Errorf("text part or closing delimiter damaged:\n%s", out)
	}
}

func TestExpandWholeMessage(t *testing.T) {
	html := []byte("<p>G\xfczel</p>")
	stream := tt.New().
		Attr(tt.LevelMessage, tt.AttMsgProps, (&tt.Props{}).
			Int32(tt.PrInternetCPID, 1254).
			Unicode(tt.PrBody, "Güzel").
			Binary(tt.PrBodyHTML, tt.TypeBinary, html).Bytes()).
		File("tablo.xlsx", "", []byte("PK xlsx")).
		Bytes()
	out := mustExpand(t, whole(stream))

	root := mimetree.Parse(out)
	if root.MediaType() != "multipart/mixed" {
		t.Fatalf("root is %s:\n%s", root.MediaType(), out)
	}
	if root.Header.Get("Subject") != "Tek parça" || root.Header.Get("From") != "ali@example.com" {
		t.Errorf("message headers lost: %v", root.Header)
	}
	if cte := root.Header.Values("Content-Transfer-Encoding"); len(cte) != 0 {
		t.Errorf("root kept Content-Transfer-Encoding %q", cte)
	}
	if n := len(root.Header.Values("Mime-Version")); n != 1 {
		t.Errorf("%d MIME-Version fields", n)
	}
	alt := root.Children[0]
	if alt.MediaType() != "multipart/alternative" || len(alt.Children) != 2 {
		t.Fatalf("body is %s", alt.MediaType())
	}
	if h := alt.Children[1]; h.Params["charset"] != "windows-1254" || !bytes.Equal(h.DecodedBody(out), html) {
		t.Errorf("html part %v %q", h.Params, h.DecodedBody(out))
	}
	if p := message.Parse(out); p.Text != "Güzel" {
		t.Errorf("text = %q", p.Text)
	}
	got := files(t, out)
	if len(got) != 1 || got[0].name != "tablo.xlsx" || string(got[0].data) != "PK xlsx" {
		t.Errorf("attachments = %+v", got)
	}
}

func TestExpandRTFOnlyBodyBecomesFile(t *testing.T) {
	stream := tt.New().
		Attr(tt.LevelMessage, tt.AttMsgProps, (&tt.Props{}).
			Binary(tt.PrRTFCompressed, tt.TypeBinary, tt.RTFExample1).Bytes()).
		Bytes()
	got := files(t, mustExpand(t, whole(stream)))
	if len(got) != 1 || got[0].name != "message.rtf" || string(got[0].data) != tt.RTFExample1Text {
		t.Errorf("attachments = %+v", got)
	}
}

func TestExpandInlineImagesJoinTheHTML(t *testing.T) {
	inline := (&tt.Props{}).
		Unicode(tt.PrAttachLongFilename, "image001.png").
		String8(tt.PrAttachContentID, []byte("<image001.png@01D0>")).
		Boolean(tt.PrAttachmentHidden, true)
	stream := tt.New().
		Attr(tt.LevelMessage, tt.AttMsgProps, (&tt.Props{}).
			Binary(tt.PrBodyHTML, tt.TypeBinary, []byte(`<img src="cid:image001.png@01D0">`)).Bytes()).
		Attr(tt.LevelAttachment, tt.AttAttachRenddata, tt.Renddata()).
		Attr(tt.LevelAttachment, tt.AttAttachData, []byte("\x89PNG")).
		Attr(tt.LevelAttachment, tt.AttAttachment, inline.Bytes()).
		Bytes()
	out := mustExpand(t, whole(stream))
	root := mimetree.Parse(out)
	if root.MediaType() != "multipart/related" || len(root.Children) != 2 {
		t.Fatalf("root is %s:\n%s", root.MediaType(), out)
	}
	img := root.Children[1]
	if img.ContentID != "<image001.png@01D0>" || img.MediaType() != "image/png" {
		t.Errorf("image part %s %q", img.MediaType(), img.ContentID)
	}
}

func TestExpandEmbeddedMessage(t *testing.T) {
	inner := tt.New().
		Attr(tt.LevelMessage, tt.AttMsgProps, (&tt.Props{}).
			Unicode(tt.PrSubject, "Eski konu").
			Unicode(tt.PrTransportMessageHeaders, "From: a@example.com\r\n"+
				"Subject: =?utf-8?q?=C3=96nceki?=\r\n"+
				"Content-Type: multipart/mixed; boundary=gone\r\n"+
				"X-Bad: \xc3\xa7\r\n").
			Unicode(tt.PrBody, "iç gövde").Bytes()).
		File("deep.txt", "text/plain", []byte("deep")).
		Bytes()
	stream := tt.New().Embed(inner, "Önceki.msg").Bytes()
	out := mustExpand(t, outlook(stream))

	var msg *mimetree.Part
	for _, a := range mimetree.Parse(out).Attachments() {
		if a.Part.Nested != nil {
			msg = a.Part
		}
	}
	if msg == nil {
		t.Fatalf("no attached message:\n%s", out)
	}
	_, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Disposition"))
	if params["filename"] != "Önceki.eml" {
		t.Errorf("filename = %q", params["filename"])
	}
	h := msg.Nested.Header
	if h.Get("From") != "a@example.com" || h.Get("Subject") != "=?utf-8?q?=C3=96nceki?=" {
		t.Errorf("inner headers = %v", h)
	}
	if h.Get("X-Bad") != "" || strings.Contains(h.Get("Content-Type"), "gone") {
		t.Errorf("inner headers kept what they should not: %v", h)
	}
	got := files(t, out)
	if len(got) != 2 || got[1].name != "deep.txt" || string(got[1].data) != "deep" {
		t.Errorf("attachments = %+v", got)
	}
}

func TestExpandSubjectWithoutTransportHeaders(t *testing.T) {
	inner := tt.New().
		Attr(tt.LevelMessage, tt.AttMsgProps, (&tt.Props{}).Unicode(tt.PrSubject, "Toplantı").Bytes()).
		Bytes()
	out := mustExpand(t, outlook(tt.New().Embed(inner, "").Bytes()))
	for _, a := range mimetree.Parse(out).Attachments() {
		if a.Part.Nested == nil {
			continue
		}
		s, err := new(mime.WordDecoder).DecodeHeader(a.Part.Nested.Header.Get("Subject"))
		if err != nil || s != "Toplantı" {
			t.Errorf("subject = %q, %v", s, err)
		}
		return
	}
	t.Fatalf("no attached message:\n%s", out)
}

func TestExpandLeavesOtherMessagesAlone(t *testing.T) {
	notTNEF := []byte("From: a@example.com\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nhi\r\n" +
		"--b\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=WINMAIL.DAT\r\n\r\n" +
		"just a file\r\n--b--\r\n")
	meeting := outlook(tt.New().Class("IPM.Schedule.Meeting.Request").File("a.txt", "", []byte("a")).Bytes())
	for name, raw := range map[string][]byte{
		"plain":           []byte("From: a@example.com\r\nSubject: hi\r\n\r\nhello\r\n"),
		"named only":      notTNEF,
		"meeting request": meeting,
	} {
		if out, err := Expand(raw); out != nil || err != nil {
			t.Errorf("%s: Expand = %q, %v", name, out, err)
		}
	}
}

func TestExpandRefusesDamagedStreams(t *testing.T) {
	good := tt.New().File("a.pdf", "", []byte("pdf")).Bytes()
	for name, stream := range map[string][]byte{
		"truncated": good[:len(good)-3],
		"not tnef":  []byte("PK\x03\x04 zip"),
	} {
		if out, err := Expand(outlook(stream)); out != nil || err == nil {
			t.Errorf("%s: Expand = %q, %v; want nil and an error", name, out, err)
		}
	}
}

func TestExpandKeepsBareLineFeeds(t *testing.T) {
	raw := bytes.ReplaceAll(outlook(tt.New().File("a.pdf", "", []byte("pdf")).Bytes()), []byte("\r\n"), []byte("\n"))
	out, err := Expand(raw)
	if err != nil || out == nil {
		t.Fatalf("Expand = %q, %v", out, err)
	}
	if bytes.Contains(out, []byte("\r")) {
		t.Errorf("CR spliced into an LF message:\n%q", out)
	}
	if got := files(t, out); len(got) != 1 || string(got[0].data) != "pdf" {
		t.Errorf("attachments = %+v", got)
	}
}

func TestEnabledFromEnv(t *testing.T) {
	for v, want := range map[string]bool{"": true, "on": true, "OFF": false, "off": false, "bogus": true} {
		t.Setenv("ODAC_MAIL_TNEF", v)
		if got := EnabledFromEnv(); got != want {
			t.Errorf("ODAC_MAIL_TNEF=%q: %v, want %v", v, got, want)
		}
	}
}

func FuzzExpand(f *testing.F) {
	f.Add(outlook(tt.New().Class("IPM.Note").File("a.pdf", "application/pdf", []byte("pdf")).Bytes()))
	f.Add(whole(tt.New().Embed(tt.New().File("x", "", []byte("x")).Bytes(), "m").Bytes()))
	f.Add(whole(tt.New().Attr(tt.LevelMessage, tt.AttMsgProps, (&tt.Props{}).
		Binary(tt.PrRTFCompressed, tt.TypeBinary, tt.RTFExample2).Bytes()).Bytes()))
	f.Fuzz(func(t *testing.T, raw []byte) {
		out, err := expand(raw)
		if out != nil && err != nil {
			t.Fatalf("returned both a message and %v", err)
		}
		if out != nil && !bytes.HasPrefix(out, []byte(Header+": converted")) {
			t.Fatalf("converted message lacks the header")
		}
	})
}
