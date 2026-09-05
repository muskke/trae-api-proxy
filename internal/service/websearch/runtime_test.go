package websearch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestParseDuckDuckGoHTML(t *testing.T) {
	raw := `
<html><body>
<a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fdocs">Example &amp; Docs</a>
<a class="result__snippet">Useful <b>documentation</b> result.</a>
<a rel="nofollow" class="result__a" href="https://second.example/path">Second</a>
<div class="result__snippet">Second snippet</div>
</body></html>`
	got := parseDuckDuckGoHTML(raw)
	if len(got) != 2 {
		t.Fatalf("results=%d %#v", len(got), got)
	}
	if got[0].URL != "https://example.com/docs" || got[0].Title != "Example & Docs" || got[0].Snippet != "Useful documentation result." {
		t.Fatalf("first result=%+v", got[0])
	}
}

func TestSearchDuckDuckGoHonorsAllowedDomains(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "codex" {
			t.Fatalf("q=%q", r.URL.Query().Get("q"))
		}
		_, _ = w.Write([]byte(`
<a class="result__a" href="https://openai.com/codex">Codex</a>
<a class="result__snippet">OpenAI Codex</a>
<a class="result__a" href="https://example.com/nope">Other</a>
<a class="result__snippet">Other result</a>`))
	}))
	defer srv.Close()

	runtime := New(Config{Backend: "duckduckgo", Endpoint: srv.URL, Timeout: time.Second, MaxResults: 8})
	result, err := runtime.Execute(context.Background(), Action{Type: "search", Query: "codex"}, ToolOptions{AllowedDomains: []string{"openai.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sources) != 1 || result.Sources[0].URL != "https://openai.com/codex" {
		t.Fatalf("sources=%+v", result.Sources)
	}
}

func TestSearXNGBackend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" || r.URL.Query().Get("format") != "json" {
			t.Fatalf("request=%s", r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"url":"https://openai.com","title":"OpenAI","content":"AI research"}]}`))
	}))
	defer srv.Close()

	runtime := New(Config{Backend: "searxng", Endpoint: srv.URL, Timeout: time.Second})
	result, err := runtime.Execute(context.Background(), Action{Type: "search", Query: "openai"}, ToolOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sources) != 1 || result.Sources[0].Title != "OpenAI" {
		t.Fatalf("result=%+v", result)
	}
}

func TestRejectsPrivateWebTargets(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1:8000/private",
		"http://10.1.2.3/",
		"http://localhost/admin",
		"http://169.254.169.254/latest/meta-data/",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := validatePublicURL(u); err == nil {
			t.Fatalf("expected private target %q to be rejected", raw)
		}
	}
}

func TestCleanPageText(t *testing.T) {
	got := cleanPageText(`<html><style>.x{}</style><script>alert(1)</script><body>Hello&nbsp;<b>world</b>\n next</body></html>`)
	if !strings.Contains(got, "Hello world") || strings.Contains(got, "alert") {
		t.Fatalf("got %q", got)
	}
}
