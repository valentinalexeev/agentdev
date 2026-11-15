package main

// Chat session Jobs (#346). The API creates one Job per session in the agents namespace, from the same pod shape as
// manifests/chat.yaml.tmpl (the kubectl path of scripts/agent-chat.sh), with these differences:
//   - an initContainer clones the repository with the read-only token of the Secret agentdev-chat-clone; the chat
//     container never holds a forge token (CHAT_MODE=console);
//   - the container runs chat-session-server on port 7681 with CHAT_CONTROL_SHA256 (the sha256 of the API's
//     per-session token).
// The ValidatingAdmissionPolicy agentdev-api-chat-jobs (manifests/api.yaml.tmpl) holds the API's service account to
// exactly this shape: name chat-*, the worker image, gVisor, no service account token, and only the Secrets
// agentdev-chat-clone and agent-<agent>-proxy. The API's Role has no access to Secrets.

import (
	"context"
	"fmt"
	"net"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
)

const chatPort = 7681

// chatJobConfig is read from the environment (CHAT_* in manifests/api.yaml.tmpl).
type chatJobConfig struct {
	Namespace      string
	WorkerImage    string
	PullPolicy     string
	ForgeURL       string // as the session pod reaches it (Gitea in-cluster, or https://github.com)
	ClaudeProxyURL string
	EgressProxy    string
	IdleSeconds    int
	MaxSeconds     int64
}

type kubeChat struct {
	c   kubernetes.Interface
	cfg chatJobConfig
}

func ptr[T any](v T) *T { return &v }

func intOrStr(n int) intstr.IntOrString { return intstr.FromInt32(int32(n)) }

func (k *kubeChat) job(s chatSession, tokenSHA256 string) *batchv1.Job {
	labels := map[string]string{"app": "agent-chat", "session": s.ID, "agentdev/chat-agent": s.Agent, "agentdev/console": "true"}
	env := func(n, v string) corev1.EnvVar { return corev1.EnvVar{Name: n, Value: v} }
	secret := func(n, name, key string, optional bool) corev1.EnvVar {
		return corev1.EnvVar{Name: n, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key, Optional: ptr(optional)}}}
	}
	noProxy := "localhost,127.0.0.1,::1,.svc,.svc.cluster.local,.cluster.local"
	proxyEnv := []corev1.EnvVar{env("HTTPS_PROXY", k.cfg.EgressProxy), env("HTTP_PROXY", k.cfg.EgressProxy),
		env("https_proxy", k.cfg.EgressProxy), env("http_proxy", k.cfg.EgressProxy), env("NO_PROXY", noProxy), env("no_proxy", noProxy)}
	sc := &corev1.SecurityContext{AllowPrivilegeEscalation: ptr(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}
	mounts := []corev1.VolumeMount{{Name: "state", MountPath: "/state"}}
	// clone: the token comes from the Secret through git's credential helper, never on the command line or in .git/config
	clone := corev1.Container{Name: "clone", Image: k.cfg.WorkerImage, ImagePullPolicy: corev1.PullPolicy(k.cfg.PullPolicy),
		Command: []string{"bash", "-c", `set -euo pipefail
git -c credential.helper='!f() { echo "username=${FORGE_USER:-x-access-token}"; echo "password=${FORGE_TOKEN:-}"; }; f' \
  clone -q --depth 50 "${FORGE_URL%/}/${REPO}.git" /state/repo`},
		Env: append([]corev1.EnvVar{env("FORGE_URL", k.cfg.ForgeURL), env("REPO", s.Repo),
			secret("FORGE_USER", "agentdev-chat-clone", "FORGE_USER", true),
			secret("FORGE_TOKEN", "agentdev-chat-clone", "FORGE_TOKEN", true)}, proxyEnv...),
		VolumeMounts: mounts, SecurityContext: sc,
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}}}
	chat := corev1.Container{Name: "chat", Image: k.cfg.WorkerImage, ImagePullPolicy: corev1.PullPolicy(k.cfg.PullPolicy),
		Command: []string{"agent-chat-session"},
		Env: append([]corev1.EnvVar{env("CHAT_MODE", "console"), env("REPO", s.Repo), env("SESSION_NAME", s.ID),
			env("CHAT_AGENT", s.Agent), env("IDLE_SECONDS", fmt.Sprint(k.cfg.IdleSeconds)), env("CHAT_CONTROL_SHA256", tokenSHA256),
			env("CHAT_PORT", fmt.Sprint(chatPort)), env("CLAUDE_CONFIG_DIR", "/home/node/.claude"),
			env("ANTHROPIC_BASE_URL", k.cfg.ClaudeProxyURL),
			secret("ANTHROPIC_AUTH_TOKEN", "agent-"+s.Agent+"-proxy", "ANTHROPIC_AUTH_TOKEN", false),
			env("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")}, proxyEnv...),
		Ports:           []corev1.ContainerPort{{Name: "session", ContainerPort: chatPort}},
		ReadinessProbe:  &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intOrStr(chatPort)}}, PeriodSeconds: 3},
		VolumeMounts:    append(mounts, corev1.VolumeMount{Name: "claude", MountPath: "/home/node/.claude"}, corev1.VolumeMount{Name: "guide", MountPath: "/etc/agentdev/guide", ReadOnly: true}),
		SecurityContext: sc,
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("512Mi")},
			Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("2Gi")}}}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: s.ID, Namespace: k.cfg.Namespace, Labels: labels,
			Annotations: map[string]string{"agentdev/started-by": s.StartedBy, "agentdev/repo": s.Repo}},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr[int32](0), ActiveDeadlineSeconds: ptr(k.cfg.MaxSeconds), TTLSecondsAfterFinished: ptr[int32](300),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever, RuntimeClassName: ptr("gvisor"),
					NodeSelector: map[string]string{"agentdev/pool": "sandboxes"},
					Tolerations: []corev1.Toleration{
						{Key: "agentdev/pool", Operator: corev1.TolerationOpEqual, Value: "sandboxes", Effect: corev1.TaintEffectNoSchedule},
						{Key: "sandbox.gke.io/runtime", Operator: corev1.TolerationOpEqual, Value: "gvisor", Effect: corev1.TaintEffectNoSchedule}},
					AutomountServiceAccountToken: ptr(false), EnableServiceLinks: ptr(false),
					SecurityContext: &corev1.PodSecurityContext{RunAsUser: ptr[int64](1000), RunAsNonRoot: ptr(true), FSGroup: ptr[int64](1000),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
					InitContainers: []corev1.Container{clone},
					Containers:     []corev1.Container{chat},
					Volumes: []corev1.Volume{
						{Name: "state", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "claude", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "guide", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: "agentdev-guides"}, Optional: ptr(true), DefaultMode: ptr[int32](0o444),
							Items: []corev1.KeyToPath{{Key: s.Agent, Path: "guide.md"}}}}},
					},
				},
			},
		},
	}
}

func (k *kubeChat) StartSession(ctx context.Context, s chatSession, tokenSHA256 string) error {
	_, err := k.c.BatchV1().Jobs(k.cfg.Namespace).Create(ctx, k.job(s, tokenSHA256), metav1.CreateOptions{})
	return err
}

func (k *kubeChat) SessionAddr(ctx context.Context, id string) (string, error) {
	pods, err := k.c.CoreV1().Pods(k.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=agent-chat,agentdev/console=true,session=" + id})
	if err != nil {
		return "", err
	}
	for _, p := range pods.Items {
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
