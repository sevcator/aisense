package autodiscovery

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/builtinCatalog.ts")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParse(t *testing.T) {
	source := fixture(t)
	names, err := Parse([]byte(source))
	if err != nil || len(names) != 8 {
		t.Fatalf("%v %v", names, err)
	}
	for _, name := range []string{"auto/best-coding", "best-reasoning", "auto/coding:fast"} {
		if !Matches(names, name) {
			t.Fatal(name)
		}
	}
	for _, name := range []string{"other/best-coding", "auto", "auto/invented"} {
		if Matches(names, name) {
			t.Fatal(name)
		}
	}
	for _, bad := range []string{
		strings.Replace(source, `"coding",`, `executeRemote(),`, 1),
		strings.Replace(source, `"coding",`, `"unknown",`, 1),
		strings.Replace(source, "AUTO_SUFFIX_VARIANTS", "RENAMED", 1),
		strings.Replace(source, `"auto/best-coding"`, `"auto/new"`, 1),
		strings.Repeat("x", MaxSourceBytes+1),
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Fatal("accepted drift or expression")
		}
	}
}

func TestCacheBoundedFailureAndLastSuccess(t *testing.T) {
	body := fixture(t)
	calls := 0
	c := Cache{client: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != SourceURL {
			t.Fatal(r.URL)
		}
		if _, ok := r.Context().Deadline(); !ok {
			t.Fatal("no timeout")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}}
	names, err := c.Refresh(context.Background())
	if err != nil || len(names) == 0 {
		t.Fatal(err)
	}
	names[0] = "mutated"
	if _, err := c.Refresh(context.Background()); err != nil || calls != 1 {
		t.Fatal("cache missed")
	}
	for _, bad := range []string{"format drift", strings.Repeat("x", MaxSourceBytes+1)} {
		body = bad
		c.attempt = time.Now().Add(-2 * time.Hour)
		stale, err := c.Refresh(context.Background())
		if err == nil || len(stale) != 8 || stale[0] == "mutated" {
			t.Fatalf("%v %v", stale, err)
		}
	}
}

func TestCacheInitialHTTPFailureThrottled(t *testing.T) {
	calls := 0
	c := Cache{client: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("unavailable")), Header: http.Header{}}, nil
	})}}
	for i := 0; i < 2; i++ {
		names, err := c.Refresh(context.Background())
		if err == nil || len(names) != 0 {
			t.Fatalf("%v %v", names, err)
		}
	}
	if calls != 1 {
		t.Fatal("failure was not throttled")
	}
}
