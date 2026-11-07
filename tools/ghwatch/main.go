package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// Usage:
//
//	ghwatch -repo owner/name -owner login -state FILE            one poll: fetch, reduce, save, print events
//	ghwatch ... -replay SNAPSHOT.json                              reduce a saved snapshot instead of fetching
//	ghwatch ... -dump-snapshot FILE                                also save the fetched snapshot (for tests)
//	ghwatch ... -status                                            print the tracked open PRs and issues
//
// Output: one JSON event per line (-format json, the default) or one human line per event (-format text).
// Exit code: 0 ok (with or without events), 1 error (state is not changed), 2 usage.
func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ghwatch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", os.Getenv("GITHUB_REPO"), "owner/name")
	owner := fs.String("owner", os.Getenv("GITHUB_OWNER"), "login whose reviews decide merges")
	statePath := fs.String("state", "", "state file")
	replay := fs.String("replay", "", "snapshot file to reduce instead of fetching")
	dump := fs.String("dump-snapshot", "", "write the fetched snapshot here")
	format := fs.String("format", "json", "json | text")
	status := fs.Bool("status", false, "print the tracked state and exit")
	pending := fs.Bool("pending", false, "print owner events not yet acknowledged (from the outbox) and exit")
	ack := fs.Bool("ack", false, "with -pending: mark the printed events as acknowledged")
	gapAfter := fs.Duration("gap-after", 5*time.Minute, "report a gap when the last poll is older than this")
	api := fs.String("api", "https://api.github.com", "GitHub API base URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *repo == "" || *owner == "" || *statePath == "" || (*format != "json" && *format != "text") {
		fmt.Fprintln(stderr, "ghwatch: -repo, -owner and -state are required; -format is json or text")
		return 2
	}
	st, err := loadState(*statePath, *repo, *owner)
	if err != nil {
		fmt.Fprintln(stderr, "ghwatch:", err)
		return 1
	}
	if *status {
		printStatus(stdout, st)
		return 0
	}
	if *pending {
		if err := printPending(stdout, *statePath, *ack); err != nil {
			fmt.Fprintln(stderr, "ghwatch: pending:", err)
			return 1
		}
		return 0
	}

	var snap Snapshot
	if *replay != "" {
		b, err := os.ReadFile(*replay)
		if err == nil {
			err = json.Unmarshal(b, &snap)
		}
		if err != nil {
			fmt.Fprintln(stderr, "ghwatch: replay:", err)
			return 1
		}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		c := &client{base: *api, repo: *repo, token: os.Getenv("GH_TOKEN"), http: &http.Client{Timeout: 30 * time.Second}}
		snap, err = fetch(ctx, c, st.Cursor, time.Now())
		if err != nil {
			fmt.Fprintln(stderr, "ghwatch: fetch:", err) // partial snapshots are never reduced
			return 1
		}
		if *dump != "" {
			b, _ := json.MarshalIndent(snap, "", " ")
			if err := os.WriteFile(*dump, b, 0o600); err != nil {
				fmt.Fprintln(stderr, "ghwatch: dump:", err)
			}
		}
	}

	baseline := st.Cursor == ""
	gap := ""
	if !baseline && *replay == "" {
		if t, err := time.Parse(time.RFC3339, st.Cursor); err == nil && time.Since(t) > *gapAfter {
			gap = time.Since(t).Round(time.Minute).String()
		}
	}
	next, events := Reduce(st, snap)
	if gap != "" {
		events = append([]Event{{Kind: EvGap, Owner: true, To: gap,
			Text: fmt.Sprintf("monitor was down for %s (last poll %s); events below cover the gap", gap, st.Cursor)}}, events...)
	}
	// #242: the outbox is written before the state. A crash between the two repeats events (at-least-once),
	// it never loses them.
	if err := appendOutbox(outboxPath(*statePath), snap.At, events); err != nil {
		fmt.Fprintln(stderr, "ghwatch: outbox:", err)
		return 1
	}
	if err := saveState(*statePath, next); err != nil {
		fmt.Fprintln(stderr, "ghwatch: save:", err)
		return 1
	}
	if baseline {
		fmt.Fprintf(stderr, "ghwatch: baseline recorded: %d PRs, %d issues, %d comments\n", len(next.PRs), len(next.Issues), len(next.CommentIDs))
	}
	enc := json.NewEncoder(stdout)
	for _, e := range events {
		if *format == "text" {
			mark := " "
			if e.Owner {
				mark = "*"
			}
			fmt.Fprintf(stdout, "%s %-16s %s\n", mark, e.Kind, e.Text)
			continue
		}
		_ = enc.Encode(e)
	}
	return 0
}

func printStatus(w io.Writer, s State) {
	fmt.Fprintf(w, "%s (owner %s), cursor %s\n", s.Repo, s.Owner, s.Cursor)
	for _, k := range sortedKeys(s.PRs) {
		p := s.PRs[k]
		if p.Lifecycle != PROpen && p.Lifecycle != PRDraft {
			continue
		}
		fmt.Fprintf(w, "PR #%s %-5s head=%s checks=%s review=%s mergeable=%s ready=%t  %s\n", k, p.Lifecycle, short(p.HeadSHA), p.Checks, p.Review, p.Mergeable, p.MergeReady, p.Title)
	}
	for _, k := range sortedKeys(s.Issues) {
		i := s.Issues[k]
		if i.Lifecycle != IssueOpen {
			continue
		}
		fmt.Fprintf(w, "issue #%s [%s] %v  %s\n", k, i.Milestone, i.Assignees, i.Title)
	}
}

// sortedKeys orders numeric keys by value, so #9 comes before #10.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && less(keys[j], keys[j-1]); j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func less(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}
