package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// defaultSearchURL is the keyless DuckDuckGo lite endpoint. Result anchors
// are duckduckgo.com redirect links whose real target is the uddg parameter.
const defaultSearchURL = "https://lite.duckduckgo.com/lite/?q={query}"

const (
	defaultWebTimeout = 30 * time.Second
	maxFetchBytes     = 256 * 1024
)

// WebClient performs outbound web searches and page fetches. It is nil-safe
// absent: a Sandbox without one reports the web tools as disabled, keeping
// runs offline by default.
type WebClient struct {
	client    *http.Client
	searchURL string

	// AllowLocal permits loopback/private hosts and non-standard ports.
	// Production defaults to false; tests point at httptest servers.
	AllowLocal bool
}

// NewWebClient builds a client using searchURL as the query template
// (must contain the "{query}" placeholder); empty means the built-in
// DuckDuckGo lite endpoint. A SearXNG instance is used when its template
// contains "format=json".
func NewWebClient(searchURL string, timeout time.Duration) (*WebClient, error) {
	if strings.TrimSpace(searchURL) == "" {
		searchURL = defaultSearchURL
	}
	if !strings.Contains(searchURL, "{query}") {
		return nil, fmt.Errorf("search URL must contain a {query} placeholder: %s", searchURL)
	}
	if timeout <= 0 {
		timeout = defaultWebTimeout
	}
	w := &WebClient{searchURL: searchURL}
	w.client = &http.Client{
		Timeout: timeout,
		// Re-validate every hop; redirects must not smuggle the fetch to a
		// private address.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("stopped after 5 redirects")
			}
			return w.validateURL(req.URL)
		},
	}
	return w, nil
}

// Search runs the query and returns compact numbered results:
// "1. <title>\n   <url>\n   <snippet>".
func (w *WebClient) Search(ctx context.Context, query string, max int) (string, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return "", fmt.Errorf("empty query")
	}
	if max <= 0 {
		max = 5
	}

	endpoint := strings.ReplaceAll(w.searchURL, "{query}", url.QueryEscape(query))
	body, err := w.get(ctx, endpoint)
	if err != nil {
		return "", fmt.Errorf("search request failed: %w", err)
	}

	var results [][3]string
	if strings.Contains(w.searchURL, "format=json") {
		if results, err = parseSearXNG(body); err != nil {
			return "", err
		}
	} else {
		results = parseDDGLite(body)
	}
	if len(results) == 0 {
		return "(no results — the search endpoint may have changed or blocked the request)", nil
	}

	var b strings.Builder
	for i, r := range results {
		if i >= max {
			break
		}
		fmt.Fprintf(&b, "%d. %s\n   %s\n   %s\n", i+1, r[0], r[1], collapseWS(r[2]))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// Fetch downloads a page and returns readable text: HTML is converted to
// plain text, other text formats pass through.
func (w *WebClient) Fetch(ctx context.Context, rawURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}
	if err := w.validateURL(u); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "go_llm_engine/1.0 (+web fetch)")
	resp, err := w.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes))
	if err != nil {
		return "", err
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	switch {
	case strings.Contains(ct, "html"):
		return strings.TrimSpace(htmlToText(string(data))), nil
	case strings.HasPrefix(ct, "text/"), strings.Contains(ct, "json"), strings.Contains(ct, "xml"):
		return strings.TrimSpace(string(data)), nil
	default:
		return "", fmt.Errorf("unsupported content type %q (only HTML/text/JSON/XML)", ct)
	}
}

// get performs a GET and returns the body. The operator-configured search
// endpoint is trusted even when it resolves locally (e.g. an internal
// SearXNG), so it bypasses validateURL.
func (w *WebClient) get(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "go_llm_engine/1.0 (+web search)")
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 4*maxFetchBytes))
}

// validateURL enforces the fetch safety policy: public schemes, non-private
// destination IPs, and standard ports.
func (w *WebClient) validateURL(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("only http/https URLs are allowed, got %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("URL is missing a host")
	}
	port := u.Port()
	switch port {
	case "", "80", "443":
	default:
		if !w.AllowLocal {
			return fmt.Errorf("port %q is not allowed (80/443 only)", port)
		}
	}

	if w.AllowLocal {
		return nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(context.Background(), host)
	if err != nil {
		return fmt.Errorf("cannot resolve %q: %w", host, err)
	}
	for _, ip := range ips {
		if !isPublicIP(ip.IP) {
			return fmt.Errorf("host %q resolves to a non-public address %s", host, ip.IP)
		}
	}
	return nil
}

// isPublicIP reports whether ip is outside loopback/private/reserved ranges.
func isPublicIP(ip net.IP) bool {
	switch {
	case ip.IsLoopback(), ip.IsPrivate(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(),
		ip.IsUnspecified(), ip.IsMulticast():
		return false
	}
	// CGNAT 100.64.0.0/10 and benchmarking 198.18.0.0/15.
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return false
		}
		if v4[0] == 198 && (v4[1] == 18 || v4[1] == 19) {
			return false
		}
	}
	return true
}

var (
	// ddgLink matches result anchors regardless of attribute order; the
	// href is a redirect whose uddg query parameter holds the real URL.
	ddgLink = regexp.MustCompile(`(?is)<a([^>]*?)class="result-link"[^>]*>(.*?)</a>`)
	ddgHref = regexp.MustCompile(`(?is)href="([^"]+)"`)
	ddgSnp  = regexp.MustCompile(`(?is)<td[^>]+class="result-snippet"[^>]*>(.*?)</td>`)
	// RE2 has no backreferences, so script/style blocks are stripped by two
	// separate expressions.
	scriptTagRe = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	styleTagRe  = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
)

// stripBlockTags removes script and style element contents.
func stripBlockTags(s string) string {
	s = scriptTagRe.ReplaceAllString(s, " ")
	return styleTagRe.ReplaceAllString(s, " ")
}

// parseDDGLite extracts title/url/snippet triples from the lite HTML page.
func parseDDGLite(body []byte) [][3]string {
	s := string(body)
	links := ddgLink.FindAllStringSubmatch(s, -1)
	snips := ddgSnp.FindAllStringSubmatch(s, -1)

	out := make([][3]string, 0, len(links))
	for i, m := range links {
		href := ""
		if hm := ddgHref.FindStringSubmatch(m[1]); hm != nil {
			href = html.UnescapeString(hm[1])
		}
		title := collapseWS(htmlToText(m[2]))
		target := href
		if ru, err := url.Parse(strings.TrimPrefix(href, "//")); err == nil {
			if uddg := ru.Query().Get("uddg"); uddg != "" {
				target = uddg
			}
		}
		if strings.HasPrefix(target, "//") {
			target = "https:" + target
		}
		snippet := ""
		if i < len(snips) {
			snippet = htmlToText(snips[i][1])
		}
		out = append(out, [3]string{title, target, snippet})
	}
	return out
}

type searXNGResponse struct {
	Results []struct {
		URL     string `json:"url"`
		Title   string `json:"title"`
		Content string `json:"content"`
	} `json:"results"`
}

// parseSearXNG parses the JSON response of a SearXNG instance
// ("?format=json").
func parseSearXNG(body []byte) ([][3]string, error) {
	var r searXNGResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("invalid SearXNG JSON: %w", err)
	}
	out := make([][3]string, 0, len(r.Results))
	for _, item := range r.Results {
		out = append(out, [3]string{item.Title, item.URL, item.Content})
	}
	return out, nil
}

// htmlToText strips scripts and tags from HTML and unescapes entities.
func htmlToText(s string) string {
	s = stripBlockTags(s)
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
			b.WriteByte(' ')
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return collapseWS(html.UnescapeString(b.String()))
}

func collapseWS(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}
