package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"time"
)

// Per-agent Claude budget (#66): the credential proxy counts tokens per agent and serves them on GET /status (no
// credentials there). The API adds them to GET /api/agents for the UI. Unset CLAUDE_PROXY_STATUS_URL or an
// unreachable proxy: no budget fields, the list still works.

type budgetEntry struct {
	Used  int64 `json:"used"`
	Limit int64 `json:"limit"`
}

type budgetSource func(ctx context.Context) map[string]budgetEntry

var budgets budgetSource = proxyBudgets(os.Getenv("CLAUDE_PROXY_STATUS_URL"), &http.Client{Timeout: 2 * time.Second})

func proxyBudgets(url string, c *http.Client) budgetSource {
	return func(ctx context.Context) map[string]budgetEntry {
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
		var st struct {
			Budgets []struct {
				Agent string `json:"agent"`
				Used  int64  `json:"used"`
				Limit int64  `json:"limit"`
			} `json:"budgets"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st) != nil {
			return nil
		}
		out := map[string]budgetEntry{}
		for _, b := range st.Budgets {
			if b.Used >= 0 && b.Limit >= 0 {
				out[b.Agent] = budgetEntry{b.Used, b.Limit}
			}
		}
		return out
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
			list[i].BudgetUsed, list[i].BudgetLimit = &b.Used, &b.Limit
		}
	}
}
