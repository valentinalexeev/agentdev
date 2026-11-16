package main

import (
	"context"
	"os"
	"regexp"
	"testing"
	"time"
)

// TestPgJobEvents needs TEST_DATABASE_URL as a superuser (like TestPgNotify): job_events (#425) as the API login, with
// the rights of grants.sql only: insert and read, no update or delete, removal only through job_events_prune().
func TestPgJobEvents(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	su, err := newPgStore(ctx, url, false)
	if err != nil {
		t.Fatal(err)
	}
	setup := `DROP TABLE IF EXISTS job_events CASCADE;
		DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='agentdev_owner') THEN CREATE ROLE agentdev_owner NOLOGIN; END IF;
		IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='agentdev_api') THEN CREATE ROLE agentdev_api NOLOGIN; END IF; END $$;
		GRANT CREATE, USAGE ON SCHEMA public TO agentdev_owner;
		SET ROLE agentdev_owner;` + schemaSQL + schemaSQL + `RESET ROLE;` + grantsSQL
	if _, err := su.db.Exec(ctx, setup); err != nil {
		t.Fatal(err)
	}
	if _, err := su.db.Exec(ctx, `ALTER ROLE agentdev_api LOGIN PASSWORD 'runlogtest'`); err != nil {
		t.Fatal(err)
	}
	s, err := newPgStore(ctx, regexp.MustCompile(`://[^@]*@`).ReplaceAllString(url, "://agentdev_api:runlogtest@"), false)
	if err != nil {
		t.Fatal(err)
	}
	ev := jobEvent{Agent: "alice", Repo: "o/r", Job: "issue-1", Time: time.Now(), Kind: "text", Data: []byte(`{"text":"<img src=x onerror=alert(1)>"}`)}
	if err := s.AppendJobEvents(ctx, []jobEvent{ev, ev}); err != nil {
		t.Fatal(err)
	}
	got, err := s.JobEvents(ctx, "alice", "o/r", "issue-1", 0, 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("read: %v %d", err, len(got))
	}
	if more, _ := s.JobEvents(ctx, "alice", "o/r", "issue-1", got[0].ID, 10); len(more) != 1 {
		t.Fatalf("after: %d", len(more))
	}
	if n, err := s.JobLogBytes(ctx, "alice", "o/r", "issue-1"); err != nil || n == 0 {
		t.Fatalf("bytes: %d %v", n, err)
	}
	for _, q := range []string{`UPDATE job_events SET kind='result'`, `DELETE FROM job_events`, `TRUNCATE job_events`} {
		if _, err := s.db.Exec(ctx, q); err == nil {
			t.Errorf("the API login may run %q", q)
		}
	}
	if err := s.AppendJobEvents(ctx, []jobEvent{{Agent: "alice", Repo: "o/r", Job: "issue-1", Time: time.Now(), Kind: "html", Data: []byte(`{}`)}}); err == nil {
		t.Error("unknown kind stored")
	}
	// retention: a row stored 31 days ago goes with the prune, a fresh one stays; inside 30 days not even the owner deletes
	if _, err := su.db.Exec(ctx, `INSERT INTO job_events (agent, repo, job, time, kind, data, stored) VALUES ('alice','o/r','issue-1', now(), 'text', '{}', now() - interval '31 days')`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PruneJobEvents(ctx); err != nil || n != 1 {
		t.Fatalf("prune: %d %v", n, err)
	}
	if got, _ := s.JobEvents(ctx, "alice", "o/r", "issue-1", 0, 10); len(got) != 2 {
		t.Fatalf("after prune: %d", len(got))
	}
	if _, err := su.db.Exec(ctx, `SET ROLE agentdev_owner; DELETE FROM job_events; RESET ROLE`); err == nil {
		t.Error("rows inside the retention were deleted")
	}
}
