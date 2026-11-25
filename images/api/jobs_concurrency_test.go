package main

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// countingForge tracks forge calls in flight; repos named "bad/*" fail, and calls block until ctx ends when hang is set.
type countingForge struct {
	inFlight, peak, calls atomic.Int32
	delay                 time.Duration
	hang                  bool
}

func (f *countingForge) enter() {
	f.calls.Add(1)
	n := f.inFlight.Add(1)
	for {
		p := f.peak.Load()
		if n <= p || f.peak.CompareAndSwap(p, n) {
			return
		}
	}
}

func (f *countingForge) wait(ctx context.Context) error {
	defer f.inFlight.Add(-1)
	if f.hang {
		<-ctx.Done()
		return ctx.Err()
	}
	select {
	case <-time.After(f.delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *countingForge) Pulls(ctx context.Context, _, repo string) ([]forgePR, error) {
	f.enter()
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	if len(repo) > 4 && repo[:4] == "bad/" {
		return nil, errors.New("forge down")
	}
	return []forgePR{{Number: 1, Title: "pr", Branch: "feature/x", State: "open", Updated: time.Now()}}, nil
}

func (f *countingForge) AgentIssues(ctx context.Context, _, repo, _ string) ([]forgeIssue, error) {
	f.enter()
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	return []forgeIssue{{Number: 1, Title: "issue", Updated: time.Now()}}, nil
}

func manyAgents(n int, bad map[int]bool) []agent {
	var as []agent
	for i := range n {
		owner := "owner"
		if bad[i] {
			owner = "bad"
		}
		as = append(as, agent{Name: fmt.Sprintf("a%d", i), Forge: "gitea", Repos: []string{fmt.Sprintf("%s/r%d", owner, i)}})
	}
	return as
}

// #519: forge calls stay bounded by jobForgeParallel repositories (2 calls each), also across concurrent requests.
func TestJobsOfBoundedConcurrency(t *testing.T) {
	_, wr := detailEnv(t)
	f := &countingForge{delay: 20 * time.Millisecond}
	wr.jobs = newJobCache(f, time.Minute, 10*time.Minute)
	done := make(chan struct{})
	for r := range 3 {
		go func() {
			defer func() { done <- struct{}{} }()
			as := manyAgents(30, nil)
			for i := range as {
				as[i].Name = fmt.Sprintf("req%d-%s", r, as[i].Name) // distinct cache keys per request
			}
			if _, failed := wr.jobsOf(context.Background(), as, func(string, string) bool { return true }); len(failed) != 0 {
				t.Errorf("failed: %v", failed)
			}
		}()
	}
	for range 3 {
		<-done
	}
	if p := f.peak.Load(); p > 2*jobForgeParallel {
		t.Fatalf("peak forge calls in flight %d, limit %d", p, 2*jobForgeParallel)
	}
	if c := f.calls.Load(); c != 180 {
		t.Fatalf("forge calls %d, want 180", c)
	}
}

// #519: one failing repository does not hide the jobs of the others.
func TestJobsOfErrorIsolation(t *testing.T) {
	_, wr := detailEnv(t)
	f := &countingForge{}
	wr.jobs = newJobCache(f, time.Minute, 10*time.Minute)
	js, failed := wr.jobsOf(context.Background(), manyAgents(10, map[int]bool{2: true, 7: true}), func(string, string) bool { return true })
	if len(failed) != 2 || failed[0] != "bad/r2" || failed[1] != "bad/r7" {
		t.Fatalf("failed %v", failed)
	}
	repos := map[string]bool{}
	for _, j := range js {
		repos[j.Repo] = true
	}
	if len(repos) != 8 {
		t.Fatalf("jobs from %d repositories, want 8: %v", len(repos), repos)
	}
}

// #519: a cancelled request returns promptly and stops the forge calls it started.
func TestJobsOfContextCancel(t *testing.T) {
	_, wr := detailEnv(t)
	f := &countingForge{hang: true}
	wr.jobs = newJobCache(f, time.Minute, 10*time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, failed := wr.jobsOf(ctx, manyAgents(20, nil), func(string, string) bool { return true })
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("cancelled jobsOf took %v", d)
	}
	if len(failed) != 20 {
		t.Fatalf("failed %d repositories, want 20", len(failed))
	}
	deadline := time.Now().Add(2 * time.Second)
	for f.inFlight.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d forge calls still in flight", f.inFlight.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
