package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/shopspring/decimal"
)

// commentMarker is appended to every comment we post so we can find and update
// our own previous comment instead of stacking new ones on each CI run.
const commentMarker = "<!-- terracost-cli -->"

func postPRComment(breakdown string, planned, increase decimal.Decimal) error {
	owner := os.Getenv("BASE_REPO_OWNER")
	name := os.Getenv("BASE_REPO_NAME")
	pull := os.Getenv("PULL_NUM")
	if owner == "" || name == "" || pull == "" {
		return fmt.Errorf("missing one of BASE_REPO_OWNER/BASE_REPO_NAME/PULL_NUM")
	}

	token, err := resolveGitHubToken()
	if err != nil {
		return err
	}

	body := fmt.Sprintf(
		"%s\n## Terraform cost estimate\n\n- **Planned monthly cost:** $%s/mo\n- **Monthly cost increase:** $%s/mo\n\n<details>\n<summary>Per-resource breakdown</summary>\n\n```\n%s```\n</details>\n",
		commentMarker, planned.StringFixed(2), increase.StringFixed(2), breakdown,
	)

	existingID, err := findExistingComment(token, owner, name, pull)
	if err != nil {
		return fmt.Errorf("looking up existing comment: %w", err)
	}
	if existingID != 0 {
		return updateComment(token, owner, name, existingID, body)
	}
	return createComment(token, owner, name, pull, body)
}

// resolveGitHubToken returns either GH_TOKEN if set, or mints a fresh installation
// token from the GitHub App credentials in env (Atlantis-compatible naming:
// GITHUB_APP_ID, GITHUB_APP_INSTALLATION_ID, GITHUB_APP_PEM_FILE — the latter
// accepts either a path to a PEM file or the PEM contents inlined).
func resolveGitHubToken() (string, error) {
	if t := os.Getenv("GH_TOKEN"); t != "" {
		return t, nil
	}
	appID := os.Getenv("GITHUB_APP_ID")
	installID := os.Getenv("GITHUB_APP_INSTALLATION_ID")
	keyPathOrPEM := os.Getenv("GITHUB_APP_PEM_FILE")
	if appID == "" || installID == "" || keyPathOrPEM == "" {
		return "", fmt.Errorf("set GH_TOKEN, or all of GITHUB_APP_ID/GITHUB_APP_INSTALLATION_ID/GITHUB_APP_PEM_FILE")
	}
	return mintInstallationToken(appID, installID, keyPathOrPEM)
}

func mintInstallationToken(appID, installID, keyPathOrPEM string) (string, error) {
	var keyBytes []byte
	if strings.HasPrefix(strings.TrimSpace(keyPathOrPEM), "-----BEGIN") {
		keyBytes = []byte(keyPathOrPEM)
	} else {
		var err error
		keyBytes, err = os.ReadFile(keyPathOrPEM)
		if err != nil {
			return "", fmt.Errorf("reading App PEM file: %w", err)
		}
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM(keyBytes)
	if err != nil {
		return "", fmt.Errorf("parsing App private key (PEM RSA expected): %w", err)
	}
	now := time.Now()
	claims := jwt.MapClaims{
		"iat": now.Add(-30 * time.Second).Unix(), // small clock-skew slack
		"exp": now.Add(9 * time.Minute).Unix(),   // GitHub max is 10m
		"iss": appID,
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
	if err != nil {
		return "", fmt.Errorf("signing App JWT: %w", err)
	}

	url := fmt.Sprintf("https://api.github.com/app/installations/%s/access_tokens", installID)
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+signed)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("minting installation token: status %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", fmt.Errorf("installation token response had empty token")
	}
	return out.Token, nil
}

// findExistingComment returns the id of our previously-posted comment on this PR,
// or 0 if none. Pages through all comments via the Link header.
func findExistingComment(token, owner, repo, pull string) (int64, error) {
	next := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%s/comments?per_page=100", owner, repo, pull)
	for next != "" {
		req, err := http.NewRequest(http.MethodGet, next, nil)
		if err != nil {
			return 0, err
		}
		req.Header.Set("Authorization", "token "+token)
		req.Header.Set("Accept", "application/vnd.github+json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err
		}
		if resp.StatusCode >= 300 {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return 0, fmt.Errorf("listing comments: status %d: %s", resp.StatusCode, b)
		}
		var comments []struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&comments); err != nil {
			resp.Body.Close()
			return 0, err
		}
		next = nextPageURL(resp.Header.Get("Link"))
		resp.Body.Close()

		for _, c := range comments {
			if strings.Contains(c.Body, commentMarker) {
				return c.ID, nil
			}
		}
	}
	return 0, nil
}

// nextPageURL extracts the rel="next" URL from a GitHub Link header, or "" if absent.
// Format: <https://...?page=2>; rel="next", <https://...?page=5>; rel="last"
func nextPageURL(link string) string {
	for _, part := range strings.Split(link, ",") {
		segs := strings.SplitN(strings.TrimSpace(part), ";", 2)
		if len(segs) != 2 {
			continue
		}
		if !strings.Contains(segs[1], `rel="next"`) {
			continue
		}
		u := strings.TrimSpace(segs[0])
		return strings.TrimSuffix(strings.TrimPrefix(u, "<"), ">")
	}
	return ""
}

func createComment(token, owner, repo, pull, body string) error {
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%s/comments", owner, repo, pull)
	return ghMutate(http.MethodPost, u, token, body)
}

func updateComment(token, owner, repo string, commentID int64, body string) error {
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/comments/%d", owner, repo, commentID)
	return ghMutate(http.MethodPatch, u, token, body)
}

func ghMutate(method, u, token, body string) error {
	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(method, u, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("github API %s %s: status %d: %s", method, u, resp.StatusCode, b)
	}
	return nil
}
