// Package notify sends operator alerts by e-mail.
package notify

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"
)

// Notifier delivers a message to the operator.
type Notifier interface {
	Send(ctx context.Context, subject, body string) error
}

// LogNotifier only logs messages; used when SMTP is not configured.
type LogNotifier struct{ Log *slog.Logger }

func (n LogNotifier) Send(_ context.Context, subject, body string) error {
	n.Log.Info("notification (SMTP not configured)", "subject", subject, "body", body)
	return nil
}

// SMTPConfig is read from the environment (see FromEnv).
type SMTPConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	From     string
	To       []string
}

// FromEnv reads SMTP_HOST, SMTP_PORT, SMTP_USER, SMTP_PASS, SMTP_FROM and
// ALERT_TO (comma separated). ok is false when SMTP_HOST or ALERT_TO is unset.
func FromEnv() (cfg SMTPConfig, ok bool, err error) {
	cfg.Host = os.Getenv("SMTP_HOST")
	cfg.User = os.Getenv("SMTP_USER")
	cfg.Password = os.Getenv("SMTP_PASS")
	cfg.From = os.Getenv("SMTP_FROM")
	for _, to := range strings.Split(os.Getenv("ALERT_TO"), ",") {
		if to = strings.TrimSpace(to); to != "" {
			cfg.To = append(cfg.To, to)
		}
	}
	if cfg.Host == "" || len(cfg.To) == 0 {
		return cfg, false, nil
	}
	cfg.Port = 587
	if p := os.Getenv("SMTP_PORT"); p != "" {
		if cfg.Port, err = strconv.Atoi(p); err != nil {
			return cfg, false, fmt.Errorf("notify: invalid SMTP_PORT %q", p)
		}
	}
	if cfg.From == "" {
		cfg.From = cfg.User
	}
	if cfg.From == "" {
		return cfg, false, errors.New("notify: SMTP_FROM or SMTP_USER must be set")
	}
	return cfg, true, nil
}

// SMTP sends mail with STARTTLS (port 587 and others) or implicit TLS (465).
type SMTP struct {
	Cfg     SMTPConfig
	Timeout time.Duration
}

func (s SMTP) Send(ctx context.Context, subject, body string) error {
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	addr := net.JoinHostPort(s.Cfg.Host, strconv.Itoa(s.Cfg.Port))
	dialer := &net.Dialer{Timeout: timeout}
	var conn net.Conn
	var err error
	tlsCfg := &tls.Config{ServerName: s.Cfg.Host, MinVersion: tls.VersionTLS12}
	if s.Cfg.Port == 465 {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("notify: connect %s: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	c, err := smtp.NewClient(conn, s.Cfg.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("notify: smtp handshake: %w", err)
	}
	defer c.Close()
	if s.Cfg.Port != 465 {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsCfg); err != nil {
				return fmt.Errorf("notify: starttls: %w", err)
			}
		}
	}
	if s.Cfg.User != "" {
		if err := c.Auth(smtp.PlainAuth("", s.Cfg.User, s.Cfg.Password, s.Cfg.Host)); err != nil {
			return fmt.Errorf("notify: auth: %w", err)
		}
	}
	if err := c.Mail(s.Cfg.From); err != nil {
		return err
	}
	for _, to := range s.Cfg.To {
		if err := c.Rcpt(to); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(buildMessage(s.Cfg.From, s.Cfg.To, subject, body, time.Now())); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func buildMessage(from string, to []string, subject, body string, now time.Time) []byte {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + strings.Join(to, ", ") + "\r\n")
	b.WriteString("Subject: " + mimeHeader(subject) + "\r\n")
	b.WriteString("Date: " + now.Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n"))
	b.WriteString("\r\n")
	return []byte(b.String())
}

// mimeHeader encodes non-ASCII subjects (RFC 2047).
func mimeHeader(s string) string { return mime.BEncoding.Encode("UTF-8", s) }
