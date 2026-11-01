package main

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Usage observed by the proxy itself (#66, replaces the forge sidecar's POST /usage, closes R47). The rate-limit
// windows come from the upstream response headers, the token counts from the response body. Nothing here is read from
// the agent: the proxy is outside the model's reach, so a usage line from claude-proxy can be trusted.

type window struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    int64   `json:"resetsAt"`
}

// usageLine has the schema of #93 (operator usage.go usageLine) plus agent and secret, which the operator takes
// only from lines of claude-proxy pods.
type usageLine struct {
	Agent        string  `json:"agent"`
	Secret       string  `json:"secret"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	RateLimited  bool    `json:"rate_limited"`
	ResetsAt     int64   `json:"resets_at,omitempty"`
	FiveHour     *window `json:"five_hour,omitempty"`
	Weekly       *window `json:"seven_day,omitempty"`
}

func headerWindow(h http.Header, name string, now time.Time, length time.Duration) *window {
	u, err1 := strconv.ParseFloat(h.Get("anthropic-ratelimit-unified-"+name+"-utilization"), 64)
	r, err2 := strconv.ParseInt(h.Get("anthropic-ratelimit-unified-"+name+"-reset"), 10, 64)
	if err1 != nil || err2 != nil || math.IsNaN(u) || u < 0 || u > 10 || r < 0 || r > now.Add(length+time.Hour).Unix() {
		return nil
	}
	return &window{u, r}
}

// fromHeaders fills the rate-limit part of a usage line from the upstream response.
func fromHeaders(u *usageLine, status int, h http.Header, now time.Time) {
	u.FiveHour = headerWindow(h, "5h", now, 5*time.Hour)
	u.Weekly = headerWindow(h, "7d", now, 7*24*time.Hour)
	if status == http.StatusTooManyRequests || strings.EqualFold(h.Get("anthropic-ratelimit-unified-status"), "rejected") {
		u.RateLimited = true
		if r, err := strconv.ParseInt(h.Get("anthropic-ratelimit-unified-reset"), 10, 64); err == nil && r > now.Unix() && r <= now.Add(8*24*time.Hour).Unix() {
			u.ResetsAt = r
		} else if s, err := strconv.ParseInt(h.Get("retry-after"), 10, 64); err == nil && s > 0 && s <= 8*24*3600 {
			u.ResetsAt = now.Unix() + s
		}
	}
}

type apiUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

func (a *apiUsage) input() int64 {
	return clampTok(a.InputTokens) + clampTok(a.CacheCreationInputTokens) + clampTok(a.CacheReadInputTokens)
}

func clampTok(n int64) int64 {
	if n < 0 {
		return 0
	}
	if n > 1e10 {
		return 1e10
	}
	return n
}

// tokenMeter counts the tokens of one response while the body streams to the client. It understands both a JSON
// body ({"usage":{...}}) and server-sent events (message_start carries input usage, message_delta the cumulative
// output tokens). It never buffers more than one line (capped) and never changes the bytes passed through.
type tokenMeter struct {
	sse     bool
	mu      sync.Mutex
	in, out int64
	buf     bytes.Buffer // JSON body (capped) or the current SSE line
	over    bool
}

const meterCap = 1 << 20

func (m *tokenMeter) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.sse {
		if !m.over && m.buf.Len()+len(p) <= meterCap {
			m.buf.Write(p)
		} else {
			m.over = true
		}
		return len(p), nil
	}
	for _, c := range p {
		if c == '\n' {
			m.sseLine(m.buf.Bytes())
			m.buf.Reset()
			m.over = false
			continue
		}
		if m.buf.Len() < meterCap {
			m.buf.WriteByte(c)
		} else {
			m.over = true
		}
	}
	return len(p), nil
}

func (m *tokenMeter) sseLine(line []byte) {
	data, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r"), []byte("data:"))
	if !ok || m.over {
		return
	}
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Usage apiUsage `json:"usage"`
		} `json:"message"`
		Usage apiUsage `json:"usage"`
	}
	if json.Unmarshal(bytes.TrimSpace(data), &ev) != nil {
		return
	}
	switch ev.Type {
	case "message_start":
		m.in += ev.Message.Usage.input()
		m.out = clampTok(ev.Message.Usage.OutputTokens)
	case "message_delta":
		if i := ev.Usage.input(); i > 0 { // newer API versions repeat the input usage here
			m.in = i
		}
		if o := clampTok(ev.Usage.OutputTokens); o > m.out { // cumulative
			m.out = o
		}
	}
}

// totals returns the counted input and output tokens (after the body ended).
func (m *tokenMeter) totals() (int64, int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.sse && !m.over {
		var body struct {
			Usage apiUsage `json:"usage"`
		}
		if json.Unmarshal(m.buf.Bytes(), &body) == nil {
			return body.Usage.input(), clampTok(body.Usage.OutputTokens)
		}
	}
	if m.sse && m.buf.Len() > 0 { // last line without a newline
		m.sseLine(m.buf.Bytes())
		m.buf.Reset()
	}
	return m.in, m.out
}

// meteredBody passes the upstream body through and calls done once with the totals at EOF or Close.
type meteredBody struct {
	io.ReadCloser
	m    *tokenMeter
	once sync.Once
	done func(in, out int64)
}

func (b *meteredBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		_, _ = b.m.Write(p[:n])
	}
	if err != nil {
		b.finish()
	}
	return n, err
}

func (b *meteredBody) Close() error {
	b.finish()
	return b.ReadCloser.Close()
}

func (b *meteredBody) finish() {
	b.once.Do(func() { b.done(b.m.totals()) })
}
