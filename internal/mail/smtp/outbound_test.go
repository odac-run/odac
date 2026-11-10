package smtp

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	gosmtp "github.com/emersion/go-smtp"

	"odac/internal/mail/config"
)

// fakeMX is a remote mail server for outbound tests, built on go-smtp's server.
type fakeMX struct {
	addr string

	mu       sync.Mutex
	sessions []*mxSession
	// rcpt, when set, answers each RCPT TO.
	rcpt func(to string) error
}

type mxSession struct {
	mx   *fakeMX
	conn *gosmtp.Conn
	tls  bool
	from string
	to   []string
	body string
}

func (m *fakeMX) NewSession(c *gosmtp.Conn) (gosmtp.Session, error) {
	s := &mxSession{mx: m, conn: c}
	m.mu.Lock()
	m.sessions = append(m.sessions, s)
	m.mu.Unlock()
	return s, nil
}

func (s *mxSession) Reset()        {}
func (s *mxSession) Logout() error { return nil }
func (s *mxSession) Mail(from string, _ *gosmtp.MailOptions) error {
	s.mx.mu.Lock()
	defer s.mx.mu.Unlock()
	s.from = from
	return nil
}
func (s *mxSession) Rcpt(to string, _ *gosmtp.RcptOptions) error {
	if s.mx.rcpt != nil {
		if err := s.mx.rcpt(to); err != nil {
			return err
		}
	}
	s.mx.mu.Lock()
	defer s.mx.mu.Unlock()
	s.to = append(s.to, to)
	return nil
}
func (s *mxSession) Data(r io.Reader) error {
	_, isTLS := s.conn.TLSConnectionState()
	b, err := io.ReadAll(r)
	s.mx.mu.Lock()
	defer s.mx.mu.Unlock()
	s.tls = isTLS
	s.body = string(b)
	return err
}

// delivered returns the sessions that completed a message.
func (m *fakeMX) delivered() []*mxSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*mxSession
	for _, s := range m.sessions {
		if s.body != "" {
			out = append(out, s)
		}
	}
	return out
}

func (m *fakeMX) sessionCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// startMX runs a fake MX on loopback, offering STARTTLS when withTLS is set.
// go-smtp sends a multi-line EHLO reply, which is part of what is under test.
func startMX(t *testing.T, withTLS bool) *fakeMX {
	t.Helper()
	mx := &fakeMX{}
	srv := gosmtp.NewServer(mx)
	srv.Domain = "mx.test"
	srv.AllowInsecureAuth = true
	if withTLS {
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mx.addr = l.Addr().String()
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return mx
}

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "mx.test"},
		DNSNames:     []string{"mx.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// testClient routes MX host names to the given listeners instead of port 25.
func testClient(mx map[string]string, records []*net.MX) *Client {
	c := NewClient(func() config.Config { return config.Config{} }, nil)
	c.lookupMX = func(context.Context, string) ([]*net.MX, error) { return records, nil }
	c.lookupIP = func(context.Context, string) ([]net.IPAddr, error) { return nil, errors.New("no AAAA") }
	c.dial = func(ctx context.Context, d *net.Dialer, _, addr string) (net.Conn, error) {
		host, _, _ := net.SplitHostPort(addr)
		target, ok := mx[host]
		if !ok {
			return nil, errors.New("connection refused")
		}
		return d.DialContext(ctx, "tcp", target)
	}
	return c
}

const testBody = "From: ali@emre.red\r\nSubject: hi\r\n\r\n<style>\n.foo { color: red; }\n</style>\r\n"

// The message reaches the server over TLS, intact: dot-stuffed lines come out
// as they went in and bare LF arrives as CRLF.
func TestSendDeliversOverStartTLS(t *testing.T) {
	mx := startMX(t, true)
	c := testClient(map[string]string{"mx1.test": mx.addr}, []*net.MX{{Host: "mx1.test.", Pref: 10}})

	if err := c.Send(context.Background(), "ali@emre.red", "bob@remote.test", []byte(testBody)); err != nil {
		t.Fatalf("Send = %v", err)
	}
	got := mx.delivered()
	if len(got) != 1 {
		t.Fatalf("delivered %d messages, want 1", len(got))
	}
	s := got[0]
	if s.from != "ali@emre.red" || len(s.to) != 1 || s.to[0] != "bob@remote.test" {
		t.Errorf("envelope from=%q to=%v", s.from, s.to)
	}
	want := strings.ReplaceAll(strings.ReplaceAll(testBody, "\r\n", "\n"), "\n", "\r\n")
	if s.body != want {
		t.Errorf("body\n got: %q\nwant: %q", s.body, want)
	}
}

// The pool handed one socket to every concurrent sender for the same MX, so
// one sender's RCPT could land in another's transaction. Each delivery now
// owns its session, and each server session sees exactly one recipient.
func TestConcurrentSendsDoNotShareASession(t *testing.T) {
	mx := startMX(t, true)
	c := testClient(map[string]string{"mx1.test": mx.addr}, []*net.MX{{Host: "mx1.test", Pref: 10}})

	rcpts := []string{"a@remote.test", "b@remote.test", "c@remote.test", "d@remote.test"}
	var wg sync.WaitGroup
	errs := make(chan error, len(rcpts))
	for _, to := range rcpts {
		wg.Add(1)
		go func(to string) {
			defer wg.Done()
			errs <- c.Send(context.Background(), "ali@emre.red", to, []byte(testBody))
		}(to)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Send = %v", err)
		}
	}

	// A second round reuses nothing either: every session is fresh and TLS.
	if err := c.Send(context.Background(), "ali@emre.red", "e@remote.test", []byte(testBody)); err != nil {
		t.Fatalf("second Send = %v", err)
	}

	got := mx.delivered()
	if len(got) != len(rcpts)+1 {
		t.Fatalf("delivered %d messages, want %d", len(got), len(rcpts)+1)
	}
	for _, s := range got {
		if len(s.to) != 1 {
			t.Errorf("one session carried recipients %v", s.to)
		}
		if !s.tls {
			t.Errorf("session for %v ran without TLS", s.to)
		}
	}
}

// A 5xx at RCPT is the domain's final answer: no other MX is asked, and the
// error says no retry can help.
func TestPermanentRefusalStopsAtFirstMX(t *testing.T) {
	first := startMX(t, false)
	first.rcpt = func(string) error {
		return &gosmtp.SMTPError{Code: 550, EnhancedCode: gosmtp.EnhancedCode{5, 1, 1}, Message: "no such user"}
	}
	second := startMX(t, false)
	c := testClient(map[string]string{"mx1.test": first.addr, "mx2.test": second.addr},
		[]*net.MX{{Host: "mx1.test", Pref: 10}, {Host: "mx2.test", Pref: 20}})

	err := c.Send(context.Background(), "ali@emre.red", "nobody@remote.test", []byte(testBody))
	if err == nil || !IsPermanent(err) {
		t.Fatalf("Send = %v, want a permanent error", err)
	}
	if got := failureStatus(err); got != "5.1.1" {
		t.Errorf("status = %q, want 5.1.1", got)
	}
	if n := second.sessionCount(); n != 0 {
		t.Errorf("second MX was contacted %d times after a permanent refusal", n)
	}
}

// A temporary refusal or an unreachable host moves on to the next MX. Only
// the first record was ever used before.
func TestTemporaryFailureTriesNextMX(t *testing.T) {
	greylist := startMX(t, false)
	greylist.rcpt = func(string) error {
		return &gosmtp.SMTPError{Code: 451, EnhancedCode: gosmtp.EnhancedCode{4, 7, 1}, Message: "greylisted"}
	}
	backup := startMX(t, false)
	c := testClient(map[string]string{"mx2.test": greylist.addr, "mx3.test": backup.addr},
		[]*net.MX{{Host: "down.test", Pref: 5}, {Host: "mx2.test", Pref: 10}, {Host: "mx3.test", Pref: 20}})

	if err := c.Send(context.Background(), "ali@emre.red", "bob@remote.test", []byte(testBody)); err != nil {
		t.Fatalf("Send = %v, want delivery through the last MX", err)
	}
	if n := len(backup.delivered()); n != 1 {
		t.Errorf("backup MX received %d messages, want 1", n)
	}
}

// When every MX defers, the error is temporary, so the queue keeps the message.
func TestAllMXDeferringIsTemporary(t *testing.T) {
	greylist := startMX(t, false)
	greylist.rcpt = func(string) error {
		return &gosmtp.SMTPError{Code: 451, EnhancedCode: gosmtp.EnhancedCode{4, 7, 1}, Message: "greylisted"}
	}
	c := testClient(map[string]string{"mx1.test": greylist.addr}, []*net.MX{{Host: "mx1.test", Pref: 10}})

	err := c.Send(context.Background(), "ali@emre.red", "bob@remote.test", []byte(testBody))
	if err == nil || IsPermanent(err) {
		t.Fatalf("Send = %v, want a temporary error", err)
	}
}

// A multi-line reply split across TCP segments used to be read with one Read,
// leaving the rest to be taken as the answer to the next command. This server
// writes its EHLO reply a line at a time and refuses any command that arrives
// before the reply was finished.
func TestSplitMultilineReplyStaysInSync(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	result := make(chan []string, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			result <- nil
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		var cmds []string
		say := func(lines ...string) {
			for _, line := range lines {
				conn.Write([]byte(line + "\r\n"))
				time.Sleep(20 * time.Millisecond)
			}
		}
		say("220 slow.test ESMTP")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				break
			}
			cmd := strings.TrimSpace(line)
			cmds = append(cmds, cmd)
			switch {
			case strings.HasPrefix(cmd, "EHLO"):
				say("250-slow.test", "250-PIPELINING", "250 SIZE 1000000")
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				say("250 ok")
			case cmd == "DATA":
				say("354 go ahead")
				for {
					l, err := r.ReadString('\n')
					if err != nil || l == ".\r\n" {
						break
					}
				}
				say("250 queued")
			case cmd == "QUIT":
				say("221 bye")
				result <- cmds
				return
			default:
				say("500 unexpected")
			}
		}
		result <- cmds
	}()

	c := testClient(map[string]string{"slow.test": l.Addr().String()}, []*net.MX{{Host: "slow.test", Pref: 10}})
	if err := c.Send(context.Background(), "ali@emre.red", "bob@remote.test", []byte(testBody)); err != nil {
		t.Fatalf("Send = %v", err)
	}
	cmds := <-result
	want := []string{"EHLO", "MAIL FROM:<ali@emre.red>", "RCPT TO:<bob@remote.test>", "DATA", "QUIT"}
	if len(cmds) != len(want) {
		t.Fatalf("commands = %q, want %q", cmds, want)
	}
	for i, w := range want {
		if !strings.HasPrefix(cmds[i], w) {
			t.Errorf("command %d = %q, want %q", i, cmds[i], w)
		}
	}
}

func notFound(name string) error {
	return &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func TestResolveMX(t *testing.T) {
	c := NewClient(func() config.Config { return config.Config{} }, nil)

	// No MX record: the domain's own address is the implicit MX.
	c.lookupMX = func(_ context.Context, d string) ([]*net.MX, error) { return nil, notFound(d) }
	c.lookupHost = func(context.Context, string) ([]string, error) { return []string{"192.0.2.1"}, nil }
	if hosts, err := c.resolveMX(context.Background(), "implicit.test"); err != nil || len(hosts) != 1 || hosts[0] != "implicit.test" {
		t.Errorf("implicit MX = %v, %v", hosts, err)
	}

	// Neither MX nor address: the domain does not exist.
	c.lookupHost = func(_ context.Context, h string) ([]string, error) { return nil, notFound(h) }
	if _, err := c.resolveMX(context.Background(), "gone.test"); !IsPermanent(err) {
		t.Errorf("missing domain = %v, want permanent", err)
	}

	// RFC 7505 null MX.
	c.lookupMX = func(context.Context, string) ([]*net.MX, error) { return []*net.MX{{Host: ".", Pref: 0}}, nil }
	if _, err := c.resolveMX(context.Background(), "nullmx.test"); !IsPermanent(err) {
		t.Errorf("null MX = %v, want permanent", err)
	}

	// A resolver failure is not an answer about the domain.
	c.lookupMX = func(context.Context, string) ([]*net.MX, error) {
		return nil, &net.DNSError{Err: "server misbehaving", IsTemporary: true}
	}
	if _, err := c.resolveMX(context.Background(), "flaky.test"); err == nil || IsPermanent(err) {
		t.Errorf("resolver failure = %v, want temporary", err)
	}

	// All records are kept, in preference order.
	c.lookupMX = func(context.Context, string) ([]*net.MX, error) {
		return []*net.MX{{Host: "a.test.", Pref: 10}, {Host: "b.test.", Pref: 20}}, nil
	}
	if hosts, _ := c.resolveMX(context.Background(), "multi.test"); len(hosts) != 2 || hosts[0] != "a.test" || hosts[1] != "b.test" {
		t.Errorf("hosts = %v, want [a.test b.test]", hosts)
	}
}

func TestReplyClassification(t *testing.T) {
	cases := []struct {
		err       *replyError
		permanent bool
	}{
		{&replyError{code: 550, stage: stageRcpt}, true},
		{&replyError{code: 552, stage: stageRcpt}, false}, // RFC 5321 4.5.3.1.10
		{&replyError{code: 552, stage: stageData}, true},
		{&replyError{code: 554, stage: stageGreeting}, false},
		{&replyError{code: 550, stage: stageMail}, true},
		{&replyError{code: 421, stage: stageMail}, false},
		{&replyError{code: 451, stage: stageData}, false},
	}
	for _, tc := range cases {
		if got := IsPermanent(tc.err); got != tc.permanent {
			t.Errorf("%d at %s: permanent = %v, want %v", tc.err.code, tc.err.stage, got, tc.permanent)
		}
	}
}
