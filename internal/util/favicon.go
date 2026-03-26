package util

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"lantern/internal/store"
)

var faviconClient = &http.Client{
	Timeout: 5 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		DisableKeepAlives: true,
	},
}

var (
	reFaviconHref     = regexp.MustCompile(`(?i)<link[^>]+rel=["'][^"']*icon[^"']*["'][^>]+href=["']([^"']+)["']`)
	reFaviconHref2    = regexp.MustCompile(`(?i)<link[^>]+href=["']([^"']+)["'][^>]+rel=["'][^"']*icon[^"']*["']`)
	reAppleTouchIcon  = regexp.MustCompile(`(?i)<link[^>]+rel=["']apple-touch-icon["'][^>]+href=["']([^"']+)["']`)
	reAppleTouchIcon2 = regexp.MustCompile(`(?i)<link[^>]+href=["']([^"']+)["'][^>]+rel=["']apple-touch-icon["']`)
)

// FetchFaviconForTarget fetches the page at targetURL, extracts the favicon
// link, fetches the favicon, and returns the raw image bytes.
// Returns nil if no favicon is found or the fetch fails.
func FetchFaviconForTarget(ctx context.Context, targetURL string) []byte {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil
	}
	resp, err := faviconClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil
	}
	return FetchFaviconFromHTML(ctx, string(body), targetURL)
}

// FetchFaviconFromHTML extracts the favicon URL from pre-fetched HTML and
// fetches the favicon bytes. Use this when the page HTML is already available.
func FetchFaviconFromHTML(ctx context.Context, html, baseURL string) []byte {
	return fetchFaviconBytes(ctx, extractFaviconURL(html, baseURL))
}

// FetchAndWriteFavicon fetches the favicon for target and writes it to the
// store under id. Returns true if data was fetched and written successfully.
// The caller is responsible for updating the entity's Icon field.
func FetchAndWriteFavicon(ctx context.Context, st *store.Store, id, target string) bool {
	data := FetchFaviconForTarget(ctx, target)
	if len(data) == 0 {
		return false
	}
	return st.WriteIcon(id, data) == nil
}

func fetchFaviconBytes(ctx context.Context, faviconURL string) []byte {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, faviconURL, nil)
	if err != nil {
		return nil
	}
	resp, err := faviconClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return nil
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil || len(data) == 0 {
		return nil
	}
	return data
}

func extractFaviconURL(html, baseURL string) string {
	// Prefer apple-touch-icon: always a high-quality PNG, no ICO format artifacts.
	for _, pair := range [][2]*regexp.Regexp{
		{reAppleTouchIcon, reAppleTouchIcon2},
		{reFaviconHref, reFaviconHref2},
	} {
		for _, re := range pair {
			if m := re.FindStringSubmatch(html); len(m) >= 2 {
				return resolveRef(m[1], baseURL)
			}
		}
	}
	return resolveRef("/favicon.ico", baseURL)
}

func resolveRef(ref, base string) string {
	if strings.HasPrefix(ref, "//") {
		bu, _ := url.Parse(base)
		return bu.Scheme + ":" + ref
	}
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}
	bu, err := url.Parse(base)
	if err != nil {
		return ref
	}
	rel, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return bu.ResolveReference(rel).String()
}
