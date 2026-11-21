package proto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
	"testing/iotest"
)

func randomData(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

func TestStreamRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for _, size := range []int{0, 1, MaxChunk - 1, MaxChunk, MaxChunk + 1, 3*MaxChunk + 17} {
		want := randomData(r, size)
		for _, viaCopy := range []bool{false, true} {
			var wire bytes.Buffer
			sw := NewStreamWriter(&wire)
			var err error
			if viaCopy {
				_, err = io.Copy(sw, iotest.HalfReader(bytes.NewReader(want)))
			} else {
				_, err = sw.Write(want)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := sw.Close(); err != nil {
				t.Fatal(err)
			}
			wire.WriteString("trailing bytes the stream must not read")

			sr := NewStreamReader(&wire)
			got, err := io.ReadAll(iotest.HalfReader(sr))
			if err != nil {
				t.Fatalf("size %d: %v", size, err)
			}
			if !bytes.Equal(got, want) || !sr.Done() || sr.Err() != nil {
				t.Fatalf("size %d (copy %v): got %d bytes, done %v, err %v", size, viaCopy, len(got), sr.Done(), sr.Err())
			}
			if !strings.HasPrefix(wire.String(), "trailing") {
				t.Fatalf("size %d: the reader went past the end frame", size)
			}
		}
	}
}

// A stream that stops without its end frame, at a frame boundary or inside
// a frame, is a dump that died. It must never read as a complete one.
func TestStreamWithoutEndFrameIsTruncated(t *testing.T) {
	var wire bytes.Buffer
	sw := NewStreamWriter(&wire)
	sw.Write(bytes.Repeat([]byte("x"), 1000))
	whole := wire.Bytes()
	for _, cut := range []int{0, 2, len(whole) - 500, len(whole)} {
		sr := NewStreamReader(bytes.NewReader(whole[:cut]))
		_, err := io.ReadAll(sr)
		if !errors.Is(err, ErrTruncated) || !errors.Is(sr.Err(), ErrTruncated) || sr.Done() {
			t.Fatalf("cut at %d: err %v, Err %v", cut, err, sr.Err())
		}
	}

	var big [4]byte
	binary.BigEndian.PutUint32(big[:], MaxChunk+1)
	sr := NewStreamReader(bytes.NewReader(big[:]))
	if _, err := io.ReadAll(sr); err == nil || errors.Is(err, ErrTruncated) {
		t.Fatalf("oversized frame: %v", err)
	}

	sr = NewStreamReader(iotest.ErrReader(errors.New("pipe broke")))
	if _, err := io.ReadAll(sr); err == nil || errors.Is(err, ErrTruncated) {
		t.Fatalf("read failure reported as %v", err)
	}
}

func validRequest() *Request {
	return &Request{
		Version: Version,
		Op:      OpBackup,
		Repo: Repo{
			Storage:  Storage{Filesystem: &FilesystemStorage{Path: "/target"}},
			Password: "correct horse battery staple",
			ServerID: "0123456789abcdef0123456789abcdef",
			CacheDir: "/cache",
		},
		Backup: &Backup{Run: "r1", Trigger: "schedule", Items: []Item{
			{Source: Source{Job: "j", App: "db", Name: "postgres"}, Consistency: "consistent", Stream: "dump.sql"},
			{Source: Source{Job: "j", App: "web", Name: "files"}, Consistency: "live", Dir: "/data/web"},
		}},
	}
}

func TestRequestRoundTrip(t *testing.T) {
	var wire bytes.Buffer
	if err := WriteRequest(&wire, validRequest()); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRequest(&wire)
	if err != nil {
		t.Fatal(err)
	}
	if !got.HasStream() || got.Backup.Items[1].Dir != "/data/web" || got.Repo.Password != validRequest().Repo.Password {
		t.Fatalf("round trip: %+v", got)
	}
}

func TestValidate(t *testing.T) {
	for name, c := range map[string]struct {
		mutate func(*Request)
		code   string
	}{
		"old version":        {func(r *Request) { r.Version = MinVersion - 1 }, CodeVersion},
		"new version":        {func(r *Request) { r.Version = Version + 1 }, CodeVersion},
		"unknown op":         {func(r *Request) { r.Op = "format" }, CodeBadRequest},
		"missing args":       {func(r *Request) { r.Backup = nil }, CodeBadRequest},
		"foreign args":       {func(r *Request) { r.List = &List{} }, CodeBadRequest},
		"args on init":       {func(r *Request) { r.Op = OpInit }, CodeBadRequest},
		"no run":             {func(r *Request) { r.Backup.Run = "" }, CodeBadRequest},
		"no items":           {func(r *Request) { r.Backup.Items = nil }, CodeBadRequest},
		"stream second":      {func(r *Request) { r.Backup.Items[0], r.Backup.Items[1] = r.Backup.Items[1], r.Backup.Items[0] }, CodeBadRequest},
		"dir and stream":     {func(r *Request) { r.Backup.Items[1].Stream = "x" }, CodeBadRequest},
		"neither":            {func(r *Request) { r.Backup.Items[1].Dir = "" }, CodeBadRequest},
		"relative dir":       {func(r *Request) { r.Backup.Items[1].Dir = "data" }, CodeBadRequest},
		"empty delete":       {func(r *Request) { r.Op, r.Backup, r.Delete = OpDelete, nil, &Delete{} }, CodeBadRequest},
		"verify percent":     {func(r *Request) { r.Op, r.Backup, r.Verify = OpVerify, nil, &Verify{Percent: 101} }, CodeBadRequest},
		"restore without id": {func(r *Request) { r.Op, r.Backup, r.Restore = OpRestore, nil, &Restore{} }, CodeBadRequest},
		"restore relative":   {func(r *Request) { r.Op, r.Backup, r.Restore = OpRestore, nil, &Restore{ID: "k1", Dir: "out"} }, CodeBadRequest},
		"owner read-only":    {func(r *Request) { r.Repo.ReadOnly, r.Repo.Owner = true, &Owner{Seq: 1} }, CodeBadRequest},
		"owner zero seq":     {func(r *Request) { r.Repo.Owner = &Owner{} }, CodeBadRequest},
		"adopt on backup":    {func(r *Request) { r.Repo.Owner = &Owner{Seq: 1, Adopt: true} }, CodeBadRequest},
	} {
		r := validRequest()
		c.mutate(r)
		var e *Error
		if err := r.Validate(); !errors.As(err, &e) || e.Code != c.code {
			t.Errorf("%s: %v, want code %s", name, err, c.code)
		}
	}
	for _, op := range []string{OpInit, OpUsage} {
		r := validRequest()
		r.Op, r.Backup = op, nil
		if err := r.Validate(); err != nil {
			t.Errorf("%s: %v", op, err)
		}
	}
}

func TestReadRequestRejectsBadHeaders(t *testing.T) {
	frame := func(body string) []byte {
		b := binary.BigEndian.AppendUint32(nil, uint32(len(body)))
		return append(b, body...)
	}
	huge := binary.BigEndian.AppendUint32(nil, MaxHeader+1)
	for name, wire := range map[string][]byte{
		"empty":     nil,
		"zero":      {0, 0, 0, 0},
		"huge":      huge,
		"short":     frame(`{"version":1}`)[:8],
		"not json":  frame("hello"),
		"no op":     frame(`{"version":1}`),
		"version 0": frame(`{"op":"usage"}`),
	} {
		var e *Error
		if _, err := ReadRequest(bytes.NewReader(wire)); !errors.As(err, &e) {
			t.Errorf("%s: %v is not an *Error", name, err)
		}
	}
}

func TestSecretsAreListed(t *testing.T) {
	r := validRequest()
	r.Repo.Storage = Storage{S3: &S3Storage{SecretAccessKey: "s3-secret", SessionToken: "s3-token"}}
	got := strings.Join(r.Secrets(), ",")
	for _, want := range []string{r.Repo.Password, "s3-secret", "s3-token"} {
		if !strings.Contains(got, want) {
			t.Errorf("Secrets misses %q", want)
		}
	}
	r.Repo.Storage = Storage{WebDAV: &WebDAVStorage{Password: "dav-pass"}}
	if !strings.Contains(strings.Join(r.Secrets(), ","), "dav-pass") {
		t.Error("Secrets misses the WebDAV password")
	}
}
