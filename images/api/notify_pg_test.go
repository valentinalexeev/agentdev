package main

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestPgNotify needs TEST_DATABASE_URL as a superuser (like TestPgAudit): the notification tables (#347) as the API
// login, with the rights of grants.sql only.
func TestPgNotify(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	su, err := newPgStore(ctx, url, false)
	if err != nil {
		t.Fatal(err)
	}
	setup := `DROP TABLE IF EXISTS notify_users, push_subscriptions, notify_settings, push_action_nonces CASCADE;
		DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='agentdev_owner') THEN CREATE ROLE agentdev_owner NOLOGIN; END IF;
		IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='agentdev_api') THEN CREATE ROLE agentdev_api NOLOGIN; END IF; END $$;
		GRANT CREATE, USAGE ON SCHEMA public TO agentdev_owner;
		SET ROLE agentdev_owner;` + schemaSQL + schemaSQL + `RESET ROLE;` + grantsSQL
	if _, err := su.db.Exec(ctx, setup); err != nil {
		t.Fatal(err)
	}
	// the API login, with the rights of grants.sql only
	if _, err := su.db.Exec(ctx, `ALTER ROLE agentdev_api LOGIN PASSWORD 'notifytest'`); err != nil {
		t.Fatal(err)
	}
	apiURL := regexp.MustCompile(`://[^@]*@`).ReplaceAllString(url, "://agentdev_api:notifytest@")
	s, err := newPgStore(ctx, apiURL, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveUser(ctx, userSnap{Subject: "s1", Username: "wendy", ForgeLogin: "wendy"}); err != nil {
		t.Fatal(err)
	}
	if u, ok, err := s.User(ctx, "s1"); err != nil || !ok || u.ForgeLogin != "wendy" {
		t.Fatalf("user: %+v %v %v", u, ok, err)
	}
	if _, ok, err := s.User(ctx, "nobody"); err != nil || ok {
		t.Fatalf("missing user: %v %v", ok, err)
	}
	for i := range 3 {
		err := s.AddSubscription(ctx, pushSub{Subject: "s1", Endpoint: "https://fcm.googleapis.com/x" + string(rune('a'+i)), P256dh: "p", Auth: "a"}, 2)
		if (i < 2) != (err == nil) {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	// the same endpoint by another user moves it
	if err := s.AddSubscription(ctx, pushSub{Subject: "s2", Endpoint: "https://fcm.googleapis.com/xa", P256dh: "p", Auth: "a"}, 2); err != nil {
		t.Fatal(err)
	}
	if subs, _ := s.Subscriptions(ctx, "s1"); len(subs) != 1 {
		t.Fatalf("s1 subs %d", len(subs))
	}
	if subj, _ := s.Subscribers(ctx); strings.Join(subj, ",") != "s1,s2" {
		t.Fatalf("subscribers %v", subj)
	}
	if err := s.DeleteEndpoint(ctx, "https://fcm.googleapis.com/xa"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.DeleteSubscription(ctx, "s2", "https://fcm.googleapis.com/xb"); ok {
		t.Fatal("removed another user's subscription")
	}
	in := notifySettings{Types: map[string]bool{"usage": false}, Mutes: []notifyMute{{"owner/x", "*"}, {"owner/y", "approvals"}}}
	if err := s.SaveSettings(ctx, "s1", in); err != nil {
		t.Fatal(err)
	}
	out, err := s.Settings(ctx, "s1")
	if err != nil || out.Types["usage"] || len(out.Mutes) != 2 || out.wants("stopped", "owner/x") || !out.wants("stopped", "owner/y") {
		t.Fatalf("settings %+v %v", out, err)
	}
	n := strings.Repeat("ab", 16)
	if ok, err := s.UseNonce(ctx, n, time.Now().Add(time.Minute)); err != nil || !ok {
		t.Fatalf("nonce: %v %v", ok, err)
	}
	if ok, _ := s.UseNonce(ctx, n, time.Now().Add(time.Minute)); ok {
		t.Fatal("nonce replay accepted")
	}
}
