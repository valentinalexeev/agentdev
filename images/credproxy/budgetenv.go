package main

import (
	"errors"
	"strconv"
)

// budgetFromEnv reads the default budget per agent (#196, owner decision on #196 option 2).
// CLAUDE_BUDGET_UNITS is the variable. CLAUDE_BUDGET_TOKENS is deprecated since 1.2.0 and is removed in 2.0.0:
// when only it is set, its value is used as units (1 token = 1 unit); when both are set, CLAUDE_BUDGET_UNITS wins.
// warn is non-empty when the deprecated variable is set; main logs it once at start.
func budgetFromEnv(getenv func(string) string) (units int64, warn string, err error) {
	name, v := "CLAUDE_BUDGET_UNITS", getenv("CLAUDE_BUDGET_UNITS")
	if old := getenv("CLAUDE_BUDGET_TOKENS"); old != "" {
		if v == "" {
			name, v = "CLAUDE_BUDGET_TOKENS", old
			warn = "CLAUDE_BUDGET_TOKENS is deprecated and will be removed in 2.0.0: use CLAUDE_BUDGET_UNITS " +
				"(the value is used as cost units, 1 token = 1 unit; see the CHANGELOG)"
		} else {
			warn = "CLAUDE_BUDGET_TOKENS is deprecated and will be removed in 2.0.0: it is ignored because " +
				"CLAUDE_BUDGET_UNITS is set; remove it"
		}
	}
	if v == "" {
		v = "0"
	}
	units, perr := strconv.ParseInt(v, 10, 64)
	if perr != nil || units < 0 {
		return 0, warn, errors.New(name + " must be a non-negative integer")
	}
	return units, warn, nil
}
