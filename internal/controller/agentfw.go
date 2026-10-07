package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	"github.com/einyx/kubo/internal/agentfw"
)

const (
	agentfwName  = "agentfw"
	agentfwImage = "meshxregistry.azurecr.io/kubo/agentfw:main" // ACR mirror — GHCR is rate-limited from clusters; sync via CI or `docker push`
	agentfwPort  = 8080
	// Admin endpoint: kill switch + session-archive viewer. The portal's
	// agentfw integration proxies this port (svc:<ns>/agentfw:8081).
	agentfwAdminPort = 8081
	agentfwPolicyKey = "policy.yaml"
	// MITM CA: agentfw terminates CONNECT tunnels so HTTPS content is
	// archived and scanned. The key stays in the proxy pod; ca.crt is
	// republished as agentfw-ca for agent containers to trust.
	agentfwMITMCASecret = "agentfw-mitm-ca"
	agentfwCASecret     = "agentfw-ca"
	// agentfwMITMAnnotation opts a Stack into TLS termination. MITM is
	// off by default: agents only accept the minted leafs once they mount
	// agentfw-ca (SSL_CERT_FILE / REQUESTS_CA_BUNDLE / NODE_EXTRA_CA_CERTS),
	// so tenants flip this when their apps are ready.
	agentfwMITMAnnotation = "kubify.io/agentfw-mitm"
	// agentfwViewPVC backs /var/lib/agentfw so the SQLite viewer/billing
	// archive survives pod restarts and rescheduling.
	agentfwViewPVC = "agentfw-view"
)

// agentfwPolicyYAML renders the tenant policy. The MITM block always carries
// the CA paths; mitmEnabled flips with the Stack annotation so enabling is a
// one-line change once agents trust the CA.
func agentfwPolicyYAML(mitm bool) string {
	return agentfwPolicyBase + agentfwMITMBlock(mitm)
}

const agentfwPolicyBase = `allowedMCPTools: ["*"]
blockPrivateEgress: true
dlpAction: redact
injectionAction: block
# upstream: https://api.openai.com   # set to enable reverse-proxy mode
# Base-URL mode alongside forward-proxy mode: clients point e.g.
# ANTHROPIC_BASE_URL=http://agentfw:8080 and agentfw resolves the target —
# per-host overrides first, this default as fallback. The X-Agentfw-Upstream
# header and path-embedded targets (/https://host/...) bypass both.
baseURLDefault: https://api.anthropic.com
baseURLRoutes:
  api.openai.com: https://api.openai.com
signingKeyPath: /var/lib/agentfw/signing.key
viewDBPath: /var/lib/agentfw/view.db
`

func agentfwMITMBlock(mitm bool) string {
	flag := "false"
	if mitm {
		flag = "true"
	}
	return `# TLS termination of CONNECT tunnels (operator-managed CA; secret agentfw-mitm-ca).
# Enable per Stack with the kubify.io/agentfw-mitm: "true" annotation once
# agent containers mount the agentfw-ca secret as their trust bundle.
mitmEnabled: ` + flag + `
mitmCaCert: /etc/agentfw/mitm/ca.crt
mitmCaKey: /etc/agentfw/mitm/ca.key
`
}

func (r *StackReconciler) ensureTenantAgentFW(ctx context.Context, stack *platformv1alpha1.Stack) error {
	ns := stack.Namespace

	if err := r.ensureAgentFWMITMCA(ctx, ns); err != nil {
		return fmt.Errorf("agentfw mitm ca: %w", err)
	}
	if err := r.ensureAgentFWConfigMap(ctx, stack); err != nil {
		return fmt.Errorf("agentfw configmap: %w", err)
	}
	if err := r.ensureAgentFWViewPVC(ctx, stack.Namespace); err != nil {
		return fmt.Errorf("agentfw view pvc: %w", err)
	}
	if err := r.ensureAgentFWDeployment(ctx, stack); err != nil {
		return fmt.Errorf("agentfw deployment: %w", err)
	}
	if err := r.ensureAgentFWService(ctx, ns); err != nil {
		return fmt.Errorf("agentfw service: %w", err)
	}
	return nil
}

// ensureAgentFWMITMCA mints the tenant's MITM CA once and republishes the
// public half as agentfw-ca. Existing CAs are never rotated: leaf certs minted
// from a replaced CA would fail every agent that pinned the old one.
func (r *StackReconciler) ensureAgentFWMITMCA(ctx context.Context, ns string) error {
	var existing corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: agentfwMITMCASecret}, &existing)
	if err == nil {
		return r.ensureAgentFWCA(ctx, ns, existing.Data["ca.crt"])
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	certPEM, keyPEM, err := agentfw.GenerateCAPEM()
	if err != nil {
		return fmt.Errorf("generate ca: %w", err)
	}
	ca := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: agentfwMITMCASecret, Namespace: ns},
		Data:       map[string][]byte{"ca.crt": certPEM, "ca.key": keyPEM},
	}
	if err := r.Create(ctx, ca); err != nil {
		return err
	}
	return r.ensureAgentFWCA(ctx, ns, certPEM)
}

// ensureAgentFWCA publishes ca.crt alone (no private key) for agent
// containers to mount as their trust bundle (SSL_CERT_FILE et al).
func (r *StackReconciler) ensureAgentFWCA(ctx context.Context, ns string, certPEM []byte) error {
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: agentfwCASecret, Namespace: ns},
		Data:       map[string][]byte{"ca.crt": certPEM},
	}
	var existing corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: agentfwCASecret}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if string(existing.Data["ca.crt"]) != string(certPEM) {
		existing.Data = desired.Data
		return r.Update(ctx, &existing)
	}
	return nil
}

func (r *StackReconciler) ensureAgentFWConfigMap(ctx context.Context, stack *platformv1alpha1.Stack) error {
	ns := stack.Namespace
	mitm := stack.Annotations[agentfwMITMAnnotation] == "true"
	desired := agentfwPolicyYAML(mitm)
	var existing corev1.ConfigMap
	err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: agentfwName}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: agentfwName, Namespace: ns},
			Data:       map[string]string{agentfwPolicyKey: desired},
		})
	}
	if err != nil {
		return err
	}
	pol := existing.Data[agentfwPolicyKey]
	updated := pol
	switch {
	case pol == "":
		updated = desired
	case !strings.Contains(pol, "mitmEnabled:"):
		// Pre-MITM policy: append the operator-managed block verbatim,
		// preserving whatever the user customised above it.
		updated = pol + agentfwMITMBlock(mitm)
	default:
		// Converge only the operator-owned flag line; user comments,
		// ordering and other values are preserved.
		want := "mitmEnabled: false"
		if mitm {
			want = "mitmEnabled: true"
		}
		if !strings.Contains(pol, want) {
			updated = regexp.MustCompile(`(?m)^mitmEnabled:.*$`).ReplaceAllString(pol, want)
		}
	}
	if updated == pol {
		return nil
	}
	existing.Data[agentfwPolicyKey] = updated
	return r.Update(ctx, &existing)
}

// ensureAgentFWViewPVC creates the billing-archive volume once. Existing
// claims are never modified — storage class/size changes are left to the
// platform team so we never fight a resized or statically-provisioned PVC.
func (r *StackReconciler) ensureAgentFWViewPVC(ctx context.Context, ns string) error {
	var existing corev1.PersistentVolumeClaim
	err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: agentfwViewPVC}, &existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	return r.Create(ctx, &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: agentfwViewPVC, Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
		},
	})
}

func (r *StackReconciler) ensureAgentFWDeployment(ctx context.Context, stack *platformv1alpha1.Stack) error {
	ns := stack.Namespace
	// Pod annotation carries a checksum of the desired policy so flips of the
	// mitmEnabled flag roll the pod (agentfw reads the policy at startup).
	mitm := stack.Annotations[agentfwMITMAnnotation] == "true"
	policySum := sha256.Sum256([]byte(agentfwPolicyYAML(mitm)))
	policyChecksum := hex.EncodeToString(policySum[:8])
	replicas := int32(1)
	fsGroup := int64(65532)
	desired := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: agentfwName, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			// Recreate, not RollingUpdate: the deployment is single-replica
			// with a ReadWriteOnce PVC. A rolling update deadlocks — the
			// new pod can't attach the volume while the old (Ready) pod
			// holds it, and the old pod is only removed once the new one
			// is Ready. Recreate accepts a brief outage instead.
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RecreateDeploymentStrategyType,
			},
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": agentfwName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      map[string]string{"app": agentfwName},
					Annotations: map[string]string{"kubify.io/agentfw-policy": policyChecksum},
				},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{
						// Let the nonroot agentfw (65532) write the viewer archive.
						FSGroup: &fsGroup,
					},
					Containers: []corev1.Container{{
						Name:  agentfwName,
						Image: agentfwImage,
						// :main is a mutable tracking tag — always re-pull so
						// nodes pick up fresh pushes instead of stale cache.
						ImagePullPolicy: corev1.PullAlways,
						Args:            []string{"-addr=:8080", "-admin=:8081", "-policy=/etc/agentfw/policy.yaml"},
						Ports: []corev1.ContainerPort{
							{ContainerPort: agentfwPort, Protocol: corev1.ProtocolTCP},
							{Name: "admin", ContainerPort: agentfwAdminPort, Protocol: corev1.ProtocolTCP},
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "policy",
							MountPath: "/etc/agentfw",
							ReadOnly:  true,
						}, {
							// MITM CA (ca.crt + ca.key) — the key never leaves
							// the proxy: it mints per-host leaf certs.
							Name:      "mitm-ca",
							MountPath: "/etc/agentfw/mitm",
							ReadOnly:  true,
						}, {
							// Viewer archive (SQLite) — /var/lib/agentfw must
							// exist and be writable by uid 65532.
							Name:      "viewer-archive",
							MountPath: "/var/lib/agentfw",
						}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("50m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("200m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "policy",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: agentfwName},
							},
						},
					}, {
						Name: "mitm-ca",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{
								SecretName: agentfwMITMCASecret,
							},
						},
					}, {
						Name: "viewer-archive",
						VolumeSource: corev1.VolumeSource{
							// Persistent claim, not emptyDir: the SQLite
							// archive is the billing/usage history — an
							// emptyDir would lose it on every pod restart.
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: agentfwViewPVC,
							},
						},
					}},
				},
			},
		},
	}

	var existing appsv1.Deployment
	err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: agentfwName}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	// Sync the pod-spec fields this controller owns. Containers alone is
	// not enough: a container that gains a volumeMount while Volumes stays
	// stale renders an invalid Deployment (volumeMounts[x].name not found)
	// that the API server rejects on every reconcile.
	existing.Spec.Template.Spec.Containers = desired.Spec.Template.Spec.Containers
	existing.Spec.Template.Spec.Volumes = desired.Spec.Template.Spec.Volumes
	existing.Spec.Template.Spec.SecurityContext = desired.Spec.Template.Spec.SecurityContext
	// Sync strategy so deployments created before the Recreate default stop
	// deadlocking on the RWO PVC during rollouts.
	existing.Spec.Strategy = desired.Spec.Strategy
	// Sync the policy checksum so mitmEnabled flips roll the pod.
	if existing.Spec.Template.Annotations == nil {
		existing.Spec.Template.Annotations = map[string]string{}
	}
	for k, v := range desired.Spec.Template.Annotations {
		existing.Spec.Template.Annotations[k] = v
	}
	return r.Update(ctx, &existing)
}

func (r *StackReconciler) ensureAgentFWService(ctx context.Context, ns string) error {
	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: agentfwName, Namespace: ns},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": agentfwName},
			Ports: []corev1.ServicePort{
				{
					// Multi-port Services require a name on every port.
					Name:       "proxy",
					Port:       agentfwPort,
					TargetPort: intstr.FromInt(agentfwPort),
					Protocol:   corev1.ProtocolTCP,
				},
				{
					Name:       "admin",
					Port:       agentfwAdminPort,
					TargetPort: intstr.FromInt(agentfwAdminPort),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
	var existing corev1.Service
	err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: agentfwName}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	// Self-heal drift (e.g. services created before the admin port existed).
	// Compare semantically: a same-count port mutation must also heal.
	if !equalPorts(existing.Spec.Ports, desired.Spec.Ports) {
		existing.Spec.Ports = desired.Spec.Ports
		return r.Update(ctx, &existing)
	}
	return err
}

func equalPorts(a, b []corev1.ServicePort) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Port != b[i].Port ||
			a[i].TargetPort != b[i].TargetPort || a[i].Protocol != b[i].Protocol {
			return false
		}
	}
	return true
}

// deleteAgentFW removes the per-tenant agent firewall resources.
func (r *StackReconciler) deleteAgentFW(ctx context.Context, stack *platformv1alpha1.Stack) error {
	ns := stack.Namespace
	for _, obj := range []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: agentfwName, Namespace: ns}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: agentfwName, Namespace: ns}},
		// ponytail: ConfigMap intentionally kept — preserves user policy customisations
	} {
		if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}
