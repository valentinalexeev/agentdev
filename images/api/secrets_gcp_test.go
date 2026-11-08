package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestGCPValues(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	secrets := map[string][]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/computeMetadata/v1/instance/service-accounts/default/token" {
			if r.Header.Get("Metadata-Flavor") != "Google" {
				w.WriteHeader(403)
				return
			}
			io.WriteString(w, `{"access_token":"ya29.test","expires_in":3600}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer ya29.test" {
			w.WriteHeader(401)
			return
		}
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/projects/p/secrets":
			id := r.URL.Query().Get("secretId")
			if _, ok := secrets[id]; ok {
				w.WriteHeader(409)
				return
			}
			secrets[id] = nil
			io.WriteString(w, `{}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, ":addVersion"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/projects/p/secrets/"), ":addVersion")
			var b struct{ Payload struct{ Data string } }
			json.NewDecoder(r.Body).Decode(&b)
			v, _ := base64.StdEncoding.DecodeString(b.Payload.Data)
			secrets[id] = append(secrets[id], string(v))
			io.WriteString(w, `{}`)
		case r.Method == "DELETE":
			id := strings.TrimPrefix(r.URL.Path, "/v1/projects/p/secrets/")
			if _, ok := secrets[id]; !ok {
				w.WriteHeader(404)
				return
			}
			delete(secrets, id)
			io.WriteString(w, `{}`)
		default:
			w.WriteHeader(400)
		}
	}))
	defer srv.Close()
	g := newGCPValues("p")
	g.api, g.metadata = srv.URL+"/v1", srv.URL
	ctx := context.Background()
	if err := g.SetValue(ctx, "npm", "v1-secret"); err != nil {
		t.Fatal(err)
	}
	if err := g.SetValue(ctx, "npm", "v2-secret"); err != nil { // rotation: 409 on create, new version
		t.Fatal(err)
	}
	if got := secrets["agentdev-ps-npm"]; len(got) != 2 || got[1] != "v2-secret" {
		t.Fatalf("versions %v", got)
	}
	if err := g.DeleteValue(ctx, "npm"); err != nil {
		t.Fatal(err)
	}
	if err := g.DeleteValue(ctx, "npm"); err != nil { // already gone
		t.Fatal(err)
	}
	if _, ok := secrets["agentdev-ps-npm"]; ok {
		t.Error("not deleted")
	}
	for _, c := range calls {
		if strings.Contains(c, "access") {
			t.Errorf("the API read a value: %s", c)
		}
	}
}
