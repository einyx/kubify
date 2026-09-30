/*
Copyright 2025 The Kubo Authors.
*/

package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
)

type check struct {
	name   string
	ok     bool
	detail string
	warn   bool
}

const (
	green = "\033[32m"
	red   = "\033[31m"
	yellow = "\033[33m"
	reset = "\033[0m"
)

var externalSecretListGVK = metav1.SchemeGroupVersion.WithKind("ExternalSecretList")

func main() {
	if err := run(); err != nil {
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	cfg := config.GetConfigOrDie()
	clientScheme := scheme.Scheme
	if err := apiextensionsv1.AddToScheme(clientScheme); err != nil {
		return err
	}
	if err := platformv1alpha1.AddToScheme(clientScheme); err != nil {
		return err
	}
	c, err := client.New(cfg, client.Options{Scheme: clientScheme})
	if err != nil {
		return err
	}

	var checks []check
	add := func(name string, ok bool, detail string) {
		checks = append(checks, check{name: name, ok: ok, detail: detail})
	}
	warnIf := func(name string, ok bool, detail string) {
		checks = append(checks, check{name: name, ok: ok, detail: detail, warn: true})
	}

	for _, crd := range []string{"stacks.platform.kubo.io", "stackdefinitions.platform.kubo.io"} {
		var o apiextensionsv1.CustomResourceDefinition
		err := c.Get(ctx, client.ObjectKey{Name: crd}, &o)
		add("CRD "+crd, err == nil, yesNo(err == nil, "installed", "MISSING — apply config/crd"))
	}

	dc := discovery.NewDiscoveryClientForConfigOrDie(cfg)
	fluxOK := true
	for _, gv := range []string{"helm.toolkit.fluxcd.io/v2", "source.toolkit.fluxcd.io/v1"} {
		if _, err := dc.ServerResourcesForGroupVersion(gv); err != nil {
			fluxOK = false
		}
	}
	add("Flux helm/source APIs", fluxOK, yesNo(fluxOK, "available", "MISSING — flux install (needed for mode: Flux)"))

	var stacks platformv1alpha1.StackList
	if err := c.List(ctx, &stacks); err != nil {
		add("Stacks", false, "cannot list: "+err.Error())
	} else {
		add("Stacks", true, fmt.Sprintf("%d found", len(stacks.Items)))
		namespaces := map[string]bool{}

		for _, s := range stacks.Items {
			prefix := fmt.Sprintf("Stack %s/%s", s.Namespace, s.Name)
			namespaces[s.Namespace] = true

			var def platformv1alpha1.StackDefinition
			defErr := c.Get(ctx, client.ObjectKey{Name: s.Spec.StackRef}, &def)
			add(prefix+" definition", defErr == nil,
				yesNo(defErr == nil, s.Spec.StackRef, fmt.Sprintf("StackDefinition %q not found", s.Spec.StackRef)))

			phase := s.Status.Phase
			if phase == "" {
				phase = "Pending"
			}
			if phase != "Ready" {
				warnIf(prefix+" phase", false, phase)
			}
			for _, comp := range s.Status.Components {
				if comp.Phase == "Failed" || comp.Phase == "Degraded" {
					warnIf(prefix+" component "+comp.Name, false, string(comp.Phase)+": "+truncate(comp.Message, 120))
				}
			}

			if defErr == nil {
				seenSecrets := map[string]bool{}
				for _, comp := range def.Spec.Components {
					if comp.ChartPullSecretRef == nil || seenSecrets[comp.ChartPullSecretRef.Name] {
						continue
					}
					seenSecrets[comp.ChartPullSecretRef.Name] = true
					secName := comp.ChartPullSecretRef.Name
					secret := &unstructured.Unstructured{}
					secret.SetKind("Secret")
					secret.SetAPIVersion("v1")
					gerr := c.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: secName}, secret)
					warnIf(prefix+" chart pull secret "+secName, gerr == nil,
						yesNo(gerr == nil, "present", "MISSING — referenced by component "+comp.Name))
				}
			}
		}

		for ns := range namespaces {
			var list unstructured.UnstructuredList
			list.SetGroupVersionKind(externalSecretListGVK)
			if err := c.List(ctx, &list, client.InNamespace(ns)); err != nil {
				continue // ESO not installed or no permission
			}
			for _, item := range list.Items {
				ready, msg := externalSecretReady(&item)
				warnIf(fmt.Sprintf("ExternalSecret %s/%s", ns, item.GetName()), ready, msg)
			}
		}
	}

	return report(checks)
}

func externalSecretReady(obj *unstructured.Unstructured) (bool, string) {
	conds, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if !found {
		return false, "not reconciled"
	}
	ready := ""
	msg := ""
	for _, raw := range conds {
		cond, ok := raw.(map[string]interface{})
		if !ok || cond["type"] != "Ready" {
			continue
		}
		ready, _ = cond["status"].(string)
		msg, _ = cond["message"].(string)
		if msg == "" {
			msg, _ = cond["reason"].(string)
		}
	}
	if ready == "True" {
		return true, "synced"
	}
	return false, truncate(msg, 120)
}

func truncate(s string, n int) string {
	s = strings.SplitN(s, "\n", 2)[0]
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func yesNo(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}

func report(checks []check) error {
	failed, warned := 0, 0
	for _, c := range checks {
		switch {
		case c.ok:
			fmt.Printf(" %s %-48s %s\n", green+"✓"+reset, c.name, c.detail)
		case c.warn:
			warned++
			fmt.Printf(" %s %-48s %s\n", yellow+"!"+reset, c.name, c.detail)
		default:
			failed++
			fmt.Printf(" %s %-48s %s\n", red+"✗"+reset, c.name, c.detail)
		}
	}
	fmt.Printf("\n%d checks, %d failed, %d warnings\n", len(checks), failed, warned)
	if failed > 0 {
		return fmt.Errorf("%d checks failed", failed)
	}
	return nil
}
