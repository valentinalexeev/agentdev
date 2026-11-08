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
	got := p.units("claude-sonnet-4-5-20250929", tc) // reference model
	if got != 100+1000+1000 {
		t.Errorf("units %d", got)
	}
	raw := int64(100 + 10000 + 200)
	if got*100/raw >= 25 { // 2% output at weight 5: about 20%
		t.Errorf("units %d not well below raw %d", got, raw)
	}
	// acceptance of #196: a cache-read-heavy run uses less than 20% of its raw token count. Shape of a Claude Code
	// turn: ~97% cache read, ~1% each of fresh input, cache write and output
	run := tokenCounts{tokIn: 300, tokCacheWrite5m: 300, tokCacheRead: 29000, tokOut: 400}
	runRaw := int64(300 + 300 + 29000 + 400)
	if u := p.units("claude-sonnet-4-5-20250929", run); u*100/runRaw >= 20 {
		t.Errorf("cache-heavy run: %d units of %d raw tokens, want < 20%%", u, runRaw)
	}
	for model, m := range map[string]float64{"claude-opus-4-1-20250805": 5, "claude-opus-4-20250514": 5, "claude-opus-4-5-20251101": 1.666667,
		"claude-opus-5": 1.666667, "claude-opus-5-5": 1.333333, "claude-sonnet-5-5": 0.666667, "claude-haiku-4-5": 0.333333,
		"claude-haiku-5-5": 0.033333, "claude-fable-5-1": 3.333333} {
		if p.multiplier(model) != m {
			t.Errorf("%s: multiplier %v, want %v", model, p.multiplier(model), m)
		}
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
	if pt.weights[tokCacheRead] != 0.2 || pt.weights[tokOut] != 5 || pt.weights[tokIn] != 1 || pt.multiplier("claude-sonnet-4") != 2 || pt.multiplier("claude-sonnet-5") != 0.666667 {
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
	if n := len(j.m["a"]); n > jobsPerAgent {
		t.Errorf("%d jobs kept", n)
	}
	if j.used("a", "job-a0") != 0 || j.used("a", otherJob) != 51 {
		t.Errorf("evicted jobs not folded into %s: %d", otherJob, j.used("a", otherJob))
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

func TestJobBudgetLoopGuard(t *testing.T) {
	f := newFixture(t, 0)
	f.srv.Config.Handler.(*server).jobLimit = 300
	f.upstream = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"model":"claude-sonnet-4-5","usage":{"input_tokens":100,"output_tokens":20}}`) // 200 units
	}
	key := agentKey(testSigning, "alice")
	for i, want := range []int{200, 200, 429, 429} {
		resp := f.post("/v1/messages", key, `{}`, map[string]string{"X-Agentdev-Job": "issue-9"})
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("request %d: %d", i, resp.StatusCode)
		}
	}
	if n := strings.Count(f.logs.String(), "job-budget-exhausted"); n != 1 {
		t.Errorf("%d job events", n)
	}
	// another job of the same agent, and requests without a job id, are not stopped by it
	for _, job := range []string{"issue-10", ""} {
		resp := f.post("/v1/messages", key, `{}`, map[string]string{"X-Agentdev-Job": job})
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("job %q: %d", job, resp.StatusCode)
		}
	}
	if st := f.srv.Config.Handler.(*server).jobs.snapshot(defaultPrices()); !st[0].Stopped || st[0].Job != "issue-9" {
		t.Errorf("stopped flag %+v", st[0])
	}
}

// Review of #275: an agent sending many fresh job ids cannot evict its expensive jobs from the report.
func TestJobsChurnKeepsExpensiveJobs(t *testing.T) {
	j := newJobs()
	now := time.Unix(1_800_000_000, 0)
	j.now = func() time.Time { return now }
	j.add("a", "issue-1", 50_000, tokenCounts{})
	for i := 0; i < 5*jobsPerAgent; i++ {
		now = now.Add(time.Second)
		j.add("a", "noise-"+jsonInt(int64(i)), 1, tokenCounts{})
	}
	if j.used("a", "issue-1") != 50_000 {
		t.Error("the expensive job was evicted by churn")
	}
	var sum int64
	for _, e := range j.m["a"] {
		sum += e.Units
	}
	if sum != 50_000+5*jobsPerAgent || len(j.m["a"]) > jobsPerAgent {
		t.Errorf("units lost or too many jobs: %d in %d", sum, len(j.m["a"]))
	}
	if cleanJob(otherJob) != "" {
		t.Error("an agent may send the reserved job id")
	}
}

func TestModelLabelBounded(t *testing.T) {
	p := defaultPrices()
	if p.modelLabel("claude-sonnet-4-5-20250929") != "claude-sonnet-4-5-20250929" || p.modelLabel("gpt-x") != "unknown" ||
		p.modelLabel("") != "unknown" || p.modelLabel("zzz-"+strings.Repeat("a", 90)) != "unknown" {
		t.Error("model label not bounded to known families")
	}
}

// Acceptance of #196 (owner decision 2026-11-08, option 1): through the real proxy path, a cache-read-heavy fixture
// response gives units = ceil(sum(tokens x weight) x multiplier) exactly, the usage line carries the numbers to
// recompute it, and the units are under 20% of the raw tokens.
func TestCacheHeavyFixtureUnderTwentyPercent(t *testing.T) {
	f := newFixture(t, 0)
	f.upstream = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-4-5-20250929\","+
			"\"usage\":{\"input_tokens\":250,\"cache_creation_input_tokens\":400,\"cache_read_input_tokens\":48000,\"output_tokens\":1}}}\n\n")
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":600}}\n\n")
	}
	resp := f.post("/v1/messages", agentKey(testSigning, "alice"), `{}`, map[string]string{jobHeader: "issue-7"})
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	u := lines(f.logs, usagePrefix)
	if len(u) != 1 {
		t.Fatalf("usage %v", u)
	}
	var ul usageLine
	json.Unmarshal([]byte(u[0]), &ul)
	// 250x1 + 400x1.25 + 48000x0.1 + 600x5 = 8550, multiplier 1 (Sonnet 4.x)
	if ul.Units != 8550 || ul.Multiplier != 1 || ul.Tokens["cache_read"] != 48000 || ul.Tokens["cache_write_5m"] != 400 {
		t.Errorf("usage line %s", u[0])
	}
	raw := int64(250 + 400 + 48000 + 600)
	if ul.Units*100/raw >= 20 {
		t.Errorf("cache-heavy fixture: %d units of %d raw tokens, want < 20%%", ul.Units, raw)
	}
	r, _ := http.Get(f.srv.URL + "/status")
	var st struct {
		Weights map[string]float64 `json:"weights"`
		Jobs    []jobTotal         `json:"jobs"`
	}
	json.NewDecoder(r.Body).Decode(&st)
	r.Body.Close()
	if st.Weights["cache_read"] != 0.1 || st.Weights["output"] != 5 || len(st.Jobs) != 1 || st.Jobs[0].Units != 8550 {
		t.Errorf("status %+v", st)
	}
}
