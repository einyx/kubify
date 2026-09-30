package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"encoding/json"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
)

const fluxManagedBy = "kubo"

// FluxStrategy deploys stack components by compiling them into Flux
// HelmRepository + HelmRelease objects. Deployment, retries and rollback
// are owned by Flux helm-controller; this strategy only reconciles the
// desired objects and aggregates HelmRelease status back into the Stack.
type FluxStrategy struct {
	Client client.Client
}

func helmRepoName(repoURL string) string {
	h := sha256.Sum256([]byte(repoURL))
	return fmt.Sprintf("kubo-%x", h[:4])
}

func toJSONValues(m map[string]interface{}) (*apiextensionsv1.JSON, error) {
	if len(m) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return &apiextensionsv1.JSON{Raw: raw}, nil
}

// Reconcile ensures HelmRepository + HelmRelease objects exist for each
// component and returns component statuses derived from HelmRelease state.
func (s *FluxStrategy) Reconcile(
	ctx context.Context,
	stack *platformv1alpha1.Stack,
	def *platformv1alpha1.StackDefinition,
	order []string,
	byName map[string]platformv1alpha1.StackComponentSpec,
) ([]platformv1alpha1.ComponentStatus, error) {
	repos := map[string]*sourcev1.HelmRepository{}

	var statuses []platformv1alpha1.ComponentStatus
	for _, name := range order {
		comp := byName[name]

		repoName := helmRepoName(comp.ChartRef.RepoURL)
		repo, ok := repos[repoName]
		if !ok {
			repo = &sourcev1.HelmRepository{
				ObjectMeta: metav1.ObjectMeta{Name: repoName, Namespace: stack.Namespace},
			}
			_, err := ctrl.CreateOrUpdate(ctx, s.Client, repo, func() error {
				repo.Labels = map[string]string{"app.kubernetes.io/managed-by": fluxManagedBy}
				repo.Spec.URL = comp.ChartRef.RepoURL
				repo.Spec.Interval = metav1.Duration{Duration: 10 * time.Minute}
				if strings.HasPrefix(comp.ChartRef.RepoURL, "oci://") {
					repo.Spec.Type = sourcev1.HelmRepositoryTypeOCI
				}
				return ctrl.SetControllerReference(stack, repo, s.Client.Scheme())
			})
			if err != nil {
				return statuses, fmt.Errorf("ensure HelmRepository %s: %w", repoName, err)
			}
			repos[repoName] = repo
		}

		values := resolveComponentValues(&comp.Values, &stack.Spec.Values, stack.Spec.ComponentValues, name)
		valsJSON, err := toJSONValues(values)
		if err != nil {
			return statuses, fmt.Errorf("marshal values for %s: %w", name, err)
		}

		// Flux v2 dependsOn references other HelmReleases in the same namespace.
		var deps []helmv2.DependencyReference
		for _, dep := range comp.DependsOn {
			deps = append(deps, helmv2.DependencyReference{Name: dep})
		}

		hr := &helmv2.HelmRelease{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: stack.Namespace},
		}
		_, err = ctrl.CreateOrUpdate(ctx, s.Client, hr, func() error {
			hr.Labels = map[string]string{"app.kubernetes.io/managed-by": fluxManagedBy}
			hr.Spec.Interval = metav1.Duration{Duration: 5 * time.Minute}
			hr.Spec.Timeout = &metav1.Duration{Duration: 10 * time.Minute}
			hr.Spec.ReleaseName = name
			hr.Spec.TargetNamespace = stack.Namespace
			hr.Spec.StorageNamespace = stack.Namespace
			hr.Spec.Chart = &helmv2.HelmChartTemplate{
				Spec: helmv2.HelmChartTemplateSpec{
					Chart:   comp.ChartRef.ChartName,
					Version: comp.ChartRef.ChartVersion,
					SourceRef: helmv2.CrossNamespaceObjectReference{
						Kind:      sourcev1.HelmRepositoryKind,
						Name:      repoName,
						Namespace: stack.Namespace,
					},
				},
			}
			hr.Spec.Values = valsJSON
			hr.Spec.DependsOn = deps
			return ctrl.SetControllerReference(stack, hr, s.Client.Scheme())
		})
		if err != nil {
			return statuses, fmt.Errorf("ensure HelmRelease %s: %w", name, err)
		}

		statuses = append(statuses, helmReleaseStatus(name, hr))
	}
	return statuses, nil
}

// Cleanup removes Flux objects previously owned by this Stack (e.g. after
// switching a Stack from Flux mode back to Direct).
func (s *FluxStrategy) Cleanup(ctx context.Context, stack *platformv1alpha1.Stack) error {
	for _, list := range []client.ObjectList{
		&helmv2.HelmReleaseList{},
		&sourcev1.HelmRepositoryList{},
	} {
		if err := s.Client.List(ctx, list, client.InNamespace(stack.Namespace), client.MatchingLabels{"app.kubernetes.io/managed-by": fluxManagedBy}); err != nil {
			continue
		}
		var items []client.Object
		switch l := list.(type) {
		case *helmv2.HelmReleaseList:
			for i := range l.Items {
				items = append(items, &l.Items[i])
			}
		case *sourcev1.HelmRepositoryList:
			for i := range l.Items {
				items = append(items, &l.Items[i])
			}
		}
		for _, obj := range items {
			// Only delete objects owned by this Stack.
			if owner := metav1.GetControllerOf(obj); owner != nil && owner.UID == stack.UID {
				if err := s.Client.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
					return err
				}
			}
		}
	}
	return nil
}

func helmReleaseStatus(name string, hr *helmv2.HelmRelease) platformv1alpha1.ComponentStatus {
	st := platformv1alpha1.ComponentStatus{Name: name, Phase: "Pending"}

	ready := metaFindCondition(hr.Status.Conditions, "Ready")
	if ready == nil {
		return st
	}
	switch ready.Status {
	case metav1.ConditionTrue:
		st.Phase = "Ready"
	case metav1.ConditionFalse:
		if ready.Reason == "InstallFailed" || ready.Reason == "UpgradeFailed" {
			st.Phase = "Failed"
		} else {
			st.Phase = "Degraded"
		}
	default:
		st.Phase = "Deploying"
	}
	st.Message = ready.Message
	if hr.Status.LastReleaseRevision > 0 {
		st.Revision = int(hr.Status.LastReleaseRevision)
	}
	if hr.Status.LastAttemptedReleaseActionDuration != nil {
		// Approximate deploy time; Flux exposes duration, not a wall-clock stamp.
		now := metav1.Now()
		ts := metav1.Time{Time: now.Add(-hr.Status.LastAttemptedReleaseActionDuration.Duration)}
		st.LastDeployed = &ts
	}
	return st
}

func metaFindCondition(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}
