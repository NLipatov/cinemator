package api

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookieName = "__Host-cinemator_session"
	sessionTTL        = 30 * 24 * time.Hour
	maxLoginBodyBytes = 4 << 10
)

type authenticator struct {
	passwordHash []byte
	mu           sync.Mutex
	sessions     map[string]time.Time
}

func newAuthenticator(passwordHash string) (*authenticator, error) {
	if passwordHash == "" {
		return nil, nil
	}
	if _, err := bcrypt.Cost([]byte(passwordHash)); err != nil {
		return nil, errors.New("CINEMATOR_PASSWORD_HASH must contain a valid bcrypt hash")
	}
	return &authenticator{
		passwordHash: []byte(passwordHash),
		sessions:     make(map[string]time.Time),
	}, nil
}

func (s *HttpServer) requireAuthentication(next http.Handler) http.Handler {
	if s.auth == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.auth.authenticated(r) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

func (s *HttpServer) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodHead)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.auth == nil || s.auth.authenticated(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, "presentation/web/client/index/login.html")
}

func (s *HttpServer) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, struct {
		Enabled       bool `json:"enabled"`
		Authenticated bool `json:"authenticated"`
	}{
		Enabled:       s.auth != nil,
		Authenticated: s.auth != nil && s.auth.authenticated(r),
	})
}

func (s *HttpServer) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.auth == nil {
		http.NotFound(w, r)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxLoginBodyBytes)
	var request struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "bad login request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if bcrypt.CompareHashAndPassword(s.auth.passwordHash, []byte(request.Password)) != nil {
		http.Error(w, "invalid password", http.StatusUnauthorized)
		return
	}

	token, expiresAt, err := s.auth.createSession()
	if err != nil {
		http.Error(w, "failed to create session", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, sessionCookie(token, expiresAt, int(sessionTTL/time.Second)))
	w.WriteHeader(http.StatusNoContent)
}

func (s *HttpServer) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.auth == nil {
		http.NotFound(w, r)
		return
	}
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		s.auth.deleteSession(cookie.Value)
	}
	http.SetCookie(w, sessionCookie("", time.Unix(1, 0), -1))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (a *authenticator) createSession() (string, time.Time, error) {
	var randomBytes [32]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return "", time.Time{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(randomBytes[:])
	expiresAt := time.Now().Add(sessionTTL)

	a.mu.Lock()
	a.deleteExpiredSessionsLocked(time.Now())
	a.sessions[token] = expiresAt
	a.mu.Unlock()
	return token, expiresAt, nil
}

func (a *authenticator) authenticated(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}

	now := time.Now()
	a.mu.Lock()
	expiresAt, exists := a.sessions[cookie.Value]
	if exists && !expiresAt.After(now) {
		delete(a.sessions, cookie.Value)
		exists = false
	}
	a.mu.Unlock()
	return exists
}

func (a *authenticator) deleteSession(token string) {
	a.mu.Lock()
	delete(a.sessions, token)
	a.mu.Unlock()
}

func (a *authenticator) deleteExpiredSessionsLocked(now time.Time) {
	for token, expiresAt := range a.sessions {
		if !expiresAt.After(now) {
			delete(a.sessions, token)
		}
	}
}

func sessionCookie(value string, expiresAt time.Time, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		Expires:  expiresAt,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	}
}
