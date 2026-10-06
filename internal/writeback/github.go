package writeback

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type GitHub struct {
	Token, APIBase string
	HTTPClient     *http.Client
}
type PullRequest struct{ URL, Branch, Commit string }

func (g *GitHub) CreateFeatureFlagPR(ctx context.Context, repoURL, file, namespace, name string, expected, desired map[string]string, title, branch string) (PullRequest, error) {
	owner, repo, err := githubRepo(repoURL)
	if err != nil {
		return PullRequest{}, err
	}
	base := g.APIBase
	if base == "" {
		base = "https://api.github.com"
	}
	var info struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err = g.do(ctx, "GET", fmt.Sprintf("%s/repos/%s/%s", base, owner, repo), nil, &info); err != nil {
		return PullRequest{}, err
	}
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err = g.do(ctx, "GET", fmt.Sprintf("%s/repos/%s/%s/git/ref/heads/%s", base, owner, repo, url.PathEscape(info.DefaultBranch)), nil, &ref); err != nil {
		return PullRequest{}, err
	}
	if err = g.do(ctx, "POST", fmt.Sprintf("%s/repos/%s/%s/git/refs", base, owner, repo), map[string]string{"ref": "refs/heads/" + branch, "sha": ref.Object.SHA}, nil); err != nil {
		return PullRequest{}, err
	}
	var content struct{ Content, SHA string }
	if err = g.do(ctx, "GET", fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s", base, owner, repo, escapePath(file), url.QueryEscape(branch)), nil, &content); err != nil {
		return PullRequest{}, err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(content.Content, "\n", ""))
	if err != nil {
		return PullRequest{}, err
	}
	updated, err := MutateFeatureFlags(raw, namespace, name, expected, desired)
	if err != nil {
		return PullRequest{}, err
	}
	var commit struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	payload := map[string]interface{}{"message": title, "content": base64.StdEncoding.EncodeToString(updated), "sha": content.SHA, "branch": branch}
	if err = g.do(ctx, "PUT", fmt.Sprintf("%s/repos/%s/%s/contents/%s", base, owner, repo, escapePath(file)), payload, &commit); err != nil {
		return PullRequest{}, err
	}
	var pr struct {
		HTMLURL string `json:"html_url"`
	}
	if err = g.do(ctx, "POST", fmt.Sprintf("%s/repos/%s/%s/pulls", base, owner, repo), map[string]string{"title": title, "head": branch, "base": info.DefaultBranch, "body": "Created by the Kubify operator console.\n\nThis change updates only `spec.featureFlags`."}, &pr); err != nil {
		return PullRequest{}, err
	}
	return PullRequest{URL: pr.HTMLURL, Branch: branch, Commit: commit.Commit.SHA}, nil
}
func (g *GitHub) do(ctx context.Context, method, u string, in, out interface{}) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(ctx, method, u, body)
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := g.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GitHub %s %s: %s: %s", method, u, resp.Status, string(b))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}
func githubRepo(s string) (string, string, error) {
	s = strings.TrimSuffix(s, ".git")
	s = strings.TrimPrefix(s, "git@github.com:")
	s = strings.TrimPrefix(s, "https://github.com/")
	p := strings.Split(s, "/")
	if len(p) != 2 {
		return "", "", fmt.Errorf("unsupported GitHub repository URL %q", s)
	}
	return p[0], p[1], nil
}
func escapePath(s string) string {
	parts := strings.Split(strings.TrimPrefix(s, "/"), "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}
