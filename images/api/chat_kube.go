package main

// Chat session Jobs (#346). The API creates one Job per session in the agents namespace.
//
// #424: the session pod is the agent's own pod. The API reads the pod template the operator rendered for the
// StatefulSet agent-<agent> (images/operator/render.go, the only definition of an agent pod) and changes only what makes
// it interactive (chatPod): the agent container becomes the chat container (agent-chat-session in CHAT_MODE=console,
// the session port 7681, CHAT_CONTROL_SHA256 = the sha256 of the API's per-session token); the forge sidecar stays as
// it is (same image, Secrets, egress proxy key, security context) except that it does not register with the router or
// listen for wakes (no WAKE_TOKEN, REGISTER_TOKEN, SELF_URL, webhook port); /state is an emptyDir instead of the
// agent's disk; the labels are the chat labels (app=agent-chat), so the pod is not in the agent's Service, not
// adopted by its StatefulSet and its approval lines are not read as the agent's (approvals stay the console
// Allow/Deny of the session server, see docs/admin.md). The chat container holds no forge token and no egress key:
// they stay in the sidecar, as in the agent pod, and every forge action goes through it (agentdev-forge).
//
// The ValidatingAdmissionPolicy agentdev-api-chat-jobs (manifests/api.yaml.tmpl) holds the API's service account to
// exactly this shape: chat + forge containers with the worker and forge images, the state-init initContainer, gVisor,
// the service account agent-<agent>, and only that agent's Secrets, each in the container the agent pod has it in.
// The API's Role has no access to Secrets: it reads the StatefulSet (get, as before) and names Secrets in the Job.

import (
	"context"
	"errors"
	"fmt"
	"net"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
)

const chatPort = 7681

// chatJobConfig is read from the environment (CHAT_* in manifests/api.yaml.tmpl).
type chatJobConfig struct {
	Namespace   string
	WorkerImage string // the admission policy pins it; the pod's images come from the agent's StatefulSet
	IdleSeconds int
	MaxSeconds  int64
}

type kubeChat struct {
	c   kubernetes.Interface
	cfg chatJobConfig
}

func ptr[T any](v T) *T { return &v }

func intOrStr(n int) intstr.IntOrString { return intstr.FromInt32(int32(n)) }

// errNoAgentPod: the agent has no StatefulSet yet (the operator has not rendered it), so there is no pod to copy.
var errNoAgentPod = &httpErr{409, "error", "the agent has no pod spec yet (the operator has not rendered it); try again in a minute"}

// chat env vars the sidecar of a chat pod drops: router registration and wakes are the autonomous agent's
var chatSidecarDrop = map[string]bool{"WAKE_TOKEN": true, "REGISTER_TOKEN": true, "SELF_URL": true}

// chatPod turns the agent's pod template into the interactive session pod. Everything not named here is the agent's.
func chatPod(agent corev1.PodTemplateSpec, s chatSession, tokenSHA256 string, idle int) (corev1.PodSpec, error) {
	p := *agent.Spec.DeepCopy()
	p.RestartPolicy = corev1.RestartPolicyNever
	p.EnableServiceLinks = ptr(false)
	if p.SecurityContext == nil {
		p.SecurityContext = &corev1.PodSecurityContext{}
	}
	p.SecurityContext.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	// the agent's disk is the StatefulSet's claim "state": the session gets its own empty /state
	hasState := false
	for _, v := range p.Volumes {
		hasState = hasState || v.Name == "state"
	}
	if !hasState {
		p.Volumes = append([]corev1.Volume{{Name: "state", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}, p.Volumes...)
	}
	env := func(n, v string) corev1.EnvVar { return corev1.EnvVar{Name: n, Value: v} }
	set := func(list []corev1.EnvVar, add ...corev1.EnvVar) []corev1.EnvVar {
		for _, a := range add {
			found := false
			for i := range list {
				if list[i].Name == a.Name {
					list[i], found = a, true
				}
			}
			if !found {
				list = append(list, a)
			}
		}
		return list
	}
	var agentC, forgeC bool
	for i := range p.Containers {
		c := &p.Containers[i]
		switch c.Name {
		case "agent":
			agentC = true
			c.Name = "chat"
			c.Command, c.Args = []string{"agent-chat-session"}, nil
			// tool permissions are answered in the console (chat-session-server, --permission-prompt-tool stdio): the
			// agent's approval path needs an issue or PR job of the agent to comment on, which a session has not
			c.Env = set(c.Env, env("CHAT_MODE", "console"), env("SESSION_NAME", s.ID), env("CHAT_AGENT", s.Agent),
				env("IDLE_SECONDS", fmt.Sprint(idle)), env("CHAT_CONTROL_SHA256", tokenSHA256), env("CHAT_PORT", fmt.Sprint(chatPort)),
				env("CLAUDE_CONFIG_DIR", "/home/node/.claude"))
			c.Ports = []corev1.ContainerPort{{Name: "session", ContainerPort: chatPort, Protocol: corev1.ProtocolTCP}}
			c.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intOrStr(chatPort)}}, PeriodSeconds: 3}
		case "forge":
			forgeC = true
			kept := c.Env[:0]
			for _, e := range c.Env {
				if !chatSidecarDrop[e.Name] {
					kept = append(kept, e)
				}
			}
			// no approval requests from a chat pod: the operator reads them only from agent pods, so a push to a
			// protected path is refused at once instead of waiting for a decision nobody can see
			c.Env = set(kept, env("APPROVALS", "false"))
			c.Ports = nil // no webhook listener without the wake token
		default:
			return p, fmt.Errorf("agent pod has an unexpected container %q", c.Name)
		}
	}
	if !agentC || !forgeC {
		return p, errors.New("agent pod has no agent or no forge container")
	}
	return p, nil
}

func (k *kubeChat) agentTemplate(ctx context.Context, agent string) (corev1.PodTemplateSpec, error) {
	sts, err := k.c.AppsV1().StatefulSets(k.cfg.Namespace).Get(ctx, "agent-"+agent, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return corev1.PodTemplateSpec{}, errNoAgentPod
	}
	if err != nil {
		return corev1.PodTemplateSpec{}, err
	}
	if sts.Labels["app"] != "coding-agent" || sts.Spec.Template.Labels["agent"] != agent {
		return corev1.PodTemplateSpec{}, errNoAgentPod
	}
	return sts.Spec.Template, nil
}

func (k *kubeChat) jobFrom(t corev1.PodTemplateSpec, s chatSession, tokenSHA256 string) (*batchv1.Job, error) {
	labels := map[string]string{"app": "agent-chat", "session": s.ID, "agentdev/chat-agent": s.Agent, "agentdev/console": "true"}
	// the agent's other pod labels (cost labels, #184), without the ones that select agent pods
	podLabels := map[string]string{}
	for k, v := range t.Labels {
		if k != "app" && k != "agent" {
			podLabels[k] = v
		}
	}
	for k, v := range labels {
		podLabels[k] = v
	}
	p, err := chatPod(t, s, tokenSHA256, k.cfg.IdleSeconds)
	if err != nil {
		return nil, err
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: s.ID, Namespace: k.cfg.Namespace, Labels: labels,
			Annotations: map[string]string{"agentdev/started-by": s.StartedBy, "agentdev/repo": s.Repo}},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr[int32](0), ActiveDeadlineSeconds: ptr(k.cfg.MaxSeconds), TTLSecondsAfterFinished: ptr[int32](300),
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: podLabels}, Spec: p},
		},
	}, nil
}

func (k *kubeChat) StartSession(ctx context.Context, s chatSession, tokenSHA256 string) error {
	t, err := k.agentTemplate(ctx, s.Agent)
	if err != nil {
		return err
	}
	j, err := k.jobFrom(t, s, tokenSHA256)
	if err != nil {
		return err
	}
	_, err = k.c.BatchV1().Jobs(k.cfg.Namespace).Create(ctx, j, metav1.CreateOptions{})
	return err
}

func (k *kubeChat) SessionAddr(ctx context.Context, id string) (string, error) {
	pods, err := k.c.CoreV1().Pods(k.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=agent-chat,agentdev/console=true,session=" + id})
	if err != nil {
		return "", err
	}
	if len(pods.Items) > 0 { // the Job has one pod; only the first counts
		p := pods.Items[0]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			return "", errChatGone
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue && p.Status.PodIP != "" {
				return net.JoinHostPort(p.Status.PodIP, fmt.Sprint(chatPort)), nil
			}
		}
		return "", nil
	}
	if _, err := k.c.BatchV1().Jobs(k.cfg.Namespace).Get(ctx, id, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		return "", errChatGone
	}
	return "", nil // the Job exists, the pod is not there yet
}

func (k *kubeChat) StopSession(ctx context.Context, id string) error {
	j, err := k.c.BatchV1().Jobs(k.cfg.Namespace).Get(ctx, id, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return errChatGone
	}
	if err != nil {
		return err
	}
	if j.Labels["agentdev/console"] != "true" || j.Labels["app"] != "agent-chat" {
		return fmt.Errorf("job %s is not a console chat session", id)
	}
	return k.c.BatchV1().Jobs(k.cfg.Namespace).Delete(ctx, id, metav1.DeleteOptions{
		PropagationPolicy: ptr(metav1.DeletePropagationBackground), Preconditions: &metav1.Preconditions{UID: &j.UID}})
}
