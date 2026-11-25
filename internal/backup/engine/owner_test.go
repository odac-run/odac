package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"golang.org/x/net/webdav"
)

const ownerID = "0123456789abcdef0123456789abcdef"

func claim(seq int64) OwnerClaim {
	return OwnerClaim{ServerID: ownerID, Name: "web-1", Seq: seq}
}

// checkOwnerProtocol runs the clone check against one storage kind.
func checkOwnerProtocol(t *testing.T, st Storage) {
	t.Helper()
	ctx := context.Background()
	if _, err := ReadOwner(ctx, st, ownerID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fresh directory: %v", err)
	}
	for _, seq := range []int64{1, 2, 5} {
		got, err := ClaimOwner(ctx, st, claim(seq))
		if err != nil || got != seq {
			t.Fatalf("claim %d: %d, %v", seq, got, err)
		}
	}
	rec, err := ReadOwner(ctx, st, ownerID)
	if err != nil || rec.Seq != 5 || rec.Name != "web-1" || rec.ServerID != ownerID {
		t.Fatalf("record: %+v, %v", rec, err)
	}
	// A clone that copied this server at 2 claims 3, which the file is past.
	if _, err := ClaimOwner(ctx, st, claim(3)); !errors.Is(err, ErrOwnerConflict) {
		t.Fatalf("clone behind: %v", err)
	}
	// The same number twice is someone else too: the server never resends.
	if _, err := ClaimOwner(ctx, st, claim(5)); !errors.Is(err, ErrOwnerConflict) {
		t.Fatalf("number reused: %v", err)
	}
	adopt := claim(1)
	adopt.Adopt = true
	if got, err := ClaimOwner(ctx, st, adopt); err != nil || got != 6 {
		t.Fatalf("adopt: %d, %v", got, err)
	}
	if got, err := ClaimOwner(ctx, st, claim(7)); err != nil || got != 7 {
		t.Fatalf("after adopt: %d, %v", got, err)
	}
}

func TestOwnerFilesystem(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, ownerID, RepositoryDir)
	checkOwnerProtocol(t, Storage{Filesystem: &FilesystemStorage{Path: repo}})

	raw, err := os.ReadFile(filepath.Join(root, ownerID, OwnerFile))
	if err != nil {
		t.Fatal(err)
	}
	var rec OwnerRecord
	if json.Unmarshal(raw, &rec) != nil || rec.Format != ownerFormat {
		t.Fatalf("file: %s", raw)
	}
	left, _ := filepath.Glob(filepath.Join(root, ownerID, ".*tmp*"))
	if len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}

func TestOwnerRefusesAnotherServersFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ownerID)
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, OwnerFile), []byte(`{"format":1,"serverId":"ffffffffffffffffffffffffffffffff","seq":1}`), 0o600)
	st := Storage{Filesystem: &FilesystemStorage{Path: filepath.Join(dir, RepositoryDir)}}
	for _, adopt := range []bool{false, true} {
		c := claim(9)
		c.Adopt = adopt
		if _, err := ClaimOwner(context.Background(), st, c); !errors.Is(err, ErrOwnerConflict) {
			t.Fatalf("adopt=%v: %v", adopt, err)
		}
	}

	os.WriteFile(filepath.Join(dir, OwnerFile), []byte("not json"), 0o600)
	if _, err := ClaimOwner(context.Background(), st, claim(1)); !errors.Is(err, ErrOwnerConflict) {
		t.Fatalf("unreadable file: %v", err)
	}
	c := claim(1)
	c.Adopt = true
	if got, err := ClaimOwner(context.Background(), st, c); err != nil || got != 1 {
		t.Fatalf("adopt unreadable: %d, %v", got, err)
	}
}

func TestOwnerNeedsTheLayout(t *testing.T) {
	ctx := context.Background()
	for name, st := range map[string]Storage{
		"fs not repository": {Filesystem: &FilesystemStorage{Path: "/t/" + ownerID + "/repo"}},
		"fs other id":       {Filesystem: &FilesystemStorage{Path: "/t/ffffffffffffffffffffffffffffffff/" + RepositoryDir}},
		"s3 no slash":       {S3: &S3Storage{Endpoint: "e", Bucket: "b", Prefix: ownerID + "/" + RepositoryDir}},
		"s3 partial id":     {S3: &S3Storage{Endpoint: "e", Bucket: "b", Prefix: "x" + ownerID + "/" + RepositoryDir + "/"}},
		"webdav":            {WebDAV: &WebDAVStorage{URL: "https://dav.example/" + RepositoryDir}},
	} {
		if _, err := ClaimOwner(ctx, st, claim(1)); err == nil || !strings.Contains(err.Error(), "lives in") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestOwnerWebDAV(t *testing.T) {
	h := &webdav.Handler{FileSystem: webdav.NewMemFS(), LockSystem: webdav.NewMemLS()}
	srv := httptest.NewServer(h)
	defer srv.Close()
	if err := h.FileSystem.Mkdir(context.Background(), "/backups", 0o700); err != nil {
		t.Fatal(err)
	}
	checkOwnerProtocol(t, Storage{WebDAV: &WebDAVStorage{URL: srv.URL + "/backups/" + ownerID + "/" + RepositoryDir}})
	// The repository's collection exists for Kopia to write below.
	if fi, err := h.FileSystem.Stat(context.Background(), "/backups/"+ownerID+"/"+RepositoryDir); err != nil || !fi.IsDir() {
		t.Fatalf("repository collection: %v", err)
	}
	if _, err := h.FileSystem.Stat(context.Background(), "/backups/"+ownerID+"/"+OwnerFile); err != nil {
		t.Fatalf("owner file not at its readable name: %v", err)
	}
}

// fakeS3 is the slice of S3 the ownership file uses: get and put of
// objects in path style.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.TrimPrefix(r.URL.Path, "/")
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if r.URL.Query().Get("list-type") == "2" {
			f.list(w, key, r.URL.Query().Get("prefix"), r.URL.Query().Get("delimiter"))
			return
		}
		b, ok := f.objects[key]
		if !ok {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Message>no such key</Message></Error>`)
			return
		}
		w.Header().Set("ETag", `"x"`)
		w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Write(b)
	case http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
			b = unchunk(b)
		}
		f.objects[key] = b
		w.Header().Set("ETag", `"x"`)
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

// list answers ListObjectsV2 for one bucket, with common prefixes when a
// delimiter is given.
func (f *fakeS3) list(w http.ResponseWriter, bucket, prefix, delim string) {
	bucket = strings.TrimSuffix(bucket, "/")
	var keys, prefixes []string
	seen := map[string]bool{}
	for k := range f.objects {
		k, ok := strings.CutPrefix(k, bucket+"/")
		if !ok || !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := k[len(prefix):]
		if i := strings.Index(rest, delim); delim != "" && i >= 0 {
			p := prefix + rest[:i+1]
			if !seen[p] {
				seen[p] = true
				prefixes = append(prefixes, p)
			}
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sort.Strings(prefixes)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><Name>` + bucket + `</Name><Prefix>` + prefix + `</Prefix><KeyCount>` + strconv.Itoa(len(keys)+len(prefixes)) + `</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>`)
	for _, k := range keys {
		b.WriteString(`<Contents><Key>` + k + `</Key><Size>` + strconv.Itoa(len(f.objects[bucket+"/"+k])) + `</Size><LastModified>2006-01-02T15:04:05.000Z</LastModified><ETag>"x"</ETag></Contents>`)
	}
	for _, p := range prefixes {
		b.WriteString(`<CommonPrefixes><Prefix>` + p + `</Prefix></CommonPrefixes>`)
	}
	b.WriteString(`</ListBucketResult>`)
	w.Header().Set("Content-Type", "application/xml")
	io.WriteString(w, b.String())
}

// unchunk decodes an aws-chunked body: "<hex size>[;ext]\r\n<data>\r\n"
// until a zero size.
func unchunk(b []byte) []byte {
	var out []byte
	for {
		line, rest, ok := strings.Cut(string(b), "\r\n")
		if !ok {
			return out
		}
		size, err := strconv.ParseInt(strings.SplitN(line, ";", 2)[0], 16, 64)
		if err != nil || size == 0 || int64(len(rest)) < size {
			return out
		}
		out = append(out, rest[:size]...)
		b = []byte(strings.TrimPrefix(rest[size:], "\r\n"))
	}
}

func TestOwnerS3(t *testing.T) {
	fake := &fakeS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	checkOwnerProtocol(t, Storage{S3: &S3Storage{
		Endpoint:        strings.TrimPrefix(srv.URL, "http://"),
		Region:          "us-east-1",
		Bucket:          "bucket",
		Prefix:          "odac/" + ownerID + "/" + RepositoryDir + "/",
		AccessKeyID:     "id",
		SecretAccessKey: "secret",
		DisableTLS:      true,
	}})
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if _, ok := fake.objects["bucket/odac/"+ownerID+"/"+OwnerFile]; !ok {
		t.Fatalf("objects: %v", fake.objects)
	}
}

func TestSameFileSystem(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	os.Mkdir(a, 0o700)
	os.Mkdir(b, 0o700)
	if same, err := SameFileSystem(a, b); err != nil || !same {
		t.Fatalf("one temp dir: %v, %v", same, err)
	}
	if _, err := SameFileSystem(a, filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing path reported a file system")
	}
}

const otherID = "fedcba9876543210fedcba9876543210"

// checkServerList writes two servers' ownership files, a directory that
// is no server's and an unreadable file, and lists them from self.
func checkServerList(t *testing.T, self, other Storage, write func(name string, data []byte)) {
	t.Helper()
	ctx := context.Background()
	if _, err := ClaimOwner(ctx, self, OwnerClaim{ServerID: ownerID, Name: "web-1", Seq: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := ClaimOwner(ctx, other, OwnerClaim{ServerID: otherID, Name: "old", Seq: 9}); err != nil {
		t.Fatal(err)
	}
	write("notes/readme.txt", []byte("x"))
	write("ffffffffffffffffffffffffffffffff/"+OwnerFile, []byte("not json"))
	got, err := ListServers(ctx, self, ownerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("servers: %+v", got)
	}
	if got[0].ID != ownerID || got[0].Name != "web-1" || got[0].Seq != 4 || !got[0].Repository {
		t.Fatalf("self: %+v", got[0])
	}
	if got[1].ID != otherID || got[1].Name != "old" || got[1].Seq != 9 || !got[1].Repository {
		t.Fatalf("other: %+v", got[1])
	}
	if got[2].ID != "ffffffffffffffffffffffffffffffff" || got[2].Error == "" || got[2].Repository {
		t.Fatalf("unreadable: %+v", got[2])
	}
}

func TestListServersFilesystem(t *testing.T) {
	root := t.TempDir()
	fsAt := func(id string) Storage {
		return Storage{Filesystem: &FilesystemStorage{Path: filepath.Join(root, id, RepositoryDir)}}
	}
	checkServerList(t, fsAt(ownerID), fsAt(otherID), func(name string, data []byte) {
		p := filepath.Join(root, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, data, 0o600)
	})
	if _, err := ListServers(context.Background(), Storage{Filesystem: &FilesystemStorage{Path: filepath.Join(root, "x", RepositoryDir)}}, ownerID); err == nil {
		t.Fatal("a storage outside the layout was listed")
	}
}

func TestListServersWebDAV(t *testing.T) {
	h := &webdav.Handler{FileSystem: webdav.NewMemFS(), LockSystem: webdav.NewMemLS()}
	srv := httptest.NewServer(h)
	defer srv.Close()
	ctx := context.Background()
	h.FileSystem.Mkdir(ctx, "/b", 0o700)
	davAt := func(id string) Storage {
		return Storage{WebDAV: &WebDAVStorage{URL: srv.URL + "/b/" + id + "/" + RepositoryDir}}
	}
	checkServerList(t, davAt(ownerID), davAt(otherID), func(name string, data []byte) {
		dir, _ := path.Split("/b/" + name)
		for p := "/b"; ; {
			rest := strings.TrimPrefix(dir, p+"/")
			first, _, _ := strings.Cut(rest, "/")
			if first == "" {
				break
			}
			p += "/" + first
			h.FileSystem.Mkdir(ctx, p, 0o700)
		}
		f, err := h.FileSystem.OpenFile(ctx, "/b/"+name, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		f.Write(data)
		f.Close()
	})
}

func TestListServersS3(t *testing.T) {
	fake := &fakeS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	s3At := func(id string) Storage {
		return Storage{S3: &S3Storage{
			Endpoint: strings.TrimPrefix(srv.URL, "http://"), Region: "us-east-1", Bucket: "bucket",
			Prefix: "odac/" + id + "/" + RepositoryDir + "/", AccessKeyID: "id", SecretAccessKey: "secret", DisableTLS: true,
		}}
	}
	checkServerList(t, s3At(ownerID), s3At(otherID), func(name string, data []byte) {
		fake.mu.Lock()
		fake.objects["bucket/odac/"+name] = data
		fake.mu.Unlock()
	})
}
