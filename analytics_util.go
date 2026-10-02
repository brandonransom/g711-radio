package main

import (
	"crypto/rand"
	"encoding/base32"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// anonymizeIP reduces a client address to its network: IPv4 to the /24 (the
// first three octets) and IPv6 to the /48 (a typical site allocation). The
// full address is never written anywhere by the analytics code.
func anonymizeIP(ip string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return ""
	}
	addr = addr.WithZone("").Unmap()
	bits := 48
	if addr.Is4() {
		bits = 24
	}
	prefix, err := addr.Prefix(bits)
	if err != nil {
		return ""
	}
	return prefix.String()
}

type userAgentInfo struct {
	Browser string
	OS      string
	Device  string // desktop, mobile, tablet, bot
}

var botMarkers = []string{
	"bot", "crawl", "spider", "slurp", "curl/", "wget/", "python-", "python/",
	"go-http-client", "headless", "httpclient", "okhttp", "java/", "libwww",
	"scrapy", "facebookexternalhit", "preview", "monitor", "uptime",
}

// parseUserAgent reduces a User-Agent string to coarse browser, OS, and
// device categories. Only these categories are stored, never the raw string.
func parseUserAgent(ua string) userAgentInfo {
	l := strings.ToLower(ua)
	if l == "" {
		return userAgentInfo{Browser: "Unknown", OS: "Unknown", Device: "bot"}
	}
	for _, m := range botMarkers {
		if strings.Contains(l, m) {
			return userAgentInfo{Browser: "Bot", OS: "Bot", Device: "bot"}
		}
	}

	info := userAgentInfo{Browser: "Other", OS: "Other", Device: "desktop"}
	switch {
	case strings.Contains(l, "edg/") || strings.Contains(l, "edgios/") || strings.Contains(l, "edga/"):
		info.Browser = "Edge"
	case strings.Contains(l, "opr/") || strings.Contains(l, "opera"):
		info.Browser = "Opera"
	case strings.Contains(l, "samsungbrowser/"):
		info.Browser = "Samsung Internet"
	case strings.Contains(l, "firefox/") || strings.Contains(l, "fxios/"):
		info.Browser = "Firefox"
	case strings.Contains(l, "crios/") || strings.Contains(l, "chrome/") || strings.Contains(l, "chromium/"):
		info.Browser = "Chrome"
	case strings.Contains(l, "safari/"):
		info.Browser = "Safari"
	}

	switch {
	case strings.Contains(l, "windows"):
		info.OS = "Windows"
	case strings.Contains(l, "iphone") || strings.Contains(l, "ipad") || strings.Contains(l, "ipod"):
		info.OS = "iOS"
	case strings.Contains(l, "android"):
		info.OS = "Android"
	case strings.Contains(l, "cros"):
		info.OS = "ChromeOS"
	case strings.Contains(l, "mac os x") || strings.Contains(l, "macintosh"):
		info.OS = "macOS"
	case strings.Contains(l, "linux"):
		info.OS = "Linux"
	}

	switch {
	case strings.Contains(l, "ipad") || strings.Contains(l, "tablet") ||
		(strings.Contains(l, "android") && !strings.Contains(l, "mobile")):
		info.Device = "tablet"
	case strings.Contains(l, "mobi") || strings.Contains(l, "iphone") || strings.Contains(l, "android"):
		info.Device = "mobile"
	}
	return info
}

// pageLabel names a user-facing page for page-view counting. ok is false for
// anything that isn't a page (scripts, icons) or for the favorites iframe
// that index.html embeds, which would otherwise double-count the home page.
func pageLabel(r *http.Request) (string, bool) {
	p := r.URL.Path
	switch {
	case p == "/" || p == "/index.html":
		return "/", true
	case p == "/section.html":
		q := r.URL.Query()
		if q.Get("embed") == "1" {
			return "", false
		}
		switch {
		case q.Get("favorites") == "1":
			return "/section.html (favorites)", true
		case q.Get("all") == "1":
			return "/section.html (all streams)", true
		case q.Get("name") != "":
			return "/section.html (" + q.Get("state") + " / " + q.Get("group") + " / " + q.Get("name") + ")", true
		case q.Get("state") != "":
			return "/section.html (" + q.Get("state") + ")", true
		}
		return "/section.html", true
	case strings.HasSuffix(p, ".html"):
		return p, true
	}
	return "", false
}

// externalReferrer returns the referring host when it is a different site,
// so internal navigation between this site's own pages is not counted.
func externalReferrer(r *http.Request) string {
	ref := r.Referer()
	if ref == "" {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil || u.Host == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if strings.EqualFold(host, hostWithoutPort(r.Host)) {
		return ""
	}
	return strings.TrimPrefix(host, "www.")
}

func hostWithoutPort(h string) string {
	if i := strings.LastIndex(h, ":"); i != -1 && !strings.Contains(h[i:], "]") {
		return h[:i]
	}
	return h
}

// randomToken returns an unguessable URL-safe string (20 base32 chars,
// 100 bits), used to suggest a dashboardPath.
func randomToken() string {
	b := make([]byte, 13)
	_, _ = rand.Read(b)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))[:20]
}

// statusRecorder captures the status code a handler wrote so middleware only
// counts requests that actually succeeded.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
