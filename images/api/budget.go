package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// Per-agent Claude budget (#66, #196): the credential proxy counts cost units per agent and per job and serves them
// on GET /status (no credentials there). The API adds them to GET /api/agents and serves the jobs on GET /api/costs
// for the UI. Unset CLAUDE_PROXY_STATUS_URL or an unreachable proxy: no budget fields, the list still works.

type budgetEntry struct {
	Used    int64    `json:"used"`
	Limit   int64    `json:"limit"`
	UsedUSD *float64 `json:"usedUsd,omitempty"`
}

type budgetSource func(ctx context.Context) map[string]budgetEntry

var budgets budgetSource = proxyBudgets(os.Getenv("CLAUDE_PROXY_STATUS_URL"), &http.Client{Timeout: 2 * time.Second})

func proxyBudgets(url string, c *http.Client) budgetSource {
	return func(ctx context.Context) map[string]budgetEntry {
		st := fetchProxyStatus(ctx, url, c)
		if st == nil {
			return nil
		}
		out := map[string]budgetEntry{}
		for _, b := range st.Budgets {
			if b.Used >= 0 && b.Limit >= 0 && (b.UsedUSD == nil || *b.UsedUSD >= 0) {
				out[b.Agent] = budgetEntry{b.Used, b.Limit, b.UsedUSD}
			}
		}
		return out
	}
}

// proxyStatus is GET /status of claude-proxy (images/credproxy). Values are checked before they are shown.
type proxyStatus struct {
	Unit    string `json:"unit"`
	Budgets []struct {
		Agent   string   `json:"agent"`
		Used    int64    `json:"used"`
		Limit   int64    `json:"limit"`
		UsedUSD *float64 `json:"usedUsd"`
	} `json:"budgets"`
	Jobs []jobCost `json:"jobs"`
}

// statusCache keeps the proxy's GET /status for statusTTL: several UI tabs polling must not hit the proxy, whose
// /status takes the jobs lock, on every request (review of #279). A failure is not cached.
var statusCache struct {
	sync.Mutex
	url string
	at  time.Time
	st  *proxyStatus
}

const statusTTL = 15 * time.Second

func fetchProxyStatus(ctx context.Context, url string, c *http.Client) *proxyStatus {
	statusCache.Lock()
	if statusCache.st != nil && statusCache.url == url && time.Since(statusCache.at) < statusTTL {
		st := statusCache.st
		statusCache.Unlock()
		return st
	}
	statusCache.Unlock()
	st := fetchProxyStatusNow(ctx, url, c)
	if st != nil {
		statusCache.Lock()
		statusCache.url, statusCache.at, statusCache.st = url, time.Now(), st
		statusCache.Unlock()
	}
	return st
}

func fetchProxyStatusNow(ctx context.Context, url string, c *http.Client) *proxyStatus {
	{
		if url == "" {
			return nil
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil
		}
		resp, err := c.Do(req)
		if err != nil {
			return nil
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil
		}
		var st proxyStatus
		if json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&st) != nil {
			return nil
		}
		return &st
	}
}

// addBudgets sets the budget fields of the listed agents.
func addBudgets(ctx context.Context, list []agent) {
	if budgets == nil {
		return
	}
	m := budgets(ctx)
	for i := range list {
		if b, ok := m[list[i].Name]; ok {
			list[i].BudgetUsed, list[i].BudgetLimit, list[i].BudgetUSD = &b.Used, &b.Limit, b.UsedUSD
		}
	}
}
