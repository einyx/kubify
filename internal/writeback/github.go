package writeback

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

type githubError struct {
	Method, URL, Status, Body string
	StatusCode                int
}

func (e *githubError) Error() string {
	return fmt.Sprintf("GitHub %s %s: %s: %s", e.Method, e.URL, e.Status, e.Body)
}

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
	if existing, err := g.openPullRequest(ctx, base, owner, repo, branch); err != nil {
		return PullRequest{}, err
	} else if existing.URL != "" {
		return existing, nil
	}
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err = g.do(ctx, "GET", fmt.Sprintf("%s/repos/%s/%s/git/ref/heads/%s", base, owner, repo, url.PathEscape(info.DefaultBranch)), nil, &ref); err != nil {
		return PullRequest{}, err
	}
	branchSHA := ref.Object.SHA
	if err = g.do(ctx, "POST", fmt.Sprintf("%s/repos/%s/%s/git/refs", base, owner, repo), map[string]string{"ref": "refs/heads/" + branch, "sha": ref.Object.SHA}, nil); err != nil {
		var ghErr *githubError
		if !errors.As(err, &ghErr) || ghErr.StatusCode != http.StatusUnprocessableEntity {
			return PullRequest{}, err
		}
		var branchRef struct {
			Object struct {
				SHA string `json:"sha"`
			} `json:"object"`
		}
		if err = g.do(ctx, "GET", fmt.Sprintf("%s/repos/%s/%s/git/ref/heads/%s", base, owner, repo, url.PathEscape(branch)), nil, &branchRef); err != nil {
			return PullRequest{}, err
		}
		branchSHA = branchRef.Object.SHA
	}
	commitSHA := branchSHA
	// A request-specific branch ahead of the base means a previous reconcile
	// committed the mutation but stopped before recording or creating the PR.
	if branchSHA == ref.Object.SHA {
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
		commitSHA = commit.Commit.SHA
	}
	var pr struct {
		HTMLURL string `json:"html_url"`
	}
	if err = g.do(ctx, "POST", fmt.Sprintf("%s/repos/%s/%s/pulls", base, owner, repo), map[string]string{"title": title, "head": branch, "base": info.DefaultBranch, "body": "Created by the Kubify operator console.\n\nThis change updates only `spec.featureFlags`."}, &pr); err != nil {
		if existing, findErr := g.openPullRequest(ctx, base, owner, repo, branch); findErr == nil && existing.URL != "" {
			return existing, nil
		}
		return PullRequest{}, err
	}
	return PullRequest{URL: pr.HTMLURL, Branch: branch, Commit: commitSHA}, nil
}

func (g *GitHub) openPullRequest(ctx context.Context, base, owner, repo, branch string) (PullRequest, error) {
	var pulls []struct {
		HTMLURL string `json:"html_url"`
		Head    struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	u := fmt.Sprintf("%s/repos/%s/%s/pulls?state=open&head=%s", base, owner, repo, url.QueryEscape(owner+":"+branch))
	if err := g.do(ctx, "GET", u, nil, &pulls); err != nil {
		return PullRequest{}, err
	}
	if len(pulls) == 0 {
		return PullRequest{}, nil
	}
	return PullRequest{URL: pulls[0].HTMLURL, Branch: branch, Commit: pulls[0].Head.SHA}, nil
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
		return &githubError{Method: method, URL: u, Status: resp.Status, Body: string(b), StatusCode: resp.StatusCode}
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
