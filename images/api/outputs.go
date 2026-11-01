package main

// SIEM outputs besides the webhook (#71): syslog over TLS (RFC 5424 messages, RFC 5425 transport) and OTLP logs over
// HTTPS (OTLP/HTTP with the JSON encoding). Both reuse the webhook's fair per-agent queues and retries (webhook.go):
// only the encoding and the transport differ. Configuration comes from the Secret agentdev-api-siem (instance
// config, never in git); see outputsFromEnv.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---- syslog over TLS ----

// syslog severities (RFC 5424 6.2.1) for agentdev severities; facility 13 (log audit).
var syslogSeverity = map[string]int{"low": 5 /* notice */, "medium": 4 /* warning */, "high": 2 /* critical */}

const syslogFacility = 13

// sdEscape escapes a structured-data parameter value (RFC 5424 6.3.3).
func sdEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, `]`, `\]`).Replace(s)
}

// syslogMessage renders e as one RFC 5424 message. The structured data element agentdev@32473 carries the fields (32473
// is the example enterprise number of RFC 5612, used until agentdev has its own); MSG is the full event as JSON, so a
// SIEM can parse either.
func syslogMessage(hostname string, e securityEvent) ([]byte, error) {
	sev, ok := syslogSeverity[e.Severity]
	if !ok {
		sev = 2
	}
	body, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	if hostname == "" {
		hostname = "-"
	}
	var sd strings.Builder
	sd.WriteString("[agentdev@32473")
	for _, kv := range [][2]string{{"schema", e.Schema}, {"id", strconv.FormatInt(e.ID, 10)}, {"agent", e.Agent},
		{"job", e.Job}, {"source", e.Source}, {"untrusted", strconv.FormatBool(e.Untrusted)}, {"rule", e.Rule},
		{"severity", e.Severity}, {"hash", e.Hash}, {"prev", e.Prev}, {"chain", e.Chain}} {
		fmt.Fprintf(&sd, ` %s="%s"`, kv[0], sdEscape(kv[1]))
	}
	sd.WriteString("]")
	// <PRI>1 TIMESTAMP HOSTNAME APP-NAME PROCID MSGID SD MSG; the BOM marks MSG as UTF-8
	msg := fmt.Sprintf("<%d>1 %s %s agentdev-api - security %s \ufeff%s", syslogFacility*8+sev,
		e.Time.UTC().Format("2006-01-02T15:04:05.000000Z"), hostname, sd.String(), body)
	return []byte(msg), nil
}

// syslogTLS keeps one TLS connection to the collector and writes octet-counted frames (RFC 5425 4.3). After a write
// error it closes the connection; the next try (webhook retries) dials again.
type syslogTLS struct {
	Addr string
	TLS  *tls.Config
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)

	mu   sync.Mutex
	conn net.Conn
}

func (s *syslogTLS) Send(ctx context.Context, msg []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: s.TLS}
		dial := d.DialContext
		if s.Dial != nil {
			dial = s.Dial
		}
		c, err := dial(ctx, "tcp", s.Addr)
		if err != nil {
			return err
		}
		s.conn = c
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(s.conn, "%d %s", len(msg), msg); err != nil {
		s.conn.Close()
		s.conn = nil
		return err
	}
	return nil
}

// ---- OTLP logs ----

// otlpSeverity maps to OpenTelemetry SeverityNumber: INFO2 (10), WARN (13), ERROR (17).
var otlpSeverity = map[string]int{"low": 10, "medium": 13, "high": 17}

type otlpAttr struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

func strAttr(k, v string) otlpAttr { return otlpAttr{Key: k, Value: map[string]any{"stringValue": v}} }

// otlpBody renders e as an ExportLogsServiceRequest (OTLP/HTTP JSON). Attribute names follow the ECS mapping in
// docs/security-model.md where one exists, with agentdev.* for the rest.
func otlpBody(e securityEvent) ([]byte, error) {
	sev, ok := otlpSeverity[e.Severity]
	if !ok {
		sev = 17
	}
	full, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	attrs := []otlpAttr{
		strAttr("event.name", "agentdev.security"), strAttr("event.id", e.Hash), strAttr("event.kind", "alert"),
		strAttr("event.category", "intrusion_detection"), strAttr("rule.name", e.Rule), strAttr("user.name", e.Agent),
		strAttr("agentdev.schema", e.Schema), strAttr("agentdev.source", e.Source), strAttr("agentdev.job", e.Job),
		strAttr("agentdev.prev", e.Prev), strAttr("agentdev.chain", e.Chain),
		strAttr("k8s.pod.name", e.Pod), strAttr("k8s.container.name", e.Container),
		{Key: "agentdev.id", Value: map[string]any{"intValue": strconv.FormatInt(e.ID, 10)}},
		{Key: "agentdev.untrusted", Value: map[string]any{"boolValue": e.Untrusted}},
	}
	req := map[string]any{"resourceLogs": []any{map[string]any{
		"resource": map[string]any{"attributes": []otlpAttr{strAttr("service.name", "agentdev-api")}},
		"scopeLogs": []any{map[string]any{
			"scope": map[string]any{"name": "agentdev.security", "version": schemaV1},
			"logRecords": []any{map[string]any{
				"timeUnixNano":   strconv.FormatInt(e.Time.UnixNano(), 10),
				"severityNumber": sev,
				"severityText":   strings.ToUpper(e.Severity),
				"body":           map[string]any{"stringValue": string(full)},
				"attributes":     attrs,
			}},
		}},
	}}}
	return json.Marshal(req)
}

// otlpHTTP posts to the collector's /v1/logs endpoint with the configured headers (for example an API key).
type otlpHTTP struct {
	URL     string
	Headers map[string]string
	Client  *http.Client
}

func (o *otlpHTTP) Send(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "agentdev-api")
	for k, v := range o.Headers {
		req.Header.Set(k, v)
	}
	resp, err := o.Client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// parseHeaders reads "k1=v1,k2=v2" (the OTEL_EXPORTER_OTLP_HEADERS format, values URL-encoded).
func parseHeaders(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range strings.Split(s, ",") {
		if strings.TrimSpace(p) == "" {
			continue
		}
		k, v, ok := strings.Cut(p, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, errors.New("headers must be key=value pairs separated by commas")
		}
		dv, err := url.QueryUnescape(strings.TrimSpace(v))
		if err != nil {
			return nil, err
		}
		out[strings.TrimSpace(k)] = dv
	}
	return out, nil
}

// ---- configuration ----

// tlsFromEnv builds the client TLS config: system roots plus an optional CA (PEM) and an optional client certificate
// (mTLS). TLS 1.2 is the minimum; certificates are always verified.
func tlsFromEnv(getenv0 func(string) string, prefix string) (*tls.Config, error) {
	// PEM values come from a one-line env file: accept a literal \n as the line break
	getenv := func(k string) string { return strings.ReplaceAll(getenv0(k), `\n`, "\n") }
	c := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: getenv(prefix + "_SERVER_NAME")}
	if ca := getenv(prefix + "_CA"); ca != "" {
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM([]byte(ca)) {
			return nil, fmt.Errorf("%s_CA: no PEM certificate", prefix)
		}
		c.RootCAs = pool
	}
	cert, key := getenv(prefix+"_CLIENT_CERT"), getenv(prefix+"_CLIENT_KEY")
	if (cert == "") != (key == "") {
		return nil, fmt.Errorf("%s_CLIENT_CERT and %s_CLIENT_KEY must be set together", prefix, prefix)
	}
	if cert != "" {
		kp, err := tls.X509KeyPair([]byte(cert), []byte(key))
		if err != nil {
			return nil, fmt.Errorf("%s client certificate: %w", prefix, err)
		}
		c.Certificates = []tls.Certificate{kp}
	}
	return c, nil
}

// outputsFromEnv returns the configured outputs. Each one refuses an insecure setting instead of falling back:
//
//	SECURITY_WEBHOOK_URL, SECURITY_WEBHOOK_SECRET          signed webhook (#70)
//	SECURITY_SYSLOG_ADDR (host:port, usually 6514)         syslog over TLS; optional SECURITY_SYSLOG_CA,
//	                                                       _SERVER_NAME, _CLIENT_CERT, _CLIENT_KEY (PEM)
//	SECURITY_OTLP_URL (https://.../v1/logs)                OTLP/HTTP JSON; optional SECURITY_OTLP_HEADERS (k=v,...),
//	                                                       SECURITY_OTLP_CA, _CLIENT_CERT, _CLIENT_KEY
func outputsFromEnv(getenv func(string) string) ([]*webhook, error) {
	var outs []*webhook
	if u := getenv("SECURITY_WEBHOOK_URL"); u != "" {
		sec := getenv("SECURITY_WEBHOOK_SECRET")
		if sec == "" {
			return nil, errors.New("SECURITY_WEBHOOK_URL is set but SECURITY_WEBHOOK_SECRET is empty: refusing to send unsigned events")
		}
		outs = append(outs, newWebhook(u, []byte(sec)))
	}
	if addr := getenv("SECURITY_SYSLOG_ADDR"); addr != "" {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return nil, fmt.Errorf("SECURITY_SYSLOG_ADDR must be host:port: %w", err)
		}
		tc, err := tlsFromEnv(getenv, "SECURITY_SYSLOG")
		if err != nil {
			return nil, err
		}
		host, _ := os.Hostname()
		s := &syslogTLS{Addr: addr, TLS: tc}
		o := newWebhook("", nil)
		o.Name, o.Workers, o.Send = "syslog", 1, s.Send // one connection, events in order
		o.Encode = func(e securityEvent) ([]byte, error) { return syslogMessage(host, e) }
		outs = append(outs, o)
	}
	if u := getenv("SECURITY_OTLP_URL"); u != "" {
		pu, err := url.Parse(u)
		if err != nil || pu.Scheme != "https" || pu.Host == "" {
			return nil, errors.New("SECURITY_OTLP_URL must be an https:// URL (for example https://collector:4318/v1/logs)")
		}
		hdr, err := parseHeaders(getenv("SECURITY_OTLP_HEADERS"))
		if err != nil {
			return nil, fmt.Errorf("SECURITY_OTLP_HEADERS: %w", err)
		}
		tc, err := tlsFromEnv(getenv, "SECURITY_OTLP")
		if err != nil {
			return nil, err
		}
		oh := &otlpHTTP{URL: u, Headers: hdr,
			Client: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: tc}}}
		o := newWebhook("", nil)
		o.Name, o.Send, o.Encode = "otlp", oh.Send, otlpBody
		outs = append(outs, o)
	}
	return outs, nil
}
