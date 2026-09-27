package admin

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aisense/internal/config"
	"aisense/internal/debuglog"
)

func authRequest(s *Server, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Aisense-Request", "1")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	s.Handle(w, r)
	return w
}

func loginCookie(t *testing.T, s *Server, code string) *http.Cookie {
	t.Helper()
	w := authRequest(s, "POST", "/admin/api/login", `{"username":"admin","password":"correct-horse","code":"`+code+`"}`, nil)
	if w.Code != 200 || len(w.Result().Cookies()) != 1 {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	return w.Result().Cookies()[0]
}

func TestSessionLoginCSRFExpiryAndRotation(t *testing.T) {
	s, cfg := testServer(t)
	c := loginCookie(t, s, "")
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Secure || c.MaxAge != 43200 {
		t.Fatalf("cookie: %+v", c)
	}
	if w := authRequest(s, "GET", "/", "", c); w.Body.String() != "<html>panel</html>" {
		t.Fatal(w.Body.String())
	}
	for _, tc := range []struct{ header, origin string }{{"", ""}, {"1", "https://evil.test"}, {"1", "null"}, {"1", "http://example.com.evil"}} {
		r := httptest.NewRequest("POST", "/admin/api/logout", nil)
		r.AddCookie(c)
		r.Header.Set("X-Aisense-Request", tc.header)
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		s.Handle(w, r)
		if w.Code != 403 {
			t.Fatalf("csrf %+v: %d", tc, w.Code)
		}
	}
	if w := authRequest(s, "POST", "/admin/api/logout", "", c); w.Code != 200 || w.Result().Cookies()[0].MaxAge != -1 {
		t.Fatal("logout failed")
	}
	if w := authRequest(s, "GET", "/admin/api/state", "", c); w.Code != 401 {
		t.Fatal("logged out cookie accepted")
	}
	c = loginCookie(t, s, "")
	s.sessions[c.Value].expires = time.Now().Add(-time.Second)
	if w := authRequest(s, "GET", "/admin/api/state", "", c); w.Code != 401 {
		t.Fatal("expired cookie accepted")
	}
	c = loginCookie(t, s, "")
	if err := cfg.Update(func(c *config.Config) error { c.Server.Admin.Password = "changed"; return nil }); err != nil {
		t.Fatal(err)
	}
	if w := authRequest(s, "GET", "/admin/api/state", "", c); w.Code != 401 {
		t.Fatal("old binding accepted")
	}
}

func TestLoginValidationAndTLS(t *testing.T) {
	s, _ := testServer(t)
	for _, tc := range []struct {
		content, origin, body string
		status                int
	}{
		{"text/plain", "", `{}`, 415},
		{"application/json", "http://evil.test", `{}`, 403},
		{"application/json", "", `{} {}`, 400},
		{"application/json", "", `{"username":"admin","password":"wrong"}`, 401},
	} {
		r := httptest.NewRequest("POST", "/admin/api/login", strings.NewReader(tc.body))
		r.Header.Set("Content-Type", tc.content)
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		s.Handle(w, r)
		if w.Code != tc.status || w.Header().Get("WWW-Authenticate") != "" {
			t.Fatalf("validation: %d", w.Code)
		}
	}
	r := httptest.NewRequest("POST", "https://example.com/admin/api/login", strings.NewReader(`{"username":"admin","password":"correct-horse"}`))
	r.TLS = &tls.ConnectionState{}
	r.Header.Set("Origin", "https://example.com")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handle(w, r)
	if w.Code != 200 || !w.Result().Cookies()[0].Secure {
		t.Fatal("TLS cookie not secure")
	}
}

func TestTOTPLifecycleAndReplay(t *testing.T) {
	s, cfg := testServer(t)
	c := loginCookie(t, s, "")
	if w := authRequest(s, "POST", "/admin/api/2fa/setup", `{"password":"wrong"}`, c); w.Code != 403 {
		t.Fatal("setup without password")
	}
	w := authRequest(s, "POST", "/admin/api/2fa/setup", `{"password":"correct-horse"}`, c)
	var setup struct{ Secret, URL string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &setup) != nil || setup.Secret == "" || !strings.HasPrefix(setup.URL, "otpauth://totp/") {
		t.Fatalf("setup: %s", w.Body.String())
	}
	other := loginCookie(t, s, "")
	if w := authRequest(s, "POST", "/admin/api/2fa/enable", `{"code":"123456"}`, other); w.Code != 400 {
		t.Fatal("pending setup shared")
	}
	now := time.Now()
	// Enable with the previous window, leaving the current code available for login.
	previous := TOTPCode(setup.Secret, now.Add(-30*time.Second))
	w = authRequest(s, "POST", "/admin/api/2fa/enable", `{"code":"`+previous+`"}`, c)
	if w.Code != 200 {
		t.Fatalf("enable: %s", w.Body.String())
	}
	if w := authRequest(s, "GET", "/admin/api/state", "", c); w.Code != 401 {
		t.Fatal("enable did not revoke session")
	}
	r := httptest.NewRequest("GET", "/admin/api/state", nil)
	r.SetBasicAuth("admin", "correct-horse")
	w = httptest.NewRecorder()
	s.Handle(w, r)
	if w.Code != 401 {
		t.Fatal("Basic bypassed 2FA")
	}
	if w := authRequest(s, "POST", "/admin/api/login", `{"username":"admin","password":"correct-horse","code":"`+previous+`"}`, nil); w.Code != 401 {
		t.Fatal("enable code replayed")
	}
	code := TOTPCode(setup.Secret, now)
	c = loginCookie(t, s, code)
	if w := authRequest(s, "POST", "/admin/api/login", `{"username":"admin","password":"correct-horse","code":"`+code+`"}`, nil); w.Code != 401 {
		t.Fatal("login code replayed")
	}
	if w := authRequest(s, "GET", "/admin/api/state", "", c); strings.Contains(w.Body.String(), setup.Secret) || strings.Contains(w.Body.String(), "totp_last_counter") {
		t.Fatal("state leaks TOTP")
	}
	if w := authRequest(s, "GET", "/admin/api/2fa", "", c); !strings.Contains(w.Body.String(), `"enabled":true`) {
		t.Fatal("status")
	}
	future := TOTPCode(setup.Secret, now.Add(30*time.Second))
	if w := authRequest(s, "POST", "/admin/api/2fa/disable", `{"password":"wrong","code":"`+future+`"}`, c); w.Code != 403 {
		t.Fatal("disable password not checked")
	}
	if w := authRequest(s, "POST", "/admin/api/2fa/disable", `{"password":"correct-horse","code":"`+code+`"}`, c); w.Code != 400 {
		t.Fatal("disable replay accepted")
	}
	w = authRequest(s, "POST", "/admin/api/2fa/disable", `{"password":"correct-horse","code":"`+future+`"}`, c)
	if w.Code != 200 || cfg.Get().Server.Admin.TOTPSecret != "" {
		t.Fatalf("disable: %d %s", w.Code, w.Body.String())
	}
	if w := authRequest(s, "GET", "/admin/api/state", "", c); w.Code != 401 {
		t.Fatal("disable did not revoke session")
	}
}

func TestTOTPRFCVectorsAndWindow(t *testing.T) {
	secret := totpBase32.EncodeToString([]byte("12345678901234567890"))
	for _, v := range []struct {
		unix int64
		code string
	}{{59, "287082"}, {1111111109, "081804"}, {1111111111, "050471"}, {1234567890, "005924"}, {2000000000, "279037"}, {20000000000, "353130"}} {
		if got := TOTPCode(secret, time.Unix(v.unix, 0)); got != v.code {
			t.Fatalf("RFC %d: %s != %s", v.unix, got, v.code)
		}
	}
	now := time.Unix(1234567890, 0)
	for step := -2; step <= 2; step++ {
		code := TOTPCode(secret, now.Add(time.Duration(step)*30*time.Second))
		counter, ok := verifyTOTPCounter(secret, code, now, -1)
		if ok != (step >= -1 && step <= 1) {
			t.Fatalf("window %d", step)
		}
		if ok {
			if _, replay := verifyTOTPCounter(secret, code, now, counter); replay {
				t.Fatal("replay")
			}
		}
	}
	for _, code := range []string{"", "12345", "abcdef", "1234567"} {
		if _, ok := verifyTOTPCounter(secret, code, now, -1); ok {
			t.Fatal("invalid format")
		}
	}
}

func TestPendingExpiryAndBoundedMaps(t *testing.T) {
	s, _ := testServer(t)
	c := loginCookie(t, s, "")
	authRequest(s, "POST", "/admin/api/2fa/setup", `{"password":"correct-horse"}`, c)
	v := s.sessions[c.Value]
	code := TOTPCode(v.pending, time.Now())
	v.pendingExpires = time.Now().Add(-time.Second)
	if w := authRequest(s, "POST", "/admin/api/2fa/enable", `{"code":"`+code+`"}`, c); w.Code != 400 {
		t.Fatal("expired setup accepted")
	}
	for i := 0; i < authMapLimit; i++ {
		s.sessions[string(rune(i))] = &adminSession{binding: v.binding, expires: time.Now().Add(time.Hour)}
	}
	if w := authRequest(s, "POST", "/admin/api/login", `{"username":"admin","password":"correct-horse"}`, nil); w.Code != 503 {
		t.Fatal("session limit")
	}
}

func TestTOTPReplayAtomicAndPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.Server.Admin.Username, c.Server.Admin.Password = "admin", "correct-horse"
		c.Server.Admin.TOTPSecret = secret
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s := &Server{Cfg: cfg}
	body := `{"username":"admin","password":"correct-horse","code":"` + TOTPCode(secret, time.Now()) + `"}`
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- authRequest(s, "POST", "/admin/api/login", body, nil).Code }()
	}
	wg.Wait()
	close(results)
	successes := 0
	for code := range results {
		if code == 200 {
			successes++
		} else if code != 401 {
			t.Fatalf("status %d", code)
		}
	}
	if successes != 1 {
		t.Fatalf("accepted the same code %d times", successes)
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if w := authRequest(&Server{Cfg: reloaded}, "POST", "/admin/api/login", body, nil); w.Code != 401 {
		t.Fatal("replay accepted after restart")
	}
}

func TestAuthDataExcludedFromRawDebug(t *testing.T) {
	s, _ := testServer(t)
	path := filepath.Join(t.TempDir(), "debug.log")
	trace, err := debuglog.NewRaw(path, 1<<20, 2)
	if err != nil {
		t.Fatal(err)
	}
	s.Debug = trace
	t.Cleanup(func() { trace.Close() })
	c := loginCookie(t, s, "")
	w := authRequest(s, "POST", "/admin/api/2fa/setup", `{"password":"correct-horse"}`, c)
	var setup struct{ Secret string }
	if err := json.Unmarshal(w.Body.Bytes(), &setup); err != nil || setup.Secret == "" {
		t.Fatal("setup failed")
	}
	authRequest(s, "POST", "/admin/api/debug/event", `{"type":"ui.input","password":"debug-secret-marker"}`, c)
	authRequest(s, "POST", "/admin/api/settings", `{"password":"settings-secret-marker"}`, c)
	authRequest(s, "GET", "/admin/api/state", "", c)
	authRequest(s, "GET", "/?password=query-secret-marker", "", c)
	if err := trace.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"correct-horse", c.Value, setup.Secret, "debug-secret-marker", "settings-secret-marker", "query-secret-marker"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("debug leaked %q", secret)
		}
	}
}
