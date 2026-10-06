package writeback

import (
	"context"
	"crypto"
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
	"strconv"
	"strings"
	"time"
)

// InstallationToken exchanges GitHub App credentials for a short-lived
// installation token. appID and installationID are decimal GitHub IDs.
func InstallationToken(ctx context.Context, appID, installationID string, privateKey []byte, hc *http.Client) (string, error) {
	if _, err := strconv.ParseInt(appID, 10, 64); err != nil {
		return "", fmt.Errorf("invalid GitHub App ID")
	}
	if _, err := strconv.ParseInt(installationID, 10, 64); err != nil {
		return "", fmt.Errorf("invalid GitHub installation ID")
	}
	block, _ := pem.Decode(privateKey)
	if block == nil {
		return "", fmt.Errorf("decode GitHub App private key")
	}
	var key *rsa.PrivateKey
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		var ok bool
		key, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return "", fmt.Errorf("GitHub App key is not RSA")
		}
	} else {
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return "", err
		}
	}
	now := time.Now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]interface{}{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": appID})
	payload := base64.RawURLEncoding.EncodeToString(claims)
	signing := header + "." + payload
	digest := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	jwt := signing + "." + base64.RawURLEncoding.EncodeToString(signature)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.github.com/app/installations/"+installationID+"/access_tokens", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("GitHub App token: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", fmt.Errorf("GitHub App returned an empty token")
	}
	return out.Token, nil
}
