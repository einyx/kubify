// Package seed consolidates kubo-system secret lifecycle:
//
//   - Adopt: recreate missing kubo-system source secrets from the tenant
//     copies that already exist in Stack namespaces (tenant-prefixed only —
//     shared secrets are never adopted from tenants).
//   - Export: dump all platform secrets in kubo-system into a single bundle
//     file for off-cluster backup. The file is plaintext YAML — encrypt it
//     (age/gpg) before storing anywhere shared.
//   - Import: restore a bundle produced by export.
package seed

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
)

const sourceNamespace = "kubo-system"

// SecretBundle is the on-disk export format: one YAML list of Secrets.
type SecretBundle struct {
	Secrets []corev1.Secret `json:"secrets"`
}

// AdoptResult reports what a pass over one Stack did.
type AdoptResult struct {
	Adopted []string
	Missing []string // tenant copy also absent — needs external provisioning
	Shared  []string // present but not adoptable (non-prefixed source)
}

// AdoptStack recreates missing kubo-system sources for one Stack from its
// tenant copies. It mirrors the operator's ensureSecrets adoption, but works
// across the whole mapping set and reports what happened.
func AdoptStack(ctx context.Context, c client.Client, stack *platformv1alpha1.Stack) (AdoptResult, error) {
	res := AdoptResult{}
	srcNS := sourceNamespace

	// Collect source→tenant-copy mappings the same way ensureSecrets does.
	type mapping struct{ from, to string }
	var mappings []mapping
	seen := map[string]bool{}
	add := func(from, to string) {
		if from == "" || seen[from] {
			return
		}
		seen[from] = true
		if to == "" {
			to = from
		}
		mappings = append(mappings, mapping{from, to})
	}
	for _, m := range stack.Spec.SecretsRef {
		add(m.From, m.To)
	}
	if stack.Spec.Bundle != nil && stack.Spec.Bundle.SecretRef != nil {
		add(stack.Spec.Bundle.SecretRef.Name, "")
	}
	for _, extra := range stack.Spec.ExtraBundles {
		if extra.SecretRef != nil {
			add(extra.SecretRef.Name, "")
		}
	}
	if stack.Spec.Inline != nil {
		for _, comp := range stack.Spec.Inline.Components {
			if comp.ChartPullSecretRef != nil {
				add(comp.ChartPullSecretRef.Name, "")
			}
		}
	}

	for _, m := range mappings {
		var src corev1.Secret
		err := c.Get(ctx, types.NamespacedName{Namespace: srcNS, Name: m.from}, &src)
		if err == nil {
			continue // source present
		}
		if !apierrors.IsNotFound(err) {
			return res, err
		}
		// The mapping's `to` is the tenant-side copy name — adopt from there.
		var copy corev1.Secret
		if err := c.Get(ctx, types.NamespacedName{Namespace: stack.Namespace, Name: m.to}, &copy); err != nil {
			if apierrors.IsNotFound(err) {
				res.Missing = append(res.Missing, m.from)
				continue
			}
			return res, err
		}
		if !strings.HasPrefix(m.from, stack.Namespace+"-") {
			res.Shared = append(res.Shared, m.from)
			continue
		}
		adopted := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:        m.from,
				Namespace:   srcNS,
				Annotations: map[string]string{"platform.kubo.io/adopted-from": stack.Namespace + "/" + m.to},
			},
			Type: copy.Type,
			Data: copy.Data,
		}
		if err := c.Create(ctx, &adopted); err != nil && !apierrors.IsAlreadyExists(err) {
			return res, err
		}
		res.Adopted = append(res.Adopted, m.from)
	}
	return res, nil
}

// AdoptAll runs AdoptStack for every Stack in the cluster.
func AdoptAll(ctx context.Context, c client.Client) (map[string]AdoptResult, error) {
	var list platformv1alpha1.StackList
	if err := c.List(ctx, &list); err != nil {
		return nil, err
	}
	out := map[string]AdoptResult{}
	for i := range list.Items {
		s := &list.Items[i]
		key := s.Namespace + "/" + s.Name
		res, err := AdoptStack(ctx, c, s)
		if err != nil {
			return out, fmt.Errorf("%s: %w", key, err)
		}
		out[key] = res
	}
	return out, nil
}

// Export collects every Secret in kubo-system into a bundle.
func Export(ctx context.Context, c client.Client) (*SecretBundle, error) {
	var list corev1.SecretList
	if err := c.List(ctx, &list, client.InNamespace(sourceNamespace)); err != nil {
		return nil, err
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	b := &SecretBundle{Secrets: list.Items}
	return b, nil
}

// Import restores a bundle: creates missing secrets, updates existing ones.
func Import(ctx context.Context, c client.Client, b *SecretBundle) (created, updated int, err error) {
	for i := range b.Secrets {
		s := b.Secrets[i]
		s.Namespace = sourceNamespace
		s.ResourceVersion = ""
		var existing corev1.Secret
		gerr := c.Get(ctx, types.NamespacedName{Namespace: sourceNamespace, Name: s.Name}, &existing)
		switch {
		case apierrors.IsNotFound(gerr):
			if e := c.Create(ctx, &s); e != nil && !apierrors.IsAlreadyExists(e) {
				return created, updated, e
			}
			created++
		case gerr != nil:
			return created, updated, gerr
		default:
			existing.Data = s.Data
			existing.Type = s.Type
			if e := c.Update(ctx, &existing); e != nil {
				return created, updated, e
			}
			updated++
		}
	}
	return created, updated, nil
}

// Scheme returns a runtime scheme covering everything the seed tools touch.
func Scheme() *runtime.Scheme {
	sch := runtime.NewScheme()
	_ = corev1.AddToScheme(sch)
	_ = platformv1alpha1.AddToScheme(sch)
	return sch
}
