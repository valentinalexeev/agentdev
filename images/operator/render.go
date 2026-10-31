package main

import (
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// render fills the StatefulSet from the Agent; the Go form of manifests/agent.yaml.tmpl. Keep them in step until
// 06-agents.sh creates Agents instead of rendering the template (follow-up of #47).
func (r *AgentReconciler) render(a *v1.Agent, s *appsv1.StatefulSet) {
	n, ns := a.Name, a.Namespace
	forgeSecret := a.Spec.ForgeSecret
	if forgeSecret == "" {
		forgeSecret = "agent-" + n
	}
	image := r.D.WorkerImage // never from the Agent (see AgentSpec)
	forge := a.Spec.Forge
	if forge == "" {
		forge = "gitea"
	}
	maxc := a.Spec.MaxConcurrent
	if maxc == 0 {
		maxc = 2
	}
	labels := map[string]string{"app": "coding-agent", "agent": n}

	if s.Labels == nil {
		s.Labels = map[string]string{}
	}
	for k, v := range labels {
		s.Labels[k] = v
	}
	if s.Annotations == nil {
		s.Annotations = map[string]string{}
	}
	s.Annotations["agentdev/scale-to-zero"] = strconv.FormatBool(a.Spec.ScaleToZero)
	s.Annotations["agentdev/user"] = n
	s.Annotations["agentdev/repos"] = a.Spec.Repo
	s.Annotations["agentdev/forge"] = forge

	s.Spec.ServiceName = "agent-" + n
	s.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"agent": n}}
	// keep the agent's state volume when the Agent is deleted: re-adding the agent resumes its work
	s.Spec.PersistentVolumeClaimRetentionPolicy = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
		WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType, WhenScaled: appsv1.RetainPersistentVolumeClaimRetentionPolicyType}
	if len(s.Spec.VolumeClaimTemplates) == 0 { // immutable after creation; same claims as manifests/agent.yaml.tmpl
		pvc := func(name, size string) corev1.PersistentVolumeClaim {
			return corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}}}}
		}
		s.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{pvc("state", "2Gi"), pvc("claude", "256Mi")}
	}

	env := func(k, v string) corev1.EnvVar { return corev1.EnvVar{Name: k, Value: v} }
	secretKey := func(k, secret string, optional bool) corev1.EnvVar {
		return corev1.EnvVar{Name: k, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: k, Optional: ptr.To(optional)}}}
	}
	s.Spec.Template.Labels = labels
	p := &s.Spec.Template.Spec
	p.RuntimeClassName = ptr.To("gvisor")
	p.NodeSelector = map[string]string{"agentdev/pool": "sandboxes"}
	p.Tolerations = []corev1.Toleration{
		{Key: "agentdev/pool", Operator: corev1.TolerationOpEqual, Value: "sandboxes", Effect: corev1.TaintEffectNoSchedule},
		{Key: "sandbox.gke.io/runtime", Operator: corev1.TolerationOpEqual, Value: "gvisor", Effect: corev1.TaintEffectNoSchedule},
	}
	p.AutomountServiceAccountToken = ptr.To(false)
	p.SecurityContext = &corev1.PodSecurityContext{RunAsUser: ptr.To[int64](1000), RunAsNonRoot: ptr.To(true), FSGroup: ptr.To[int64](1000)}
	p.Containers = []corev1.Container{{
		Name:                     "agent",
		Image:                    image,
		ImagePullPolicy:          corev1.PullPolicy(r.D.PullPolicy),
		TerminationMessagePath:   corev1.TerminationMessagePathDefault, // API defaults: set so updates are no-ops
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		Env: []corev1.EnvVar{
			env("FORGE_URL", r.D.ForgeURL), env("FORGE_USER", n), env("AGENT_NAME", n), env("REPO", a.Spec.Repo),
			env("POLL_SECONDS", r.D.PollSeconds), env("MAX_CONCURRENT", fmt.Sprint(maxc)), env("REVIEWER", r.D.Reviewer),
			env("ROUTER_URL", fmt.Sprintf("http://webhook-router.%s.svc.cluster.local:8080", ns)),
			env("SELF_URL", fmt.Sprintf("http://agent-%s.%s.svc.cluster.local:8080/", n, ns)),
			env("FORGE_KIND", forge), env("SCALE_TO_ZERO", strconv.FormatBool(a.Spec.ScaleToZero)),
			env("IDLE_SECONDS", r.D.IdleSeconds), env("BOTBOARD_URL", r.D.BotboardURL), env("FORGE_PUBLIC_URL", r.D.ForgePublicURL),
			{Name: "READY_ISSUE", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "agent-" + n + "-project"}, Key: "READY_ISSUE", Optional: ptr.To(true)}}},
			secretKey("WAKE_TOKEN", forgeSecret, true), secretKey("REGISTER_TOKEN", forgeSecret, true),
			secretKey("FORGE_TOKEN", forgeSecret, false),
		},
		// exactly one Claude credential per agent (#5); MCP token optional (#14)
		EnvFrom: []corev1.EnvFromSource{
			{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: a.Spec.ClaudeSecret}}},
			{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "agent-" + n + "-mcp"}, Optional: ptr.To(true)}},
		},
		Ports:        []corev1.ContainerPort{{Name: "webhook", ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
		VolumeMounts: []corev1.VolumeMount{{Name: "state", MountPath: "/state"}, {Name: "claude", MountPath: "/home/node/.claude"}},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("512Mi")},
			Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"),
				// 3Gi fits 2 parallel items (agent.yaml.tmpl); ~1Gi per extra slot
				corev1.ResourceMemory: *resource.NewQuantity(int64(1+maxc)*(1<<30), resource.BinarySI)},
		},
	}}
}
