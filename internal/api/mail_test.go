package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"
)

type localSMTP struct {
	listener     net.Listener
	certificate  tls.Certificate
	roots        *x509.CertPool
	failure      string
	mu           sync.Mutex
	connections  map[net.Conn]bool
	closed       bool
	wg           sync.WaitGroup
	messages     []string
	auth         int
	insecureAuth bool
	started      chan struct{}
}

func newLocalSMTP(t *testing.T, failure string) *localSMTP {
	t.Helper()
	certServer := httptest.NewTLSServer(nil)
	cert := certServer.TLS.Certificates[0]
	roots := x509.NewCertPool()
	roots.AddCert(certServer.Certificate())
	certServer.Close()
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	m := &localSMTP{listener: listener, certificate: cert, roots: roots, failure: failure, connections: map[net.Conn]bool{}, started: make(chan struct{}, 1)}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for {
			conn, e := listener.Accept()
			if e != nil {
				return
			}
			m.mu.Lock()
			if m.closed {
				m.mu.Unlock()
				conn.Close()
				continue
			}
			m.connections[conn] = true
			m.wg.Add(1)
			m.mu.Unlock()
			go m.serve(conn)
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		m.mu.Lock()
		m.closed = true
		for c := range m.connections {
			c.Close()
		}
		m.mu.Unlock()
		m.wg.Wait()
	})
	return m
}
func (m *localSMTP) config() Config {
	host, port, _ := net.SplitHostPort(m.listener.Addr().String())
	return Config{SMTPHost: host, SMTPPort: port, SMTPFrom: "Broto <noreply@example.invalid>", SMTPUser: "local-user", SMTPPassword: "local-password", SiteURL: "https://app.example.invalid"}
}
func (m *localSMTP) snapshot() ([]string, int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.messages...), m.auth, m.insecureAuth
}
func (m *localSMTP) serve(conn net.Conn) {
	defer m.wg.Done()
	defer func() { conn.Close(); m.mu.Lock(); delete(m.connections, conn); m.mu.Unlock() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	select {
	case m.started <- struct{}{}:
	default:
	}
	p := textproto.NewConn(conn)
	secure := false
	if m.failure == "greeting" {
		_ = p.PrintfLine("421 unavailable")
		return
	}
	if m.failure == "stall" {
		var b [1]byte
		_, _ = conn.Read(b[:])
		return
	}
	if p.PrintfLine("220 local.test ESMTP") != nil {
		return
	}
	for {
		line, e := p.ReadLine()
		if e != nil {
			return
		}
		command := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		if m.failure == command {
			_ = p.PrintfLine("550 simulated rejection")
			continue
		}
		switch command {
		case "EHLO", "HELO":
			if p.PrintfLine("250-local.test") != nil {
				return
			}
			if !secure {
				if p.PrintfLine("250-STARTTLS") != nil {
					return
				}
			}
			if p.PrintfLine("250 AUTH PLAIN") != nil {
				return
			}
		case "STARTTLS":
			if p.PrintfLine("220 start TLS") != nil {
				return
			}
			c := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{m.certificate}, MinVersion: tls.VersionTLS12})
			if c.Handshake() != nil {
				return
			}
			p = textproto.NewConn(c)
			secure = true
		case "AUTH":
			m.mu.Lock()
			m.auth++
			if !secure {
				m.insecureAuth = true
			}
			m.mu.Unlock()
			parts := strings.Fields(line)
			var decoded []byte
			if len(parts) == 3 {
				decoded, _ = base64.StdEncoding.DecodeString(parts[2])
			}
			if string(decoded) != "\x00local-user\x00local-password" {
				_ = p.PrintfLine("535 invalid credentials")
			} else {
				_ = p.PrintfLine("235 authenticated")
			}
		case "MAIL", "RCPT":
			if p.PrintfLine("250 accepted") != nil {
				return
			}
		case "DATA":
			if p.PrintfLine("354 send content") != nil {
				return
			}
			b, e := io.ReadAll(p.DotReader())
			if e != nil {
				return
			}
			if m.failure == "after-data" {
				_ = p.PrintfLine("451 temporary delivery failure")
				continue
			}
			m.mu.Lock()
			m.messages = append(m.messages, string(b))
			m.mu.Unlock()
			if p.PrintfLine("250 queued") != nil {
				return
			}
		case "QUIT":
			_ = p.PrintfLine("221 bye")
			return
		default:
			_ = p.PrintfLine("500 unsupported")
		}
	}
}
func TestSMTPTLSAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name, failure                               string
		untrusted, wrongPassword, noAuth, wantError bool
	}{
		{name: "authenticated TLS"},
		{name: "TLS without SMTP auth", noAuth: true},
		{name: "untrusted certificate", untrusted: true, wantError: true},
		{name: "bad credentials", wrongPassword: true, wantError: true},
		{name: "bad greeting", failure: "greeting", wantError: true},
		{name: "TLS rejected", failure: "STARTTLS", wantError: true},
		{name: "sender rejected", failure: "MAIL", wantError: true},
		{name: "recipient rejected", failure: "RCPT", wantError: true},
		{name: "DATA rejected", failure: "DATA", wantError: true},
		{name: "message rejected after DATA", failure: "after-data", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newLocalSMTP(t, tc.failure)
			c := m.config()
			roots := m.roots
			if tc.untrusted {
				roots = x509.NewCertPool()
			}
			if tc.wrongPassword {
				c.SMTPPassword = "wrong"
			}
			if tc.noAuth {
				c.SMTPUser = ""
				c.SMTPPassword = ""
			}
			e := sendSMTP(context.Background(), c, "recipient@example.invalid", "https://app.example.invalid/confirmado?token_hash=example&type=email", roots)
			if (e != nil) != tc.wantError {
				t.Fatalf("error=%v wantError=%v", e, tc.wantError)
			}
			messages, auth, insecure := m.snapshot()
			if insecure {
				t.Fatal("credentials sent before TLS")
			}
			if !tc.wantError {
				if len(messages) != 1 || !strings.Contains(messages[0], "token_hash=example&type=email") {
					t.Fatal("message missing confirmation link")
				}
				if tc.noAuth && auth != 0 {
					t.Fatal("unexpected authentication")
				}
			}
			if tc.wantError && len(messages) != 0 {
				t.Fatal("rejected message was accepted")
			}
			if (tc.untrusted || tc.failure == "STARTTLS") && auth != 0 {
				t.Fatal("authenticated before validating TLS")
			}
		})
	}
}
func TestSMTPCancellationAndValidation(t *testing.T) {
	t.Run("cancel established stalled connection", func(t *testing.T) {
		m := newLocalSMTP(t, "stall")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- sendSMTP(ctx, m.config(), "a@example.invalid", "https://app.example.invalid", m.roots) }()
		select {
		case <-m.started:
		case <-time.After(2 * time.Second):
			t.Fatal("SMTP did not connect")
		}
		cancel()
		select {
		case e := <-done:
			if e == nil {
				t.Fatal("cancellation succeeded")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("cancellation did not close SMTP connection")
		}
	})
	t.Run("deadline", func(t *testing.T) {
		m := newLocalSMTP(t, "stall")
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if e := sendSMTP(ctx, m.config(), "a@example.invalid", "https://app.example.invalid", m.roots); e == nil {
			t.Fatal("deadline ignored")
		}
	})
	for _, tc := range []struct{ name, host, from, to string }{{"not configured", "", "a@example.invalid", "b@example.invalid"}, {"bad sender", "127.0.0.1", "invalid", "b@example.invalid"}, {"header injection", "127.0.0.1", "a@example.invalid", "b@example.invalid\r\nBcc: c@example.invalid"}} {
		t.Run(tc.name, func(t *testing.T) {
			e := sendSMTP(context.Background(), Config{SMTPHost: tc.host, SMTPFrom: tc.from}, tc.to, "https://app.example.invalid", nil)
			if e == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	t.Run("cancel before dial", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		e := sendSMTP(ctx, Config{SMTPHost: "127.0.0.1", SMTPPort: "1", SMTPFrom: "a@example.invalid"}, "b@example.invalid", "https://app.example.invalid", nil)
		if !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
	})
}
