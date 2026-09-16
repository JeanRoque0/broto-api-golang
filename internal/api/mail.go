package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"time"
)

// sendSMTP uses the actual SMTP protocol, requires STARTTLS and validates the
// certificate before authenticating. roots=nil means the system trust store.
func sendSMTP(ctx context.Context, c Config, to, link string, roots *x509.CertPool) error {
	return sendSMTPText(ctx, c, to, "Broto - confirme seu acesso", "Use este link em ate uma hora:\r\n"+link+"\r\n", roots)
}

// Subject and text are internal templates, never supplied by an HTTP client.
func sendSMTPText(ctx context.Context, c Config, to, subject, text string, roots *x509.CertPool) error {
	if c.SMTPHost == "" {
		return errors.New("SMTP is not configured")
	}
	from, e := mail.ParseAddress(c.SMTPFrom)
	if e != nil {
		return e
	}
	recipient, e := mail.ParseAddress(to)
	if e != nil || recipient.Address != to {
		return errors.New("invalid email recipient")
	}
	conn, e := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(c.SMTPHost, c.SMTPPort))
	if e != nil {
		return e
	}
	defer conn.Close()
	// Deadlines bound stalled servers; cancellation closes an established socket.
	deadline := time.Now().Add(20 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if e = conn.SetDeadline(deadline); e != nil {
		return e
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	client, e := smtp.NewClient(conn, c.SMTPHost)
	if e != nil {
		return e
	}
	defer client.Close()
	if e = client.StartTLS(&tls.Config{ServerName: c.SMTPHost, RootCAs: roots, MinVersion: tls.VersionTLS12}); e != nil {
		return e
	}
	if c.SMTPUser != "" {
		if e = client.Auth(smtp.PlainAuth("", c.SMTPUser, c.SMTPPassword, c.SMTPHost)); e != nil {
			return e
		}
	}
	if e = client.Mail(from.Address); e != nil {
		return e
	}
	if e = client.Rcpt(to); e != nil {
		return e
	}
	w, e := client.Data()
	if e != nil {
		return e
	}
	_, e = fmt.Fprintf(w, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s", from.String(), to, subject, text)
	if e != nil {
		return e
	}
	if e = w.Close(); e != nil {
		return e
	}
	return client.Quit()
}
