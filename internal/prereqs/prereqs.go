// Package prereqs installs cluster-scoped prerequisites shared across all
// tenants' Stacks (Istio CRDs, cert-manager, etc.). Run once at controller
// startup, idempotent — subsequent Stack reconciles assume these exist.
package prereqs

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed istio-crds.yaml
var istioCRDs []byte

// Ensure server-side-applies the embedded prereqs. Owner name is stable so
// re-applies converge.
func Ensure(ctx context.Context, c client.Client) error {
	return applyAll(ctx, c, istioCRDs)
}

func applyAll(ctx context.Context, c client.Client, doc []byte) error {
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(doc), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := dec.Decode(obj); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("decode prereq: %w", err)
		}
		if obj.Object == nil {
			continue
		}
		if err := c.Patch(ctx, obj, client.Apply, client.FieldOwner("kubo-prereqs"), client.ForceOwnership); err != nil {
			if meta.IsNoMatchError(err) {
				continue
			}
			return fmt.Errorf("apply %s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}
}
