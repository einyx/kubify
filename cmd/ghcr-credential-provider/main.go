// ghcr-credential-provider: kubelet image credential provider that mints a
// short-lived GHCR token from a GitHub App installation key. No cluster Secret.
//
// Install: drop the binary in imageCredentialProviderBinDir, point
// CredentialProviderConfig at it, matchImages: ["ghcr.io"].
//
// Env:
//   GITHUB_APP_ID           numeric app id
//   GITHUB_INSTALLATION_ID  numeric installation id
//   GITHUB_APP_KEY_FILE     path to the app's PEM private key (node-local)
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

type resp struct {
	Kind          string                 `json:"kind"`
	APIVersion    string                 `json:"apiVersion"`
	CacheKeyType  string                 `json:"cacheKeyType"`
	CacheDuration string                 `json:"cacheDuration"`
	Auth          map[string]authEntry   `json:"auth"`
}
type authEntry struct{ Username, Password string }

func main() {
	if _, err := io.ReadAll(os.Stdin); err != nil { die(err) } // drain request

	appID := os.Getenv("GITHUB_APP_ID")
	instID := os.Getenv("GITHUB_INSTALLATION_ID")
	keyPath := os.Getenv("GITHUB_APP_KEY_FILE")
	if appID == "" || instID == "" || keyPath == "" {
		die(fmt.Errorf("GITHUB_APP_ID, GITHUB_INSTALLATION_ID, GITHUB_APP_KEY_FILE required"))
	}

	key, err := loadKey(keyPath)
	if err != nil { die(err) }
	jwt, err := appJWT(appID, key)
	if err != nil { die(err) }
	tok, exp, err := installToken(instID, jwt)
	if err != nil { die(err) }

	// cache until ~5m before GitHub expiry
	cache := time.Until(exp) - 5*time.Minute
	if cache < time.Minute { cache = time.Minute }

	out := resp{
		Kind: "CredentialProviderResponse",
		APIVersion: "credentialprovider.kubelet.k8s.io/v1",
		CacheKeyType: "Registry",
		CacheDuration: cache.Round(time.Second).String(),
		Auth: map[string]authEntry{"ghcr.io": {Username: "x-access-token", Password: tok}},
	}
	json.NewEncoder(os.Stdout).Encode(out)
}

func loadKey(path string) (*rsa.PrivateKey, error) {
	b, err := os.ReadFile(path); if err != nil { return nil, err }
	blk, _ := pem.Decode(b); if blk == nil { return nil, fmt.Errorf("bad PEM") }
	k, err := x509.ParsePKCS1PrivateKey(blk.Bytes)
	if err != nil {
		any, err2 := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if err2 != nil { return nil, err }
		k = any.(*rsa.PrivateKey)
	}
	return k, nil
}

func appJWT(appID string, key *rsa.PrivateKey) (string, error) {
	now := time.Now().Unix()
	hdr := b64(`{"alg":"RS256","typ":"JWT"}`)
	body := b64(fmt.Sprintf(`{"iat":%d,"exp":%d,"iss":"%s"}`, now-60, now+540, appID))
	signing := hdr + "." + body
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, 5 /*crypto.SHA256*/, sum[:])
	if err != nil { return "", err }
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func installToken(instID, jwt string) (string, time.Time, error) {
	url := "https://api.github.com/app/installations/" + instID + "/access_tokens"
	req, _ := http.NewRequest("POST", url, bytes.NewReader(nil))
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	r, err := http.DefaultClient.Do(req); if err != nil { return "", time.Time{}, err }
	defer r.Body.Close()
	if r.StatusCode/100 != 2 {
		b, _ := io.ReadAll(r.Body)
		return "", time.Time{}, fmt.Errorf("github %d: %s", r.StatusCode, b)
	}
	var out struct{ Token string; ExpiresAt time.Time `json:"expires_at"` }
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil { return "", time.Time{}, err }
	return out.Token, out.ExpiresAt, nil
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
func die(err error)       { fmt.Fprintln(os.Stderr, err); os.Exit(1) }

// ponytail: hand-rolled JWT to avoid a jwt lib dep; swap for github.com/golang-jwt/jwt if signing needs grow.
var _ = strconv.Itoa
