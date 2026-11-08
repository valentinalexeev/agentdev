package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUnitsWeighted(t *testing.T) {
	p := defaultPrices()
	// a cache-read-heavy response: 100 input, 10000 cache read, 200 output on the reference model
	tc := tokenCounts{tokIn: 100, tokCacheRead: 10000, tokOut: 200}
	got := p.units("claude-sonnet-4-5-20250929", tc)
	if got != 100+1000+1000 {
		t.Errorf("units %d", got)
	}
	raw := int64(100 + 10000 + 200)
	if got*100/raw >= 25 { // far below the raw count (acceptance: cache-heavy run well under its raw tokens)
		t.Errorf("units %d not well below raw %d", got, raw)
	}
	if p.units("claude-opus-4-1", tc) != 5*got {
		t.Error("opus multiplier")
	}
	if p.units("claude-opus-4-5-20251101", tc) <= got || p.units("claude-opus-4-5-20251101", tc) >= 2*got {
		t.Error("opus 4.5 multiplier (longest prefix)")
	}
	if p.units("gpt-oops", tc) != 5*got || p.units("", tc) != 5*got {
		t.Error("unknown model must use the highest multiplier")
	}
	if p.units("claude-sonnet-4", tokenCounts{tokCacheRead: 1}) != 1 {
		t.Error("fractional units must round up")
	}
	if _, ok := p.usdOf(1000); ok {
		t.Error("USD without prices")
	}
}

func TestPricesConfigMap(t *testing.T) {
	dir := t.TempDir()
	logs := captureOut(t)
	os.WriteFile(filepath.Join(dir, "w_cr"), []byte("0.2\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "model.claude-sonnet"), []byte("2"), 0o644)
	os.WriteFile(filepath.Join(dir, "usd_per_million_units"), []byte("3"), 0o644)
	os.WriteFile(filepath.Join(dir, "w_out"), []byte("$(id)"), 0o644) // bad value: default kept
	os.WriteFile(filepath.Join(dir, "Evil_Key"), []byte("1"), 0o644)  // bad key
	os.WriteFile(filepath.Join(dir, "w_in"), []byte("1e9"), 0o644)    // not plain decimal
	os.MkdirAll(filepath.Join(dir, "..data"), 0o755)                  // ConfigMap internals are skipped
	p := newPrices(dir)
	pt := p.get()
	if pt.weights[tokCacheRead] != 0.2 || pt.weights[tokOut] != 5 || pt.weights[tokIn] != 1 || pt.multiplier("claude-sonnet-4") != 2 {
		t.Errorf("prices %+v", pt)
	}
	if usd, ok := pt.usdOf(2_000_000); !ok || usd != 6 {
		t.Errorf("usd %v %v", usd, ok)
	}
	if n := strings.Count(logs.String(), "proxy-price-invalid"); n != 1 {
		t.Errorf("%d price events: %s", n, logs)
	}
	if !strings.Contains(logs.String(), "Evil_Key,w_in,w_out") {
		t.Errorf("event %s", logs)
	}
	p.reload() // same bad keys: no second event
	if n := strings.Count(logs.String(), "proxy-price-invalid"); n != 1 {
		t.Errorf("repeated event")
	}
}

func TestCacheWriteSplit(t *testing.T) {
	var u apiUsage
	json.Unmarshal([]byte(`{"input_tokens":1,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":10,"ephemeral_1h_input_tokens":20},"cache_read_input_tokens":5,"output_tokens":2}`), &u)
	if c := u.counts(); c != (tokenCounts{1, 10, 20, 5, 2}) {
		t.Errorf("counts %v", c)
	}
	json.Unmarshal([]byte(`{"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_1h_input_tokens":99}}`), &u)
	if c := u.counts(); c[tokCacheWrite1h] != 30 || c[tokCacheWrite5m] != 0 {
		t.Errorf("1h clamp %v", c)
	}
}

func TestJobAttribution(t *testing.T) {
	f := newFixture(t, 0)
	f.upstream = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-4-5\",\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":1000,\"output_tokens\":1}}}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":20}}\n\n")
	}
	key := agentKey(testSigning, "alice")
	for _, job := range []string{"issue-12", "issue-12", "BAD JOB$", ""} {
		resp := f.post("/v1/messages", key, `{}`, map[string]string{"X-Agentdev-Job": job})
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if f.got.Header.Get("X-Agentdev-Job") != "" {
			t.Fatal("job header sent upstream")
		}
	}
	var ul usageLine
	u := lines(f.logs, usagePrefix)
	json.Unmarshal([]byte(u[0]), &ul)
	if ul.Job != "issue-12" || ul.Model != "claude-sonnet-4-5" || ul.Units != 10+100+100 || ul.CacheRead != 1000 || ul.InputTokens != 1010 || ul.OutputTokens != 20 {
		t.Errorf("usage %+v", ul)
	}
	var ul2 usageLine
	json.Unmarshal([]byte(u[2]), &ul2)
	if ul2.Job != "" {
		t.Errorf("bad job kept: %q", ul2.Job)
	}
	var st struct {
		Unit    string
		Budgets []budgetStatus
		Jobs    []jobTotal
	}
	r, _ := http.Get(f.srv.URL + "/status")
	json.NewDecoder(r.Body).Decode(&st)
	r.Body.Close()
	if st.Unit != "units" || len(st.Jobs) != 1 || st.Jobs[0].Job != "issue-12" || st.Jobs[0].Units != 420 || st.Jobs[0].Requests != 2 || st.Jobs[0].Tokens["cache_read"] != 2000 || st.Jobs[0].USD != nil {
		t.Errorf("status %+v", st)
	}
	if len(st.Budgets) != 1 || st.Budgets[0].Used != 840 || st.Budgets[0].UsedUSD != nil {
		t.Errorf("budgets %+v", st.Budgets)
	}
}

func TestJobsBoundedAndExpire(t *testing.T) {
	j := newJobs()
	now := time.Unix(1_800_000_000, 0)
	j.now = func() time.Time { return now }
	for i := 0; i < jobsPerAgent+50; i++ {
		now = now.Add(time.Second)
		j.add("a", "job-"+string(rune('a'+i%26))+jsonInt(int64(i)), 1, tokenCounts{})
	}
	if n := len(j.m["a"]); n != jobsPerAgent {
		t.Errorf("%d jobs kept", n)
	}
	if j.used("a", "job-a0") != 0 {
		t.Error("oldest job not dropped")
	}
	now = now.Add(jobRetention + time.Hour)
	if len(j.snapshot(defaultPrices())) != 0 {
		t.Error("expired jobs listed")
	}
}

func TestValuePattern(t *testing.T) {
	if !valueRe.MatchString("0.1") || valueRe.MatchString("1.1234567") || valueRe.MatchString("-1") || valueRe.MatchString("1e3") {
		t.Error("value pattern")
	}
}

func captureOut(t *testing.T) *strings.Builder {
	b := &strings.Builder{}
	out = b
	t.Cleanup(func() { out = os.Stdout })
	return b
}
