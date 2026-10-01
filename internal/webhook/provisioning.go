package webhook

import (
	"encoding/json"
	"net/http"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ProvisionRequest is the payload the website POSTs when a customer signs up.
type ProvisionRequest struct {
	CustomerName string         `json:"customer_name"`
	Namespace    string         `json:"namespace"`
	StackRef     string         `json:"stack_ref"`
	Values       map[string]any `json:"values,omitempty"`
}

// ProvisioningHandler creates a Stack CR for each inbound provision request.
type ProvisioningHandler struct {
	Client    client.Client
	SecretKey string
}

func (h *ProvisioningHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())

	if r.Header.Get("X-Webhook-Secret") != h.SecretKey {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req ProvisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: req.Namespace}}
	if err := h.Client.Create(r.Context(), ns); err != nil && !errors.IsAlreadyExists(err) {
		logger.Error(err, "failed to create namespace", "namespace", req.Namespace)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	raw, _ := json.Marshal(req.Values)
	stack := &platformv1alpha1.Stack{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.CustomerName,
			Namespace: req.Namespace,
		},
		Spec: platformv1alpha1.StackSpec{
			StackRef: req.StackRef,
			Values:   apiextensionsv1.JSON{Raw: raw},
		},
	}

	if err := h.Client.Create(r.Context(), stack); err != nil {
		logger.Error(err, "failed to create Stack", "customer", req.CustomerName)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "provisioning", "name": req.CustomerName})
}
