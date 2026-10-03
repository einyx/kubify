// Command bootstrap-dev runs ONLY the StackBootstrap reconciler against the
// current kubeconfig — a local test harness so the CRD can be exercised
// without rebuilding/pushing the in-cluster operator (which would race the
// deployed operator over Stack resources).
package main

import (
	"flag"
	"os"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	"github.com/einyx/kubo/internal/controller"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

func main() {
	flag.Parse()

	sch := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sch); err != nil {
		os.Exit(1)
	}
	if err := corev1.AddToScheme(sch); err != nil {
		os.Exit(1)
	}
	if err := platformv1alpha1.AddToScheme(sch); err != nil {
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(config.GetConfigOrDie(), ctrl.Options{
		Scheme: sch,
	})
	if err != nil {
		os.Exit(1)
	}
	if err = (&controller.StackBootstrapReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		os.Exit(1)
	}
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		os.Exit(1)
	}
}
