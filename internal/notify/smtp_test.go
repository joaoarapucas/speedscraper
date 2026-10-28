package notify

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
)

// fakeSMTP accepts one session and records the commands and message data.
func fakeSMTP(t *testing.T) (addr string, got chan []string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = make(chan []string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var lines []string
		r := bufio.NewReader(conn)
		w := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
		w("220 fake ESMTP")
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				got <- lines
				return
			}
			line = strings.TrimRight(line, "\r\n")
			lines = append(lines, line)
			switch {
			case inData && line == ".":
				inData = false
				w("250 queued")
			case inData:
			case strings.HasPrefix(line, "EHLO"):
				w("250-fake")
				w("250 AUTH PLAIN")
			case strings.HasPrefix(line, "AUTH PLAIN"):
				w("235 ok")
			case strings.HasPrefix(line, "DATA"):
				inData = true
				w("354 go ahead")
			case strings.HasPrefix(line, "QUIT"):
				w("221 bye")
				got <- lines
				return
			default:
				w("250 ok")
			}
		}
	}()
	return ln.Addr().String(), got
}

func TestSMTPSend(t *testing.T) {
	addr, got := fakeSMTP(t)
	host, port, _ := net.SplitHostPort(addr)
	var p int
	for _, c := range port {
		p = p*10 + int(c-'0')
	}
	s := SMTP{Cfg: SMTPConfig{Host: host, Port: p, User: "me", Password: "pw", From: "me@x", To: []string{"you@x"}}}
	if err := s.Send(context.Background(), "hello", "body line"); err != nil {
		t.Fatal(err)
	}
	session := strings.Join(<-got, "\n")
	for _, want := range []string{"AUTH PLAIN", "MAIL FROM:<me@x>", "RCPT TO:<you@x>", "Subject: hello", "body line"} {
		if !strings.Contains(session, want) {
			t.Errorf("session lacks %q:\n%s", want, session)
		}
	}
}
