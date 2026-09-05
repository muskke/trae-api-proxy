package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	defaultDuckDuckGoEndpoint = "https://html.duckduckgo.com/html/"
	defaultUserAgent          = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/140 Safari/537.36"
)

type Config struct {
	Backend      string
	Endpoint     string
	Timeout      time.Duration
	MaxResults   int
	MaxPageBytes int64
	UserAgent    string
}

type Runtime struct {
	cfg    Config
	client *http.Client
}

type ToolOptions struct {
	SearchContextSize string
	AllowedDomains    []string
	UserLocation      map[string]any
	ExternalWebAccess *bool
	IndexedWebAccess  *bool
}

type Action struct {
	Type    string   `json:"type"`
	Query   string   `json:"query,omitempty"`
	Queries []string `json:"queries,omitempty"`
	URL     string   `json:"url,omitempty"`
	Pattern string   `json:"pattern,omitempty"`
}

type Source struct {
	Title   string `json:"title,omitempty"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}

type Result struct {
	Action  Action   `json:"action"`
	Sources []Source `json:"sources,omitempty"`
	Content string   `json:"content,omitempty"`
}

func New(cfg Config) *Runtime {
	if strings.TrimSpace(cfg.Backend) == "" {
		cfg.Backend = "duckduckgo"
	}
	cfg.Backend = strings.ToLower(strings.TrimSpace(cfg.Backend))
	if strings.TrimSpace(cfg.Endpoint) == "" {
		cfg.Endpoint = defaultDuckDuckGoEndpoint
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 20 * time.Second
	}
	if cfg.MaxResults <= 0 {
		cfg.MaxResults = 8
	}
	if cfg.MaxPageBytes <= 0 {
		cfg.MaxPageBytes = 2 << 20
	}
	if strings.TrimSpace(cfg.UserAgent) == "" {
		cfg.UserAgent = defaultUserAgent
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyFromEnvironment
	transport.ForceAttemptHTTP2 = true
	transport.MaxIdleConns = 50
	transport.MaxIdleConnsPerHost = 10
	transport.IdleConnTimeout = 60 * time.Second

	client := &http.Client{
		Timeout:   cfg.Timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 8 {
				return errors.New("too many redirects")
			}
			return validatePublicURLContext(req.Context(), req.URL)
		},
	}
	return &Runtime{cfg: cfg, client: client}
}

func (r *Runtime) Backend() string {
	if r == nil {
		return ""
	}
	return r.cfg.Backend
}

func (r *Runtime) Execute(ctx context.Context, action Action, options ToolOptions) (Result, error) {
	action.Type = strings.ToLower(strings.TrimSpace(action.Type))
	switch action.Type {
	case "search", "":
		if action.Type == "" {
			action.Type = "search"
		}
		return r.search(ctx, action, options)
	case "open_page":
		return r.openPage(ctx, action)
	case "find_in_page":
		return r.findInPage(ctx, action)
	default:
		return Result{}, fmt.Errorf("unsupported web search action %q", action.Type)
	}
}

func (r *Runtime) search(ctx context.Context, action Action, options ToolOptions) (Result, error) {
	queries := make([]string, 0, len(action.Queries)+1)
	if q := strings.TrimSpace(action.Query); q != "" {
		queries = append(queries, q)
	}
	for _, q := range action.Queries {
		if q = strings.TrimSpace(q); q != "" {
			queries = append(queries, q)
		}
	}
	if len(queries) == 0 {
		return Result{}, errors.New("web search requires query or queries")
	}
	if len(queries) > 4 {
		queries = queries[:4]
	}

	maxResults := r.maxResultsForContext(options.SearchContextSize)
	if maxResults < 1 {
		maxResults = 1
	}
	seen := map[string]bool{}
	sources := make([]Source, 0, maxResults)
	for _, query := range queries {
		batch, err := r.searchOne(ctx, query, maxResults, options.AllowedDomains)
		if err != nil {
			if len(sources) > 0 {
				break
			}
			return Result{}, err
		}
		for _, source := range batch {
			if source.URL == "" || seen[source.URL] {
				continue
			}
			seen[source.URL] = true
			sources = append(sources, source)
			if len(sources) >= maxResults {
				break
			}
		}
		if len(sources) >= maxResults {
			break
		}
	}

	resultAction := action
	if resultAction.Query == "" && len(queries) > 0 {
		resultAction.Query = queries[0]
	}
	return Result{Action: resultAction, Sources: sources}, nil
}

func (r *Runtime) searchOne(ctx context.Context, query string, limit int, allowedDomains []string) ([]Source, error) {
	switch r.cfg.Backend {
	case "duckduckgo", "ddg", "auto":
		return r.searchDuckDuckGo(ctx, query, limit, allowedDomains)
	case "searxng":
		return r.searchSearXNG(ctx, query, limit, allowedDomains)
	default:
		return nil, fmt.Errorf("unsupported web search backend %q", r.cfg.Backend)
	}
}

func (r *Runtime) searchDuckDuckGo(ctx context.Context, query string, limit int, allowedDomains []string) ([]Source, error) {
	endpoint, err := url.Parse(r.cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse DuckDuckGo endpoint: %w", err)
	}
	values := endpoint.Query()
	values.Set("q", query)
	endpoint.RawQuery = values.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", r.cfg.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "en-US,en;q=0.8")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("DuckDuckGo search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("DuckDuckGo search returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, r.cfg.MaxPageBytes))
	if err != nil {
		return nil, fmt.Errorf("read DuckDuckGo search results: %w", err)
	}
	results := parseDuckDuckGoHTML(string(body))
	results = filterDomains(results, allowedDomains)
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func (r *Runtime) searchSearXNG(ctx context.Context, query string, limit int, allowedDomains []string) ([]Source, error) {
	endpoint, err := url.Parse(r.cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse SearXNG endpoint: %w", err)
	}
	path := strings.TrimRight(endpoint.Path, "/")
	if !strings.HasSuffix(path, "/search") {
		path += "/search"
	}
	endpoint.Path = path
	values := endpoint.Query()
	values.Set("q", query)
	values.Set("format", "json")
	endpoint.RawQuery = values.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", r.cfg.UserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("SearXNG search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("SearXNG search returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var raw struct {
		Results []struct {
			URL     string `json:"url"`
			Title   string `json:"title"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, r.cfg.MaxPageBytes)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode SearXNG response: %w", err)
	}
	out := make([]Source, 0, len(raw.Results))
	for _, item := range raw.Results {
		source := Source{Title: strings.TrimSpace(item.Title), URL: strings.TrimSpace(item.URL), Snippet: cleanText(item.Content)}
		if source.URL != "" {
			out = append(out, source)
		}
	}
	out = filterDomains(out, allowedDomains)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *Runtime) openPage(ctx context.Context, action Action) (Result, error) {
	target, err := url.Parse(strings.TrimSpace(action.URL))
	if err != nil || target == nil {
		return Result{}, errors.New("open_page requires a valid url")
	}
	if err := validatePublicURLContext(ctx, target); err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("User-Agent", r.cfg.UserAgent)
	req.Header.Set("Accept", "text/html,text/plain,application/xhtml+xml;q=0.9,*/*;q=0.2")
	resp, err := r.client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("open web page: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Result{}, fmt.Errorf("open web page returned HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, r.cfg.MaxPageBytes))
	if err != nil {
		return Result{}, fmt.Errorf("read web page: %w", err)
	}
	content := cleanPageText(string(raw))
	if len(content) > 18000 {
		content = content[:18000]
	}
	action.URL = resp.Request.URL.String()
	return Result{Action: action, Content: content, Sources: []Source{{URL: action.URL}}}, nil
}

func (r *Runtime) findInPage(ctx context.Context, action Action) (Result, error) {
	pattern := strings.TrimSpace(action.Pattern)
	if pattern == "" {
		return Result{}, errors.New("find_in_page requires pattern")
	}
	page, err := r.openPage(ctx, Action{Type: "open_page", URL: action.URL})
	if err != nil {
		return Result{}, err
	}
	text := page.Content
	lower := strings.ToLower(text)
	needle := strings.ToLower(pattern)
	var matches []string
	offset := 0
	for len(matches) < 8 {
		idx := strings.Index(lower[offset:], needle)
		if idx < 0 {
			break
		}
		idx += offset
		start := idx - 240
		if start < 0 {
			start = 0
		}
		end := idx + len(pattern) + 360
		if end > len(text) {
			end = len(text)
		}
		matches = append(matches, strings.TrimSpace(text[start:end]))
		offset = idx + len(needle)
		if offset >= len(lower) {
			break
		}
	}
	action.URL = page.Action.URL
	return Result{Action: action, Content: strings.Join(matches, "\n\n---\n\n"), Sources: page.Sources}, nil
}

func (r *Runtime) maxResultsForContext(size string) int {
	max := r.cfg.MaxResults
	switch strings.ToLower(strings.TrimSpace(size)) {
	case "low":
		if max > 4 {
			max = 4
		}
	case "high":
		if max < 10 {
			max = 10
		}
	default:
		if max > 8 {
			max = 8
		}
	}
	return max
}

var (
	resultLinkRE = regexp.MustCompile(`(?is)<a[^>]*class="[^"]*result__a[^"]*"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	snippetRE    = regexp.MustCompile(`(?is)<(?:a|div)[^>]*class="[^"]*result__snippet[^"]*"[^>]*>(.*?)</(?:a|div)>`)
	scriptRE     = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	styleRE      = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	tagRE        = regexp.MustCompile(`(?is)<[^>]+>`)
	whitespaceRE = regexp.MustCompile(`[\t\r\n ]+`)
)

func parseDuckDuckGoHTML(body string) []Source {
	links := resultLinkRE.FindAllStringSubmatch(body, -1)
	snippets := snippetRE.FindAllStringSubmatch(body, -1)
	out := make([]Source, 0, len(links))
	for i, match := range links {
		if len(match) < 3 {
			continue
		}
		href := decodeDuckDuckGoURL(html.UnescapeString(match[1]))
		if href == "" {
			continue
		}
		source := Source{Title: cleanText(match[2]), URL: href}
		if i < len(snippets) && len(snippets[i]) > 1 {
			source.Snippet = cleanText(snippets[i][1])
		}
		out = append(out, source)
	}
	return out
}

func decodeDuckDuckGoURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if strings.Contains(strings.ToLower(u.Hostname()), "duckduckgo.com") {
		if target := strings.TrimSpace(u.Query().Get("uddg")); target != "" {
			if decoded, err := url.QueryUnescape(target); err == nil {
				return decoded
			}
			return target
		}
	}
	return raw
}

func filterDomains(results []Source, allowed []string) []Source {
	domains := normalizeDomains(allowed)
	if len(domains) == 0 {
		return results
	}
	out := make([]Source, 0, len(results))
	for _, result := range results {
		u, err := url.Parse(result.URL)
		if err != nil {
			continue
		}
		host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
		for _, domain := range domains {
			if host == domain || strings.HasSuffix(host, "."+domain) {
				out = append(out, result)
				break
			}
		}
	}
	return out
}

func normalizeDomains(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		value = strings.TrimPrefix(value, "*.")
		value = strings.TrimPrefix(value, ".")
		value = strings.TrimSuffix(value, ".")
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func cleanPageText(raw string) string {
	raw = scriptRE.ReplaceAllString(raw, " ")
	raw = styleRE.ReplaceAllString(raw, " ")
	return cleanText(raw)
}

func cleanText(raw string) string {
	raw = tagRE.ReplaceAllString(raw, " ")
	raw = html.UnescapeString(raw)
	raw = strings.ReplaceAll(raw, "\u00a0", " ")
	raw = whitespaceRE.ReplaceAllString(raw, " ")
	return strings.TrimSpace(raw)
}

func validatePublicURL(u *url.URL) error {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || strings.TrimSpace(u.Hostname()) == "" {
		return errors.New("web tool URL must be an absolute http(s) URL")
	}
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(u.Hostname()), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return errors.New("web tool URL must target the public internet")
	}
	if ip := net.ParseIP(host); ip != nil && !isPublicIP(ip) {
		return errors.New("web tool URL must target the public internet")
	}
	return nil
}

func validatePublicURLContext(ctx context.Context, u *url.URL) error {
	if err := validatePublicURL(u); err != nil {
		return err
	}
	host := strings.TrimSpace(u.Hostname())
	if ip := net.ParseIP(host); ip != nil {
		if !isPublicIP(ip) {
			return errors.New("web tool URL must target the public internet")
		}
		return nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve web tool host: %w", err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("web tool host %s did not resolve", host)
	}
	for _, resolved := range ips {
		if !isPublicIP(resolved.IP) {
			return errors.New("web tool URL must target the public internet")
		}
	}
	return nil
}

func isPublicIP(ip net.IP) bool {
	return ip != nil && !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsUnspecified() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast()
}
