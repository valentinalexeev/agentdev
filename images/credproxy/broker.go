package main

// Credential broker (#202, ADR 0003 D2 mode broker): the generalised credproxy. The same binary, started with
// MODE=broker, adds a project secret to requests for one upstream, base-URL style. An agent calls
//
//	$AGENTDEV_BROKER_URL/s/<secret>/<path>   with its agent key (Bearer, or the password of Basic auth)
//
// and the broker forwards to <upstream>/<path> with the secret's value in the configured header. The agent never
// sees the value. The routes (which secret goes to which upstream and which agents may use it) are written by the
// operator into ROUTES_FILE (ConfigMap credential-broker-routes); the values are files VALUES_DIR/<secret>, the
// projected Secrets ps-<secret> synced by External Secrets Operator. Both are re-read, so a rotated value applies
// without a restart.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// route is one ProjectSecret in mode broker, as the operator writes it.
type route struct {
	Upstream string   `json:"upstream"`
	Header   string   `json:"header"`
	Scheme   string   `json:"scheme"` // bearer, basic, raw
	Agents   []string `json:"agents"` // agents allowed to use it (scope project: the agents of the repository)
}

var (
	secretNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,48}[a-z0-9])?$`)
	headerRe     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,63}$`)
)

// hop-by-hop and credential headers the agent must never pass on; the route's header is removed as well.
var strippedHeaders = []string{"Authorization", "Proxy-Authorization", "Cookie", "X-Api-Key", "Accept-Encoding"}

// maxRewrite: text responses up to this size have the upstream base URL rewritten to the broker's; larger ones pass
// unchanged (a registry index is small; package archives are not text).
const maxRewrite = 32 << 20

type broker struct {
	signing   []byte
	routes    string // routes file
	values    string // values directory
	self      string // the broker's own base URL as agents see it, for rewriting absolute links
	transport http.RoundTripper
	now       func() time.Time

	mu   sync.Mutex
	used map[string]int64 // agent/secret -> unix hour of the last broker-use event (one per hour)
}

func newBroker(signing []byte, routes, values, self string, transport http.RoundTripper) *broker {
	return &broker{signing: signing, routes: routes, values: values, self: strings.TrimRight(self, "/"),
		transport: transport, now: time.Now, used: map[string]int64{}}
}

func (b *broker) loadRoutes() map[string]route {
	data, err := os.ReadFile(b.routes)
	if err != nil {
		return nil
	}
	var m map[string]route
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	return m
}

// brokerKey takes the agent key from Authorization: Bearer <key> or Basic <user:key>.
func brokerKey(auth string) string {
	if k, ok := strings.CutPrefix(auth, "Bearer "); ok {
		return strings.TrimSpace(k)
	}
	if enc, ok := strings.CutPrefix(auth, "Basic "); ok {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(enc))
		if err != nil {
			return ""
		}
		_, pw, _ := strings.Cut(string(raw), ":")
		return pw
	}
	return ""
}

func headerValue(scheme, v string) string {
	switch scheme {
	case "basic":
		return "Basic " + v
	case "raw":
		return v
	default:
		return "Bearer " + v
	}
}

// target joins the upstream base and the rest of the request path. The rest is the escaped path; a path that would
// leave the upstream's base path (.., encoded slashes or dots) is refused.
func target(up *url.URL, rest, query string) (*url.URL, bool) {
	low := strings.ToLower(rest)
	if strings.Contains(low, "%2f") || strings.Contains(low, "%2e") || strings.Contains(low, "%5c") || strings.Contains(rest, "\\") {
		return nil, false
	}
	for _, seg := range strings.Split(rest, "/") {
		if seg == ".." || seg == "." {
			return nil, false
		}
	}
	p := strings.TrimRight(up.EscapedPath(), "/") + "/" + strings.TrimLeft(rest, "/")
	u, err := url.Parse(up.Scheme + "://" + up.Host + p)
	if err != nil {
		return nil, false
	}
	u.RawQuery = query
	return u, true
}

func (b *broker) fail(w http.ResponseWriter, status int, msg string) {
	http.Error(w, "credential-broker: "+msg, status)
}

func (b *broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), "/s/")
	if !ok {
		b.fail(w, http.StatusNotFound, "use /s/<secret>/<path>")
		return
	}
	name, rest, _ := strings.Cut(rest, "/")
	agent := verifyKey(b.signing, brokerKey(r.Header.Get("Authorization")))
	if agent == "" {
		emitEvent("", "broker-key-invalid", "medium", r.Method+" /s/"+clip(name, 60)+": missing or wrong agent key")
		w.Header().Set("WWW-Authenticate", `Basic realm="agentdev credential broker"`)
		b.fail(w, http.StatusUnauthorized, "missing or wrong agent key")
		return
	}
	rt, found := b.loadRoutes()[name]
	if !secretNameRe.MatchString(name) || !found {
		emitEvent(agent, "broker-unknown-secret", "medium", r.Method+" /s/"+clip(name, 60)+": no such secret in mode broker")
		b.fail(w, http.StatusNotFound, "no secret "+clip(name, 60)+" in mode broker")
		return
	}
	allowed := false
	for _, a := range rt.Agents {
		allowed = allowed || a == agent
	}
	if !allowed {
		emitEvent(agent, "broker-secret-denied", "high", r.Method+" /s/"+name+": the secret is not in the agent's scope")
		b.fail(w, http.StatusForbidden, "secret "+name+" is not in the scope of agent "+agent)
		return
	}
	up, err := url.Parse(rt.Upstream)
	if err != nil || up.Scheme != "https" && !(up.Scheme == "http" && os.Getenv("ALLOW_HTTP_UPSTREAM") == "true") || up.Host == "" || up.User != nil {
		b.fail(w, http.StatusBadGateway, "bad upstream for "+name)
		return
	}
	dst, ok := target(up, rest, r.URL.RawQuery)
	if !ok {
		emitEvent(agent, "broker-path", "medium", r.Method+" /s/"+name+"/"+clip(rest, 100)+": path leaves the upstream base")
		b.fail(w, http.StatusBadRequest, "path not allowed")
		return
	}
	raw, err := os.ReadFile(filepath.Join(b.values, name))
	value := strings.TrimSpace(string(raw))
	if err != nil || value == "" {
		b.fail(w, http.StatusServiceUnavailable, "secret "+name+" has no value yet")
		return
	}
	header := rt.Header
	if !headerRe.MatchString(header) {
		header = "Authorization"
	}
	b.noteUse(agent, name, r.Method)
	self := b.self + "/s/" + name
	p := &httputil.ReverseProxy{
		Transport:     b.transport,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = dst
			pr.Out.Host = dst.Host
			for _, h := range strippedHeaders {
				pr.Out.Header.Del(h)
			}
			pr.Out.Header.Del(header)
			pr.Out.Header.Set(header, headerValue(rt.Scheme, value))
		},
		ModifyResponse: func(resp *http.Response) error {
			return rewriteBody(resp, strings.TrimRight(rt.Upstream, "/"), self, value)
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Printf("broker %s: upstream error: %v", name, err)
			if errors.Is(err, errUnsolicitedEncoding) {
				emitEvent(agent, "broker-encoded-response", "medium", r.Method+" /s/"+name+": upstream answered with an unrequested Content-Encoding")
			}
			b.fail(w, http.StatusBadGateway, "upstream not reachable")
		},
	}
	p.ServeHTTP(w, r)
}

// noteUse prints one broker-use event per agent, secret and hour (ADR 0003 D1: every use is a SecurityEvent, without
// flooding the list with one event per package download).
func (b *broker) noteUse(agent, name, method string) {
	hour := b.now().Unix() / 3600
	k := agent + "/" + name
	b.mu.Lock()
	first := b.used[k] != hour
	b.used[k] = hour
	b.mu.Unlock()
	if first {
		emitEvent(agent, "broker-use", "low", method+" through secret "+name)
	}
}

// rewriteBody: absolute links to the upstream in a text response (a registry index lists tarball URLs) are pointed
// at the broker, so the client keeps coming through it; and the value never goes back to the agent, even if an
// upstream echoes it.
func rewriteBody(resp *http.Response, upstream, self, value string) error {
	resp.Header.Del("Set-Cookie")
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	text := strings.Contains(ct, "json") || strings.HasPrefix(ct, "text/") || strings.Contains(ct, "xml")
	if loc := resp.Header.Get("Location"); strings.HasPrefix(loc, upstream) {
		resp.Header.Set("Location", self+strings.TrimPrefix(loc, upstream))
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
		// the broker strips Accept-Encoding (and Go's transport decodes the gzip it asked for itself): an encoded body
		// was not requested, and a value inside it could not be redacted (review of #272)
		resp.Body.Close()
		return errUnsolicitedEncoding
	}
	if !text || resp.ContentLength > maxRewrite {
		resp.Body = redactStream(resp.Body, value)
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRewrite+1))
	if err != nil {
		resp.Body.Close()
		return err
	}
	if len(body) > maxRewrite { // larger than announced: pass it on, links unchanged, the value still redacted
		resp.Body = redactStream(struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(body), resp.Body), resp.Body}, value)
		return nil
	}
	resp.Body.Close()
	body = bytes.ReplaceAll(body, []byte(upstream), []byte(self))
	if len(value) >= 8 {
		body = bytes.ReplaceAll(body, []byte(value), []byte("[redacted]"))
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return nil
}

var errUnsolicitedEncoding = errors.New("upstream sent an encoded body the broker did not ask for")

// redactStream replaces the value in a body that is not rewritten as a whole (binary, or larger than maxRewrite).
// It holds back len(value)-1 bytes between reads, so a value split across two reads is still found; the length of
// the body changes, so Content-Length must not be trusted by the caller (ReverseProxy streams it chunked).
func redactStream(rc io.ReadCloser, value string) io.ReadCloser {
	if len(value) < 8 {
		return rc
	}
	return &redactor{rc: rc, v: []byte(value)}
}

type redactor struct {
	rc   io.ReadCloser
	v    []byte
	buf  []byte // read but not yet returned
	out  []byte // ready to return
	done bool
}

func (r *redactor) Read(p []byte) (int, error) {
	for len(r.out) == 0 {
		if r.done {
			return 0, io.EOF
		}
		chunk := make([]byte, 32<<10)
		n, err := r.rc.Read(chunk)
		r.buf = append(r.buf, chunk[:n]...)
		r.buf = bytes.ReplaceAll(r.buf, r.v, []byte("[redacted]"))
		if err != nil {
			r.out, r.buf, r.done = r.buf, nil, true
			if err != io.EOF {
				return 0, err
			}
			continue
		}
		if keep := len(r.v) - 1; len(r.buf) > keep {
			r.out = append(r.out, r.buf[:len(r.buf)-keep]...)
			r.buf = append([]byte(nil), r.buf[len(r.buf)-keep:]...)
		}
	}
	n := copy(p, r.out)
	r.out = r.out[n:]
	return n, nil
}

func (r *redactor) Close() error { return r.rc.Close() }

// publicOnly refuses connections to loopback, private, link-local, CGNAT and unspecified addresses, checked on the
// address actually dialled (after DNS, so a public name resolving to 10.x or a rebinding answer is refused too). The
// NetworkPolicy says the same on GKE; this holds on any CNI (review of #272).
func publicOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !publicIP(ip) {
		return fmt.Errorf("credential-broker: upstream address %s is not public", host)
	}
	return nil
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func publicIP(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast() || cgnat.Contains(ip))
}

// brokerTransport is http.DefaultTransport with publicOnly on every dial.
func brokerTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: publicOnly}
	t.DialContext = d.DialContext
	t.Proxy = nil // the broker is the egress for its upstreams; it never goes through another proxy
	return t
}
