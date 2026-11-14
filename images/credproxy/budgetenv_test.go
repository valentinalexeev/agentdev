package main

import (
	"strings"
	"testing"
)

func TestBudgetFromEnv(t *testing.T) {
	cases := []struct {
		units, tokens string
		want          int64
		warn          string // substring; "" = no warning
		bad           bool
	}{
		{"", "", 0, "", false},
		{"500", "", 500, "", false},
		{"", "700", 700, "use CLAUDE_BUDGET_UNITS", false}, // old only: 1 token = 1 unit
		{"500", "700", 500, "ignored", false},              // new wins
		{"", "-1", 0, "deprecated", true},                  // old value still validated
		{"x", "", 0, "", true},
	}
	for _, c := range cases {
		env := map[string]string{"CLAUDE_BUDGET_UNITS": c.units, "CLAUDE_BUDGET_TOKENS": c.tokens}
		got, warn, err := budgetFromEnv(func(k string) string { return env[k] })
		if (err != nil) != c.bad || (!c.bad && got != c.want) {
			t.Errorf("%+v: got %d, %v", c, got, err)
		}
		if c.warn == "" && warn != "" || c.warn != "" && !(strings.Contains(warn, c.warn) && strings.Contains(warn, "2.0.0")) {
			t.Errorf("%+v: warning %q", c, warn)
		}
	}
}
