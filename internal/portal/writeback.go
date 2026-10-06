package portal

import (
	"context"
	"fmt"
	"strings"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (p *Portal) CreateFeatureFlagWriteback(ctx context.Context, namespace, name, requestedBy string, flags, expected map[string]string) (*platformv1alpha1.GitWritebackRequest, error) {
	if namespace == "" || name == "" || len(flags) == 0 {
		return nil, fmt.Errorf("target and featureFlags are required")
	}
	for k, v := range flags {
		if strings.TrimSpace(k) == "" || (v != "true" && v != "false") {
			return nil, fmt.Errorf("featureFlags[%q] must be true or false", k)
		}
	}
	wb := &platformv1alpha1.GitWritebackRequest{ObjectMeta: metav1.ObjectMeta{GenerateName: name + "-flags-", Namespace: namespace}, Spec: platformv1alpha1.GitWritebackRequestSpec{Target: platformv1alpha1.WritebackTarget{Namespace: namespace, Name: name}, FeatureFlags: flags, ExpectedFeatureFlags: expected, RequestedBy: requestedBy}}
	if err := p.client.Create(ctx, wb); err != nil {
		return nil, err
	}
	return wb, nil
}
