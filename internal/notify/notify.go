// Package notify sends email (SMTP) and Home Assistant webhook messages.
package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"ytguard/internal/store"
)

// Email sends an HTML + plain text email using the SMTP settings.
func Email(st store.Settings, subject, htmlBody, textBody string) error {
	if st.SMTPHost == "" || st.SMTPFrom == "" || st.SMTPTo == "" {
		return errors.New("SMTP host, from and to must be set")
	}
	var to []string
	for _, a := range strings.Split(st.SMTPTo, ",") {
		if a = strings.TrimSpace(a); a != "" {
			to = append(to, a)
		}
	}
	msg := buildMessage(st.SMTPFrom, to, subject, htmlBody, textBody)
	port := st.SMTPPort
	if port == 0 {
		port = 587
	}
	addr := net.JoinHostPort(st.SMTPHost, strconv.Itoa(port))
	tlsCfg := &tls.Config{ServerName: st.SMTPHost, MinVersion: tls.VersionTLS12}

	var c *smtp.Client
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	if st.SMTPTLS == "tls" {
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
		if err != nil {
			return err
		}
		if c, err = smtp.NewClient(conn, st.SMTPHost); err != nil {
			return err
		}
	} else {
		conn, err := dialer.Dial("tcp", addr)
		if err != nil {
			return err
		}
		if c, err = smtp.NewClient(conn, st.SMTPHost); err != nil {
			return err
		}
		if st.SMTPTLS != "none" {
			if err := c.StartTLS(tlsCfg); err != nil {
				c.Close()
				return fmt.Errorf("STARTTLS: %w", err)
			}
		}
	}
	defer c.Close()
	if st.SMTPUser != "" {
		if err := c.Auth(smtp.PlainAuth("", st.SMTPUser, st.SMTPPass, st.SMTPHost)); err != nil {
			return fmt.Errorf("SMTP login: %w", err)
		}
	}
	if err := c.Mail(addrOnly(st.SMTPFrom)); err != nil {
		return err
	}
	for _, a := range to {
		if err := c.Rcpt(addrOnly(a)); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func addrOnly(a string) string {
	if i := strings.LastIndex(a, "<"); i >= 0 {
		return strings.TrimSuffix(strings.TrimSpace(a[i+1:]), ">")
	}
	return strings.TrimSpace(a)
}

func buildMessage(from string, to []string, subject, htmlBody, textBody string) []byte {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	boundary := "ytg-" + hex.EncodeToString(b)
	var buf bytes.Buffer
	host := "ytguard.local"
	if i := strings.LastIndex(addrOnly(from), "@"); i >= 0 {
		host = addrOnly(from)[i+1:]
	}
	fmt.Fprintf(&buf, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@%s>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=%q\r\n\r\n",
		from, strings.Join(to, ", "), mime.QEncoding.Encode("utf-8", subject), time.Now().Format(time.RFC1123Z), hex.EncodeToString(b), host, boundary)
	for _, part := range []struct{ ctype, body string }{{"text/plain", textBody}, {"text/html", htmlBody}} {
		fmt.Fprintf(&buf, "--%s\r\nContent-Type: %s; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", boundary, part.ctype)
		qp := quotedprintable.NewWriter(&buf)
		qp.Write([]byte(part.body))
		qp.Close()
		buf.WriteString("\r\n")
	}
	fmt.Fprintf(&buf, "--%s--\r\n", boundary)
	return buf.Bytes()
}

// HA posts a JSON payload to a Home Assistant webhook URL
// (http(s)://ha:8123/api/webhook/<id>).
// insecureTLS skips certificate checks (for HA with a self-signed cert).
func HA(ctx context.Context, webhookURL string, insecureTLS bool, payload any) error {
	if webhookURL == "" {
		return errors.New("Home Assistant webhook URL not set")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", webhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := http.DefaultClient
	if insecureTLS {
		client = insecureClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("Home Assistant webhook: HTTP %d", resp.StatusCode)
	}
	return nil
}

var insecureClient = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
