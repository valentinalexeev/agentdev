package main

// GCP Secret Manager backend for project secret values (#202 part 4, GKE). The API runs as its own Kubernetes service
// account, bound with Workload Identity to the Google service account agentdev-api-secrets (infra/terraform/secrets.tf).
// That account has a custom role with create, delete and addVersion on secrets, conditioned on the name prefix
// agentdev-ps-: it cannot read a value (no secretmanager.versions.access) and cannot touch any other secret.
// Plain REST with the metadata server token: no Google client library in the API image.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type gcpValues struct {
	project  string
	api      string // https://secretmanager.googleapis.com/v1
	metadata string // http://metadata.google.internal
	hc       *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

func newGCPValues(project string) *gcpValues {
	return &gcpValues{project: project, api: "https://secretmanager.googleapis.com/v1", metadata: "http://metadata.google.internal",
		hc: &http.Client{Timeout: 15 * time.Second}}
}

func (g *gcpValues) accessToken(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.token != "" && time.Now().Before(g.expires) {
		return g.token, nil
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, g.metadata+"/computeMetadata/v1/instance/service-accounts/default/token", nil)
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := g.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var t struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&t) != nil || t.AccessToken == "" {
		return "", fmt.Errorf("metadata token: status %d", resp.StatusCode)
	}
	g.token, g.expires = t.AccessToken, time.Now().Add(time.Duration(t.ExpiresIn-60)*time.Second)
	return g.token, nil
}

// do sends one request; the body never contains anything but the payload, and errors never echo it.
func (g *gcpValues) do(ctx context.Context, method, path string, body any) (int, error) {
	code, _, err := g.doBody(ctx, method, path, body)
	return code, err
}

// doBody is do that also returns up to 64 KiB of the response (metadata only: no call here returns a value).
func (g *gcpValues) doBody(ctx context.Context, method, path string, body any) (int, []byte, error) {
	tok, err := g.accessToken(ctx)
	if err != nil {
		return 0, nil, err
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(ctx, method, g.api+path, rd)
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, out, nil
}

func (g *gcpValues) secretPath(name string) string {
	return "/projects/" + url.PathEscape(g.project) + "/secrets/" + url.PathEscape(remoteKeyPrefix+name)
}

// SetValue creates the secret (automatic replication, labelled) if needed and adds a version. External Secrets Operator
// reads "latest".
func (g *gcpValues) SetValue(ctx context.Context, name, value string) error {
	code, err := g.do(ctx, http.MethodPost, "/projects/"+url.PathEscape(g.project)+"/secrets?secretId="+url.QueryEscape(remoteKeyPrefix+name),
		map[string]any{"replication": map[string]any{"automatic": map[string]any{}},
			"labels": map[string]string{"managed-by": "agentdev-api", "agentdev-projectsecret": name}})
	if err != nil {
		return err
	}
	if code != http.StatusOK && code != http.StatusConflict {
		return fmt.Errorf("create secret: status %d", code)
	}
	code, out, err := g.doBody(ctx, http.MethodPost, g.secretPath(name)+":addVersion",
		map[string]any{"payload": map[string]string{"data": base64.StdEncoding.EncodeToString([]byte(value))}})
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("add version: status %d", code)
	}
	g.destroyPrevious(ctx, name, out)
	return nil
}

// destroyPrevious destroys the version before the one just added, so a rotated credential is not readable any more
// (External Secrets Operator reads latest; review of #274). Best effort: a failure is logged, the rotation stands.
// Each rotation destroys its predecessor, so at most one enabled version exists.
func (g *gcpValues) destroyPrevious(ctx context.Context, name string, added []byte) {
	var v struct{ Name string }
	if json.Unmarshal(added, &v) != nil {
		return
	}
	i := strings.LastIndex(v.Name, "/versions/")
	if i < 0 {
		return
	}
	n, err := strconv.Atoi(v.Name[i+len("/versions/"):])
	if err != nil || n < 2 {
		return
	}
	code, err := g.do(ctx, http.MethodPost, g.secretPath(name)+"/versions/"+strconv.Itoa(n-1)+":destroy", map[string]any{})
	if err != nil || (code != http.StatusOK && code != http.StatusNotFound && code != http.StatusBadRequest) {
		log.Printf("secret %s: previous version %d not destroyed (status %d, %v); destroy it by hand", name, n-1, code, err)
	}
}

// DeleteValue deletes the secret with all its versions.
func (g *gcpValues) DeleteValue(ctx context.Context, name string) error {
	code, err := g.do(ctx, http.MethodDelete, g.secretPath(name), nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK && code != http.StatusNotFound {
		return fmt.Errorf("delete secret: status %d", code)
	}
	return nil
}
