package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

var deviceId = uuid.NewString()

// tokenURL is a variable so tests can point it at a local server.
var tokenURL = "https://www.crunchyroll.com/auth/v1/token"

// errAuthRejected marks a token request Crunchyroll refused, which almost
// always means the etp_rt cookie is wrong or expired.
var errAuthRejected = errors.New("crunchyroll rejected the etp_rt cookie")

// reUUID matches the canonical 8-4-4-4-12 form the etp_rt value takes, so a
// paste that wraps it in extra text can still be rescued.
var reUUID = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// sanitizeEtpRt reduces whatever was pasted into the cookie field to the bare
// cookie value Crunchyroll expects. Pasting rarely gives just the value: the
// browser may hand back "etp_rt=<value>", "etp_rt:\"<value>\"", a whole
// Cookie header, or a JSON-ish "\"etp_rt\": \"<value>\"" line, plus stray
// whitespace. Stored verbatim, every one of those is rejected as invalid_grant.
func sanitizeEtpRt(raw string) string {
	s := strings.TrimSpace(raw)
	// A whole header or a list of cookies: only the first pair can be ours.
	if i := strings.Index(s, ";"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	s = strings.Trim(s, "\"'")
	// Drop a leading "etp_rt=" / "etp_rt:" name, if there is one.
	if strings.HasPrefix(s, "etp_rt") {
		rest := strings.TrimSpace(s[len("etp_rt"):])
		if strings.HasPrefix(rest, "=") || strings.HasPrefix(rest, ":") {
			s = strings.TrimSpace(rest[1:])
		}
	}
	s = strings.Trim(strings.TrimSpace(s), "\"'")
	// Anything still carrying extra text around the value (a JSON line, say)
	// keeps only the UUID inside it.
	if m := reUUID.FindString(s); m != "" && strings.TrimSpace(s) != m {
		return m
	}
	return s
}

type CrunchyrollTokenResponse struct {
	AccessToken string `json:"access_token"`
}

// GetAccessToken fetches an access token from Crunchyroll.
//
// A bad cookie gets an HTTP 400 "invalid_grant" whose JSON still parses, so
// the status and the token itself are checked; otherwise an empty token is
// returned and every later request 401s.
func GetAccessToken(etpRt string) (string, error) {
	etpRt = sanitizeEtpRt(etpRt)
	body := url.Values{}
	body.Set("device_id", deviceId)
	body.Set("device_type", "Firefox on Linux")
	body.Set("grant_type", "etp_rt_cookie")

	req, err := http.NewRequest(http.MethodPost, tokenURL, strings.NewReader(body.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Basic bm9haWhkZXZtXzZpeWcwYThsMHE6")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:147.0) Gecko/20100101 Firefox/147.0")
	req.AddCookie(&http.Cookie{Name: "device_id", Value: deviceId})
	req.AddCookie(&http.Cookie{Name: "etp_rt", Value: etpRt})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to get access token: %w", err)
	}
	defer resp.Body.Close()

	res, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to get access token: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		return "", fmt.Errorf("%w (HTTP %d: %s)", errAuthRejected, resp.StatusCode, strings.TrimSpace(string(res)))
	default:
		return "", fmt.Errorf("failed to get access token: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(res)))
	}

	var result CrunchyrollTokenResponse
	if err := json.Unmarshal(res, &result); err != nil {
		return "", fmt.Errorf("failed to get access token: %w", err)
	}
	if result.AccessToken == "" {
		return "", errors.New("failed to get access token: the response had no access_token")
	}

	return result.AccessToken, nil
}
