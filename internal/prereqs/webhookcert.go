package prereqs

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// EnsureWebhookCert makes sure the webhook serving certificate Secret exists
// so the manager's certwatcher can boot on a bare cluster. If the Secret is
// missing, a self-signed certificate is generated (cert-manager or an admin
// can replace it later; the certwatcher reloads on change).
func EnsureWebhookCert(ctx context.Context, c client.Client, namespace, secretName, serviceName string) error {
	var sec corev1.Secret
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: secretName}, &sec)
	if err == nil && len(sec.Data["tls.crt"]) > 0 && len(sec.Data["tls.key"]) > 0 {
		return nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	certPEM, keyPEM, err := selfSignedCert(serviceName, namespace)
	if err != nil {
		return err
	}
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        secretName,
			Namespace:   namespace,
			Annotations: map[string]string{"kubo.io/managed": "self-signed-fallback"},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM},
	}
	if apierrors.IsNotFound(err) {
		return c.Create(ctx, desired)
	}
	sec.Data = desired.Data
	return c.Update(ctx, &sec)
}

func selfSignedCert(serviceName, namespace string) (certPEM, keyPEM []byte, err error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	cn := fmt.Sprintf("%s.%s.svc", serviceName, namespace)
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames: []string{
			cn,
			fmt.Sprintf("%s.%s.svc.cluster.local", serviceName, namespace),
			"localhost",
		},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(caKey)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}
