package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Cost units (#196, ADR 0002 D2). The budget counts input-token equivalents at the price of a reference model, not
// raw tokens, because cache reads (about 0.1x input) dominate Claude Code traffic and output costs about 5x:
//
//	units = (input*w_in + cache_write_5m*w_cw5 + cache_write_1h*w_cw1h + cache_read*w_cr + output*w_out) * m(model)
//
// Weights, model multipliers and an optional USD price per million units come from the ConfigMap claude-proxy-prices
// (one file per key in PRICES_DIR). Keys:
//
//	w_in, w_cw5, w_cw1h, w_cr, w_out   token weights
//	model.<prefix>                    multiplier of models whose name starts with <prefix> (longest prefix wins)
//	usd_per_million_units             USD per million units; unset or 0 = no USD estimate
//
// Every value must match valueRe; a bad value keeps the built-in default and prints a security event. The model comes
// from the upstream response, never from the request; an unknown model uses the highest known multiplier (fail closed).

var (
	valueRe    = regexp.MustCompile(`^[0-9]+(\.[0-9]{1,6})?$`)
	priceKeyRe = regexp.MustCompile(`^(w_in|w_cw5|w_cw1h|w_cr|w_out|usd_per_million_units|model\.[a-z0-9][a-z0-9.-]{0,62})$`)
)

// Token types, in the order of the per-type counters.
const (
	tokIn = iota
	tokCacheWrite5m
	tokCacheWrite1h
	tokCacheRead
	tokOut
	tokTypes
)

var tokNames = [tokTypes]string{"input", "cache_write_5m", "cache_write_1h", "cache_read", "output"}

// tokenCounts holds the counted tokens of one response (or a sum), by type.
type tokenCounts [tokTypes]int64

func (t *tokenCounts) add(o tokenCounts) {
	for i := range t {
		t[i] += o[i]
	}
}

func (t tokenCounts) input() int64 {
	return t[tokIn] + t[tokCacheWrite5m] + t[tokCacheWrite1h] + t[tokCacheRead]
}

func (t tokenCounts) byName() map[string]int64 {
	m := make(map[string]int64, tokTypes)
	for i, n := range tokNames {
		m[n] = t[i]
	}
	return m
}

type priceTable struct {
	weights [tokTypes]float64
	models  map[string]float64 // name prefix -> multiplier
	usd     float64            // USD per million units; 0 = no estimate
}

// defaultPrices follow the Anthropic list price ratios (November 2026): cache write 5 min 1.25x, 1 h 2x, cache read
// 0.1x, output 5x input. One unit is one input token at 3 USD per million (Sonnet 4.x); a model's multiplier is its
// input price / 3 USD. The longest matching prefix wins, so the family prefixes cover new versions at the current
// family price, and older versions with other prices are listed explicitly. A model outside every family uses the
// highest multiplier (fail closed). Prices change: override them in ConfigMap claude-proxy-prices.
func defaultPrices() priceTable {
	return priceTable{
		weights: [tokTypes]float64{1, 1.25, 2, 0.1, 5},
		models: map[string]float64{
			"claude-opus":        1.666667, // Opus 4.5 to 5: 5 USD
			"claude-opus-5-5":    1.333333, // Opus 5.5: 4 USD
			"claude-opus-4-2025": 5,        // Opus 4 (claude-opus-4-20250514): 15 USD
			"claude-opus-4-0":    5,
			"claude-opus-4-1":    5,        // Opus 4.1: 15 USD
			"claude-sonnet":      1,        // Sonnet 4.x: 3 USD
			"claude-sonnet-5":    0.666667, // Sonnet 5 and 5.5: 2 USD
			"claude-haiku":       0.333333, // Haiku 4.5: 1 USD
			"claude-haiku-5":     0.033333, // Haiku 5.5: 0.10 USD
			"claude-3-5-haiku":   0.266667,
			"claude-fable":       3.333333, // Fable 5.x: 10 USD
		},
	}
}

// multiplier of a model: the longest matching prefix, else the highest known multiplier.
func (p priceTable) multiplier(model string) float64 {
	best, bestLen, maxM := 0.0, -1, 0.0
	for prefix, m := range p.models {
		if m > maxM {
			maxM = m
		}
		if strings.HasPrefix(model, prefix) && len(prefix) > bestLen {
			best, bestLen = m, len(prefix)
		}
	}
	if bestLen < 0 {
		return maxM
	}
	return best
}

// modelLabel is the metric label of a model: the model when a configured prefix matches, else "unknown". The model
// name comes from the upstream response, so an unbounded set of names must not become an unbounded set of series
// (review of #276); the usage line and the span keep the full name.
func (p priceTable) modelLabel(model string) string {
	for prefix := range p.models {
		if strings.HasPrefix(model, prefix) {
			return model
		}
	}
	return "unknown"
}

// weightsByName lists the token weights (GET /status), so a check can recompute the units of a usage line.
func (p priceTable) weightsByName() map[string]float64 {
	m := make(map[string]float64, tokTypes)
	for i, n := range tokNames {
		m[n] = p.weights[i]
	}
	return m
}

// units of one response, rounded up so that any spend counts.
func (p priceTable) units(model string, t tokenCounts) int64 {
	var u float64
	for i, n := range t {
		u += float64(n) * p.weights[i]
	}
	u *= p.multiplier(model)
	if u <= 0 {
		return 0
	}
	if u > 1e12 {
		return 1e12
	}
	n := int64(u)
	if float64(n) < u {
		n++
	}
	return n
}

// usdOf converts units to estimated USD; ok is false when no price is configured.
func (p priceTable) usdOf(units int64) (float64, bool) {
	if p.usd <= 0 {
		return 0, false
	}
	return float64(units) * p.usd / 1e6, true
}

// prices re-reads PRICES_DIR at most every reload interval, so an edited ConfigMap takes effect without a restart.
type prices struct {
	dir    string
	every  time.Duration
	now    func() time.Time
	mu     sync.Mutex
	cur    priceTable
	loaded time.Time
	bad    string // keys rejected at the last load (one event per change)
}

func newPrices(dir string) *prices {
	p := &prices{dir: dir, every: 30 * time.Second, now: time.Now, cur: defaultPrices()}
	p.reload()
	return p
}

func (p *prices) get() priceTable {
	p.mu.Lock()
	stale := p.now().Sub(p.loaded) >= p.every
	p.mu.Unlock()
	if stale {
		p.reload()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cur
}

func (p *prices) reload() {
	t := defaultPrices()
	var bad []string
	if p.dir != "" {
		entries, _ := os.ReadDir(p.dir)
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") || e.IsDir() { // ConfigMap volume internals (..data, ..2026_...)
				continue
			}
			data, err := os.ReadFile(filepath.Join(p.dir, name))
			if err != nil {
				continue
			}
			v := strings.TrimSpace(string(data))
			if !priceKeyRe.MatchString(name) || !valueRe.MatchString(v) {
				bad = append(bad, name)
				continue
			}
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f > 1e6 {
				bad = append(bad, name)
				continue
			}
			switch name {
			case "w_in":
				t.weights[tokIn] = f
			case "w_cw5":
				t.weights[tokCacheWrite5m] = f
			case "w_cw1h":
				t.weights[tokCacheWrite1h] = f
			case "w_cr":
				t.weights[tokCacheRead] = f
			case "w_out":
				t.weights[tokOut] = f
			case "usd_per_million_units":
				t.usd = f
			default:
				t.models[strings.TrimPrefix(name, "model.")] = f
			}
		}
	}
	sort.Strings(bad)
	key := strings.Join(bad, ",")
	p.mu.Lock()
	p.cur, p.loaded = t, p.now()
	changed := key != p.bad
	p.bad = key
	p.mu.Unlock()
	if changed && key != "" {
		emitEvent("", "proxy-price-invalid", "medium", "claude-proxy-prices: ignored bad keys or values (built-in default kept): "+clip(key, 300))
	}
}
