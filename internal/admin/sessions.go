package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"aisense/internal/config"
)

const sessionCookie = "aisense_session"
const sessionLifetime = 12 * time.Hour
const authMapLimit = 4096

type adminSession struct {
	binding        [32]byte
	expires        time.Time
	pending        string
	pendingExpires time.Time
}

func authBinding(c config.AdminCfg) [32]byte {
	b, _ := json.Marshal([]string{c.Username, c.Password, c.TOTPSecret})
	return sha256.Sum256(b)
}

func credentialsMatch(c config.AdminCfg, username, password string) bool {
	u, p := sha256.Sum256([]byte(username)), sha256.Sum256([]byte(password))
	cu, cp := sha256.Sum256([]byte(c.Username)), sha256.Sum256([]byte(c.Password))
	return c.Username != "" && c.Password != "" && subtle.ConstantTimeCompare(u[:], cu[:])&subtle.ConstantTimeCompare(p[:], cp[:]) == 1
}

func (s *Server) session(r *http.Request) (string, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", false
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()
	v := s.sessions[c.Value]
	if v == nil {
		return "", false
	}
	if !time.Now().Before(v.expires) || v.binding != authBinding(s.Cfg.Get().Server.Admin) {
		delete(s.sessions, c.Value)
		return "", false
	}
	return c.Value, true
}

func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return err == nil && u.Scheme == scheme && strings.EqualFold(u.Host, r.Host) && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == ""
}

func (s *Server) authJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		s.writeJSON(w, 415, map[string]string{"error": "application/json required"})
		return false
	}
	if !sameOrigin(r) {
		s.writeJSON(w, 403, map[string]string{"error": "same-origin request required"})
		return false
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	if d.Decode(v) != nil || d.Decode(new(any)) != io.EOF {
		s.writeJSON(w, 400, map[string]string{"error": "invalid JSON"})
		return false
	}
	return true
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in struct{ Username, Password, Code string }
	if !s.authJSON(w, r, &in) {
		return
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()
	c := s.Cfg.Get().Server.Admin
	if !credentialsMatch(c, in.Username, in.Password) {
		s.unauthorized(w, true)
		return
	}
	now := time.Now()
	if s.sessions == nil {
		s.sessions = make(map[string]*adminSession)
	}
	for k, v := range s.sessions {
		if !now.Before(v.expires) || v.binding != authBinding(c) {
			delete(s.sessions, k)
		}
	}
	if len(s.sessions) >= authMapLimit {
		s.writeJSON(w, 503, map[string]string{"error": "session capacity reached"})
		return
	}
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		s.writeJSON(w, 500, map[string]string{"error": "session creation failed"})
		return
	}
	if c.TOTPSecret != "" {
		err := s.Cfg.Update(func(next *config.Config) error {
			a := &next.Server.Admin
			if authBinding(*a) != authBinding(c) {
				return errors.New("credentials changed")
			}
			counter, ok := verifyTOTPCounter(a.TOTPSecret, in.Code, now, a.TOTPLastCounter)
			if !ok {
				return errors.New("invalid code")
			}
			a.TOTPLastCounter = counter
			return nil
		})
		if err != nil {
			s.unauthorized(w, true)
			return
		}
	}
	key := hex.EncodeToString(token[:])
	if old, err := r.Cookie(sessionCookie); err == nil {
		delete(s.sessions, old.Value)
	}
	s.sessions[key] = &adminSession{binding: authBinding(c), expires: now.Add(sessionLifetime)}
	cookiePath := s.basePath() + "/"
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: key, Path: cookiePath, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, Expires: now.Add(sessionLifetime), MaxAge: int(sessionLifetime.Seconds())})
	s.writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.authMu.Lock()
	if c, err := r.Cookie(sessionCookie); err == nil {
		delete(s.sessions, c.Value)
	}
	s.authMu.Unlock()
	cookiePath := s.basePath() + "/"
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: cookiePath, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: -1, Expires: time.Unix(1, 0)})
	s.writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) twoFactor(w http.ResponseWriter, r *http.Request, path string) {
	key, ok := s.session(r)
	if !ok {
		s.unauthorized(w, true)
		return
	}
	if path == "2fa" && r.Method == http.MethodGet {
		s.writeJSON(w, 200, map[string]bool{"enabled": s.Cfg.Get().Server.Admin.TOTPSecret != ""})
		return
	}
	if r.Method != http.MethodPost || (path != "2fa/setup" && path != "2fa/enable" && path != "2fa/disable") {
		http.NotFound(w, r)
		return
	}
	var in struct{ Password, Code string }
	if !s.authJSON(w, r, &in) {
		return
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()
	c := s.Cfg.Get().Server.Admin
	v := s.sessions[key]
	now := time.Now()
	if v == nil || v.binding != authBinding(c) || !now.Before(v.expires) {
		s.unauthorized(w, true)
		return
	}
	if path != "2fa/enable" && !credentialsMatch(c, c.Username, in.Password) {
		// The session is valid; reserve 401 for expired/missing sessions so the
		// UI can display a password error without redirecting away from the form.
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "incorrect current password"})
		return
	}
	if path == "2fa/setup" {
		if c.TOTPSecret != "" {
			s.writeJSON(w, 409, map[string]string{"error": "2FA already enabled"})
			return
		}
		secret, err := GenerateTOTPSecret()
		if err != nil {
			s.writeJSON(w, 500, map[string]string{"error": "secret generation failed"})
			return
		}
		v.pending, v.pendingExpires = secret, now.Add(10*time.Minute)
		s.writeJSON(w, 200, map[string]string{"secret": secret, "url": TOTPAuthURL(secret, c.Username)})
		return
	}
	err := s.Cfg.Update(func(next *config.Config) error {
		a := &next.Server.Admin
		if authBinding(*a) != v.binding {
			return errors.New("credentials changed")
		}
		if path == "2fa/enable" {
			if a.TOTPSecret != "" || v.pending == "" || !now.Before(v.pendingExpires) {
				return errors.New("setup expired or unavailable")
			}
			counter, valid := verifyTOTPCounter(v.pending, in.Code, now, -1)
			if !valid {
				return errors.New("invalid code")
			}
			a.TOTPSecret, a.TOTPLastCounter = v.pending, counter
		} else {
			if _, valid := verifyTOTPCounter(a.TOTPSecret, in.Code, now, a.TOTPLastCounter); !valid {
				return errors.New("invalid code")
			}
			a.TOTPSecret, a.TOTPLastCounter = "", 0
		}
		return nil
	})
	if err != nil {
		s.writeJSON(w, 400, map[string]string{"error": "invalid code or setup expired"})
		return
	}
	// Enabling/disabling changes the binding and revokes every session.
	clear(s.sessions)
	s.writeJSON(w, 200, map[string]bool{"ok": true})
}
