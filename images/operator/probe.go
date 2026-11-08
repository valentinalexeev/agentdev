package main

// Probe catalogue (#205, ADR 0003 D6). Agents never get SSH. They ask for one of the checks an admin listed in
// Executor spec.probes through the MCP tool server_check(target, check, args) of obs-mcp; obs-mcp forwards the call
// to this server in the operator (PROBE_LISTEN, only obs-mcp may connect: NetworkPolicy and a bearer token file).
//
// For each call the operator
//
//  1. rate-limits (all callers together, and per target),
//  2. finds the Ready SSHTarget, its Executor (spec.ssh, valid, not paused) and the check by name; an unknown check
//     is refused,
//  3. checks every argument: the value charset [A-Za-z0-9._@:=/+-], 1..128 characters, no leading '-', and the
//     check's anchored pattern,
//  4. for a check marked sensitive: requests an Approval (source executor, tool probe) on the executor's probe issue
//     and runs only once a person allowed exactly this call (single use, bound by the run that used it),
//  5. records an ExecutorRun (stage probe) and a SecurityEvent, makes a fresh key with a 5-minute certificate
//     (principal agentdev-probe, force-command /usr/local/sbin/agentdev-probe, key ID check/<run>) and sends
//     "check <name> <values...>" as the SSH command. The server's agentdev-probe runs the check only if its own
//     allow-list (/etc/agentdev/probes, written by scripts/ssh-enrol.sh) has it, checks the arguments again, and
//     runs it as the check user (no sudo) under timeout, output capped.
//  6. returns the output clipped and redacted. It is untrusted data for the model, never a control decision.
//
// snapshot and drift (GET) return the published snapshot and drift report of an executor, for the same tool server.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"golang.org/x/time/rate"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

const (
	probeCertTTL    = 5 * time.Minute
	probeMaxOutput  = 64 << 10
	probeTimeout    = 45 * time.Second // the server's own timeout is 20 s
	probeApprovalTT = time.Hour
)

var (
	probeValueRE = regexp.MustCompile(`^[A-Za-z0-9._@:=/+-]{1,128}$`)
	probeNameRE  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	probeArgRE   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,30}$`)
	probeElemRE  = regexp.MustCompile(`^[A-Za-z0-9._@:=/+,{}%-]+$`)
	probeHoleRE  = regexp.MustCompile(`^\{([a-z][a-z0-9_]{0,30})\}$`)
)

// validProbes repeats the CRD rules of spec.probes and checks what the CRD cannot: patterns compile, every
// placeholder is an argument and every argument is used, braces only as a whole placeholder.
func validProbes(e *v1.Executor) string {
	if len(e.Spec.Probes) > 0 && !e.Spec.SSH {
		return "spec.probes needs spec.ssh"
	}
	seen := map[string]bool{}
	for _, pr := range e.Spec.Probes {
		if !probeNameRE.MatchString(pr.Name) || len(pr.Name) > 40 || seen[pr.Name] {
			return "spec.probes: invalid or duplicate name " + clip(pr.Name, 40)
		}
		seen[pr.Name] = true
		if len(pr.Command) == 0 || len(pr.Command) > 16 || len(pr.Args) > 4 {
			return "spec.probes[" + pr.Name + "]: 1..16 command elements, at most 4 args"
		}
		args := map[string]bool{}
		for _, a := range pr.Args {
			if !probeArgRE.MatchString(a.Name) || args[a.Name] {
				return "spec.probes[" + pr.Name + "]: invalid or duplicate arg name"
			}
			if _, err := regexp.Compile("^(?:" + a.Pattern + ")$"); err != nil || a.Pattern == "" || len(a.Pattern) > 200 {
				return "spec.probes[" + pr.Name + "]: invalid pattern of arg " + a.Name
			}
			args[a.Name] = true
		}
		used := map[string]bool{}
		for i, c := range pr.Command {
			if !probeElemRE.MatchString(c) || len(c) > 200 {
				return "spec.probes[" + pr.Name + "]: invalid command element"
			}
			if m := probeHoleRE.FindStringSubmatch(c); m != nil {
				if i == 0 || !args[m[1]] {
					return "spec.probes[" + pr.Name + "]: placeholder " + c + " is not an argument (or is the program)"
				}
				used[m[1]] = true
			} else if strings.ContainsAny(c, "{}") {
				return "spec.probes[" + pr.Name + "]: braces only as a whole placeholder element"
			}
		}
		if len(used) != len(args) {
			return "spec.probes[" + pr.Name + "]: every arg must be used in the command"
		}
	}
	return ""
}

// probeArgv checks the values against the check and returns the argv the server will run (for the record).
func probeArgv(pr *v1.ExecutorProbe, values []string) ([]string, error) {
	if len(values) != len(pr.Args) {
		return nil, fmt.Errorf("check %s takes %d argument(s), got %d", pr.Name, len(pr.Args), len(values))
	}
	byName := map[string]string{}
	for i, a := range pr.Args {
		v := values[i]
		if !probeValueRE.MatchString(v) || strings.HasPrefix(v, "-") {
			return nil, fmt.Errorf("argument %s: only [A-Za-z0-9._@:=/+-], 1..128 characters, not starting with '-'", a.Name)
		}
		re, err := regexp.Compile("^(?:" + a.Pattern + ")$")
		if err != nil || !re.MatchString(v) {
			return nil, fmt.Errorf("argument %s does not match its pattern %s", a.Name, a.Pattern)
		}
		byName[a.Name] = v
	}
	argv := make([]string, 0, len(pr.Command))
	for _, c := range pr.Command {
		if m := probeHoleRE.FindStringSubmatch(c); m != nil {
			c = byName[m[1]]
		}
		argv = append(argv, c)
	}
	return argv, nil
}

// ProbeServer serves the catalogue to obs-mcp. It shares the executor poller's clients, CA and forge.
type ProbeServer struct {
	P         *ExecutorPoller
	Token     []byte // bearer token obs-mcp sends; empty: TokenFile is read per request (rotation, startup order)
	TokenFile string
	Dial      func(ctx context.Context, network, addr string) (net.Conn, error)
	// Run runs the SSH command; replaced in tests. Default: sshProbeRun.
	Run    func(ctx context.Context, ca *SSHCA, t *v1.SSHTarget, keyID, command string) (string, error)
	mu     sync.Mutex
	global *rate.Limiter
	perTgt map[string]*rate.Limiter
	rate   float64 // calls per second, all targets; one target gets half
	burst  int
}

func newProbeServer(p *ExecutorPoller, token []byte, perMin, burst int) *ProbeServer {
	return &ProbeServer{P: p, Token: token, global: rate.NewLimiter(rate.Limit(float64(perMin)/60), burst),
		perTgt: map[string]*rate.Limiter{}, rate: float64(perMin) / 60, burst: burst}
}

func (s *ProbeServer) allow(target string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.perTgt[target]
	if !ok {
		l = rate.NewLimiter(rate.Limit(s.rate/2), max(1, s.burst))
		s.perTgt[target] = l
	}
	return l.Allow() && s.global.Allow()
}

type probeRequest struct {
	Target string   `json:"target"`
	Check  string   `json:"check"`
	Args   []string `json:"args"`
}

// probeAnswer: Status ok | refused | pending | error; Output is the server's output (redacted, clipped).
type probeAnswer struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	Output  string `json:"output,omitempty"`
	Run     string `json:"run,omitempty"`
}

func (s *ProbeServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			want := s.Token
			if len(want) == 0 && s.TokenFile != "" {
				if b, err := os.ReadFile(s.TokenFile); err == nil {
					want = []byte(strings.TrimSpace(string(b)))
				}
			}
			if len(want) < 32 || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/v1/check", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req probeRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		ans := s.Check(r.Context(), req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ans)
	}))
	mux.HandleFunc("/v1/catalogue", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.Catalogue(r.Context()))
	}))
	// published snapshot or drift report of one executor: /v1/report?kind=snapshot|drift&executor=<name>
	mux.HandleFunc("/v1/report", auth(func(w http.ResponseWriter, r *http.Request) {
		kind, ex := r.URL.Query().Get("kind"), r.URL.Query().Get("executor")
		cm := map[string]string{"snapshot": snapshotConfigMap, "drift": driftConfigMap}[kind]
		if cm == "" || !probeNameRE.MatchString(ex) || len(ex) > 40 {
			http.Error(w, "kind snapshot|drift and an executor name", http.StatusBadRequest)
			return
		}
		var c corev1.ConfigMap
		if err := s.P.Direct.Get(r.Context(), client.ObjectKey{Namespace: s.P.C.AgentsNS, Name: cm}, &c); err != nil || c.Data[ex] == "" {
			http.Error(w, "no "+kind+" of executor "+ex+" yet", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, c.Data[ex])
	}))
	return mux
}

// catalogueEntry is what an agent sees of a check (never the command template of a sensitive check's arguments only).
type catalogueEntry struct {
	Target      string   `json:"target"`
	Executor    string   `json:"executor"`
	Check       string   `json:"check"`
	Description string   `json:"description,omitempty"`
	Args        []string `json:"args,omitempty"` // name=pattern
	Sensitive   bool     `json:"sensitive,omitempty"`
}

// Catalogue lists the checks of every Ready target.
func (s *ProbeServer) Catalogue(ctx context.Context) []catalogueEntry {
	var ts v1.SSHTargetList
	if err := s.P.Direct.List(ctx, &ts, client.InNamespace(s.P.C.NS)); err != nil {
		return nil
	}
	var out []catalogueEntry
	for _, t := range ts.Items {
		if t.Status.Phase != "Ready" {
			continue
		}
		var e v1.Executor
		if err := s.P.Direct.Get(ctx, client.ObjectKey{Namespace: s.P.C.NS, Name: t.Spec.Executor}, &e); err != nil ||
			!e.Spec.SSH || e.Spec.Paused || validExecutor(&e) != "" {
			continue
		}
		for _, pr := range e.Spec.Probes {
			c := catalogueEntry{Target: t.Name, Executor: e.Name, Check: pr.Name, Description: pr.Description, Sensitive: pr.Sensitive}
			for _, a := range pr.Args {
				c.Args = append(c.Args, a.Name+"="+a.Pattern)
			}
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Target+out[i].Check < out[j].Target+out[j].Check })
	return out
}

// refuse records the refusal (SecurityEvent, medium) and answers it.
func (s *ProbeServer) refuse(ctx context.Context, e *v1.Executor, req probeRequest, msg string) probeAnswer {
	log.FromContext(ctx).Info("probe refused", "target", req.Target, "check", req.Check, "reason", msg)
	if e != nil {
		action := fmt.Sprintf("refused server_check %s on %s: %s", clip(req.Check, 40), clip(req.Target, 63), msg)
		if err := s.P.record(ctx, e, "", "probe", "executor.probe-refused", "medium", action); err != nil {
			log.FromContext(ctx).Error(err, "probe event")
		}
	}
	return probeAnswer{Status: "refused", Message: msg}
}

// Check runs one catalogue check (the flow at the top of this file).
func (s *ProbeServer) Check(ctx context.Context, req probeRequest) probeAnswer {
	if !probeNameRE.MatchString(req.Target) || len(req.Target) > 63 || !probeNameRE.MatchString(req.Check) || len(req.Check) > 40 || len(req.Args) > 4 {
		return probeAnswer{Status: "refused", Message: "target and check are names ([a-z0-9-]), at most 4 args"}
	}
	if !s.allow(req.Target) {
		return probeAnswer{Status: "refused", Message: "rate limit: try again in a minute"}
	}
	var t v1.SSHTarget
	if err := s.P.Direct.Get(ctx, client.ObjectKey{Namespace: s.P.C.NS, Name: req.Target}, &t); err != nil {
		return probeAnswer{Status: "refused", Message: "no such target (server_checks lists them)"}
	}
	var e v1.Executor
	if err := s.P.Direct.Get(ctx, client.ObjectKey{Namespace: s.P.C.NS, Name: t.Spec.Executor}, &e); err != nil {
		return probeAnswer{Status: "refused", Message: "the target has no executor"}
	}
	switch {
	case t.Status.Phase != "Ready" || t.Status.HostCAKey == "" || !sshHostRE.MatchString(t.Spec.Host):
		return s.refuse(ctx, &e, req, "target is not Ready")
	case !e.Spec.SSH || e.Spec.Paused || validExecutor(&e) != "":
		return s.refuse(ctx, &e, req, "the executor of the target is paused or invalid")
	}
	var pr *v1.ExecutorProbe
	for i := range e.Spec.Probes {
		if e.Spec.Probes[i].Name == req.Check {
			pr = &e.Spec.Probes[i]
		}
	}
	if pr == nil {
		return s.refuse(ctx, &e, req, "check "+req.Check+" is not in the catalogue of executor "+e.Name)
	}
	argv, err := probeArgv(pr, req.Args)
	if err != nil {
		return s.refuse(ctx, &e, req, err.Error())
	}
	approval := ""
	if pr.Sensitive {
		name, ans := s.sensitive(ctx, &e, &t, pr, req)
		if name == "" {
			return ans
		}
		approval = name
	}
	return s.run(ctx, &e, &t, pr, req, argv, approval)
}

// probeInput is what a person approves for a sensitive check.
func probeInput(e *v1.Executor, t *v1.SSHTarget, pr *v1.ExecutorProbe, argv []string) string {
	b, _ := json.Marshal(argv)
	return fmt.Sprintf("probe %s of executor %s on server %s (%s) as %s: argv %s", pr.Name, e.Name, t.Name, t.Spec.Host,
		checkUser(t), clip(string(b), 3000))
}

// sensitive returns the name of an allowed, unused Approval of exactly this call, or requests one.
func (s *ProbeServer) sensitive(ctx context.Context, e *v1.Executor, t *v1.SSHTarget, pr *v1.ExecutorProbe, req probeRequest) (string, probeAnswer) {
	argv, _ := probeArgv(pr, req.Args)
	input := probeInput(e, t, pr, argv)
	var list v1.ApprovalList
	if err := s.P.Direct.List(ctx, &list, client.InNamespace(s.P.C.AgentsNS),
		client.MatchingLabels{"agent": e.Name, "agentdev/source": "executor", "agentdev/tool": "probe"}); err != nil {
		return "", probeAnswer{Status: "error", Message: "approvals: " + err.Error()}
	}
	now := s.P.Now()
	pending := ""
	for _, a := range list.Items {
		if a.Spec.Input != input || a.Spec.Source != "executor" || a.Spec.Tool != "probe" || a.Spec.Agent != e.Name ||
			a.Spec.ContentHash != approvalHash(a.Spec.ID, a.Spec.Agent, a.Spec.Repo, a.Spec.Job, a.Spec.Source, a.Spec.Tool, a.Spec.Input) {
			continue
		}
		switch {
		case a.Status.State == "allowed" && a.Status.ContentHash == a.Spec.ContentHash && a.Status.DecidedAt != nil &&
			!a.Status.DecidedAt.After(a.Spec.Expires.Time) && now.Before(a.Status.DecidedAt.Add(probeApprovalTT)):
			used, err := s.approvalUsed(ctx, a.Name)
			if err != nil {
				return "", probeAnswer{Status: "error", Message: err.Error()}
			}
			if !used {
				return a.Name, probeAnswer{}
			}
		case a.Status.State == "" && now.Before(a.Spec.Expires.Time):
			pending = a.Spec.ID
		}
	}
	if pending != "" {
		return "", probeAnswer{Status: "pending", Message: "approval " + pending + " is still pending on issue #" +
			fmt.Sprint(e.Status.ProbeIssue) + "; call again after a person approved it"}
	}
	id, err := s.requestProbeApproval(ctx, e, input)
	if err != nil {
		return "", probeAnswer{Status: "error", Message: "approval request: " + err.Error()}
	}
	return "", probeAnswer{Status: "pending", Message: fmt.Sprintf("check %s is sensitive: approval %s requested on issue "+
		"#%d of %s. Call server_check again with the same arguments after a person approved it (valid once, for %s).",
		pr.Name, id, e.Status.ProbeIssue, e.Spec.Repo, probeApprovalTT)}
}

// approvalUsed: a probe run that used the approval exists (single use).
func (s *ProbeServer) approvalUsed(ctx context.Context, approval string) (bool, error) {
	var runs v1.ExecutorRunList
	if err := s.P.Direct.List(ctx, &runs, client.InNamespace(s.P.C.NS), client.MatchingLabels{"agentdev/stage": "probe"}); err != nil {
		return false, err
	}
	for _, r := range runs.Items {
		if r.Spec.Stage == "probe" && r.Labels["agentdev/approval"] == approval {
			return true, nil
		}
	}
	return false, nil
}

// requestProbeApproval creates the Approval on the executor's probe issue (opened once).
func (s *ProbeServer) requestProbeApproval(ctx context.Context, e *v1.Executor, input string) (string, error) {
	var sec corev1.Secret
	if err := s.P.Direct.Get(ctx, types.NamespacedName{Namespace: s.P.C.NS, Name: e.Spec.ForgeSecret}, &sec); err != nil {
		return "", err
	}
	f, err := s.P.Forge(e, strings.TrimSpace(string(sec.Data["FORGE_TOKEN"])))
	if err != nil {
		return "", err
	}
	if open, _ := f.IssueOpen(ctx, e.Status.ProbeIssue); !open {
		n, err := f.OpenIssue(ctx, "Server check approvals: executor "+e.Name+" ("+e.Spec.Environment+")",
			"🤖 agentdev executor `"+e.Name+"`: agents ask here to run server checks marked `sensitive` (#205). "+
				"Each request names the check, the server and the exact argv. Approve with `/approve <id>`, deny with "+
				"`/deny <id>`. An approval allows one run within "+probeApprovalTT.String()+".")
		if err != nil {
			return "", err
		}
		e.Status.ProbeIssue = n
		if err := s.P.Status().Update(ctx, e); err != nil {
			return "", err
		}
	}
	self, err := f.SelfID(ctx)
	if err != nil {
		return "", err
	}
	var rnd [8]byte
	var secret [32]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	if _, err := rand.Read(secret[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(rnd[:])
	sh := sha256.Sum256([]byte(hex.EncodeToString(secret[:])))
	now := s.P.Now().UTC().Truncate(time.Second)
	spec := v1.ApprovalSpec{ID: id, Agent: e.Name, Repo: e.Spec.Repo, Forge: forgeOf(e), Job: fmt.Sprintf("issue-%d", e.Status.ProbeIssue),
		Source: "executor", Tool: "probe", Input: input, RequesterID: self, SecretHash: hex.EncodeToString(sh[:]),
		Created: metav1.NewTime(now), Expires: metav1.NewTime(now.Add(probeApprovalTT))}
	spec.ContentHash = approvalHash(spec.ID, spec.Agent, spec.Repo, spec.Job, spec.Source, spec.Tool, spec.Input)
	a := &v1.Approval{ObjectMeta: metav1.ObjectMeta{Name: "exec-" + id, Namespace: s.P.C.AgentsNS,
		Labels: map[string]string{"agent": e.Name, "agentdev/source": "executor", "agentdev/tool": "probe"}}, Spec: spec}
	if err := s.P.Create(ctx, a); err != nil {
		return "", err
	}
	body := fmt.Sprintf("🤖 agentdev: an agent asks to run a sensitive server check.\n\n%s\n\nContent sha256 `%s`. "+
		"A person with write access replies `/approve %s` (or `/deny %s`) within %s.", quoteOutput(input), spec.ContentHash, id, id, probeApprovalTT)
	if err := f.Comment(ctx, e.Status.ProbeIssue, body); err != nil {
		log.FromContext(ctx).Error(err, "comment probe approval")
	}
	return id, nil
}

// run records the call, runs it over SSH and returns the output.
func (s *ProbeServer) run(ctx context.Context, e *v1.Executor, t *v1.SSHTarget, pr *v1.ExecutorProbe, req probeRequest, argv []string, approval string) probeAnswer {
	if s.P.SSHCA == nil {
		return probeAnswer{Status: "error", Message: "the SSH CA is not configured"}
	}
	var rnd [5]byte
	_, _ = rand.Read(rnd[:])
	labels := map[string]string{"agentdev/executor": e.Name, "agentdev/stage": "probe"}
	if approval != "" {
		labels["agentdev/approval"] = approval
	}
	r := &v1.ExecutorRun{ObjectMeta: metav1.ObjectMeta{Name: e.Name + "-probe-" + hex.EncodeToString(rnd[:]), Namespace: s.P.C.NS,
		Labels: labels}, Spec: v1.ExecutorRunSpec{Executor: e.Name, Action: "probe", Stage: "probe", Target: t.Name, Check: pr.Name,
		Args: req.Args, Approval: approval}}
	if err := controllerutil.SetControllerReference(e, r, s.P.Scheme()); err != nil {
		return probeAnswer{Status: "error", Message: err.Error()}
	}
	if err := s.P.Create(ctx, r); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return probeAnswer{Status: "error", Message: "try again"}
		}
		return probeAnswer{Status: "error", Message: "record: " + err.Error()}
	}
	now := metav1.NewTime(s.P.Now())
	r.Status.Phase, r.Status.StartedAt = "Running", &now
	_ = s.P.Status().Update(ctx, r)
	action := fmt.Sprintf("server_check %s on %s: argv %s", pr.Name, t.Name, clip(strings.Join(argv, " "), 300))
	if err := s.P.record(ctx, e, "", r.Name, "executor.probe", map[bool]string{true: "medium", false: "low"}[pr.Sensitive], action); err != nil {
		log.FromContext(ctx).Error(err, "probe event")
	}
	ca, err := s.P.SSHCA(ctx)
	if err == nil {
		cctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		run := s.Run
		if run == nil {
			run = s.sshProbeRun
		}
		var out string
		out, err = run(cctx, ca, t, "check/"+r.Name, "check "+strings.Join(append([]string{pr.Name}, req.Args...), " "))
		if err == nil {
			s.finishRun(ctx, r, "Succeeded", "ok")
			return probeAnswer{Status: "ok", Output: redactSnapshot(clip(out, probeMaxOutput)), Run: r.Name}
		}
	}
	s.finishRun(ctx, r, "Failed", clip(err.Error(), 300))
	return probeAnswer{Status: "error", Message: "the check failed: " + clip(err.Error(), 300), Run: r.Name}
}

func (s *ProbeServer) finishRun(ctx context.Context, r *v1.ExecutorRun, phase, msg string) {
	now := metav1.NewTime(s.P.Now())
	r.Status.Phase, r.Status.Message, r.Status.FinishedAt = phase, msg, &now
	if err := s.P.Status().Update(ctx, r); err != nil {
		log.FromContext(ctx).Error(err, "probe run status")
	}
}

// sshProbeRun connects as the check user with a fresh key and a 5-minute agentdev-probe certificate (force-command)
// and runs command; the server accepts only a host certificate of the host CA for spec.host.
func (s *ProbeServer) sshProbeRun(ctx context.Context, ca *SSHCA, t *v1.SSHTarget, keyID, command string) (string, error) {
	hostCA, _, _, _, err := ssh.ParseAuthorizedKey([]byte(t.Status.HostCAKey))
	if err != nil {
		return "", fmt.Errorf("host CA key: %w", err)
	}
	_, signer, cert, err := newCertKey(ctx, ca, SSHCertRequest{Type: "user", KeyID: keyID,
		Principals: []string{sshProbePrincipal}, Validity: probeCertTTL,
		CriticalOptions: map[string]string{"force-command": sshProbeCommand}})
	if err != nil {
		return "", err
	}
	en := &SSHEnroller{Dial: s.Dial, Now: s.P.Now}
	c, err := en.sshSession(ctx, en.addr(t), t.Spec.Host, checkUser(t), signer, cert, hostCA)
	if err != nil {
		return "", err
	}
	defer c.Close()
	sess, err := c.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	var out strings.Builder
	w := &limitWriter{w: &out, n: probeMaxOutput}
	sess.Stdout, sess.Stderr = w, w
	done := make(chan error, 1)
	go func() { done <- sess.Run(command) }()
	select {
	case <-ctx.Done():
		return out.String(), errors.New("timeout")
	case err := <-done:
		var ee *ssh.ExitError
		if errors.As(err, &ee) {
			return out.String(), fmt.Errorf("exit %d: %s", ee.ExitStatus(), clip(strings.TrimSpace(out.String()), 200))
		}
		return out.String(), err
	}
}

// probeServerFromEnv starts the probe server when PROBE_LISTEN is set; the token comes from PROBE_TOKEN_FILE.
func probeServerFromEnv(p *ExecutorPoller) (*ProbeServer, string, error) {
	listen := os.Getenv("PROBE_LISTEN")
	if listen == "" {
		return nil, "", nil
	}
	// the token is read per request: until Secret agentdev-probe-token is mounted, every call is refused (fail closed)
	s := newProbeServer(p, nil, envInt("PROBE_RATE_PER_MIN", 20), envInt("PROBE_RATE_BURST", 5))
	s.TokenFile = env("PROBE_TOKEN_FILE", "/var/run/agentdev-probe/token")
	return s, listen, nil
}

// Start serves until ctx ends (manager.Runnable).
type probeRunnable struct {
	s      *ProbeServer
	listen string
}

func (r probeRunnable) Start(ctx context.Context) error {
	hs := &http.Server{Addr: r.listen, Handler: r.s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = hs.Close()
	}()
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
