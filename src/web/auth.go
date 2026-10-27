package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/time/rate"
)

const (
	sessionCookieName        = "__Host-cinemator_session"
	sessionTTL               = 7 * 24 * time.Hour
	maxSessionRenewals       = 3
	maxLoginBodyBytes        = 4 << 10
	loginAttemptInterval     = 10 * time.Second
	loginAttemptBurst        = 5
	maxConcurrentLoginChecks = 2
	minSessionSecret         = 32
	sessionNonceBytes        = 32
	sessionVersion           = "v2"
)

type session struct {
	expiresAt time.Time
	renewals  int
}

type authenticator struct {
	passwordHash   []byte
	sessionKey     []byte
	loginAttempts  *rate.Limiter
	loginChecks    chan struct{}
	signInAttempts *rate.Limiter
	signInMu       sync.Mutex
	signInRequests map[string]*signInRequest
}

func newAuthenticator(passwordHash, sessionSecret string) (*authenticator, error) {
	if passwordHash == "" {
		return nil, nil
	}
	if _, err := bcrypt.Cost([]byte(passwordHash)); err != nil {
		return nil, errors.New("CINEMATOR_PASSWORD_HASH must contain a valid bcrypt hash")
	}
	if len(sessionSecret) < minSessionSecret {
		return nil, errors.New("CINEMATOR_SESSION_SECRET must contain at least 32 bytes when authentication is enabled")
	}
	return &authenticator{
		passwordHash:  []byte(passwordHash),
		sessionKey:    []byte(sessionSecret),
		loginAttempts: rate.NewLimiter(rate.Every(loginAttemptInterval), loginAttemptBurst),
		loginChecks:   make(chan struct{}, maxConcurrentLoginChecks),
		signInAttempts: rate.NewLimiter(
			rate.Every(signInRequestInterval),
			signInRequestBurst,
		),
		signInRequests: make(map[string]*signInRequest),
	}, nil
}

func (s *Server) requireAuthentication(next http.Handler) http.Handler {
	if s.auth == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if current := s.auth.sessionFromRequest(r); current != nil {
			if r.URL.Path != "/api/auth/logout" && current.renewals < maxSessionRenewals && time.Until(current.expiresAt) <= sessionTTL/3 {
				cookie, err := s.auth.createSession(session{
					expiresAt: time.Now().Add(sessionTTL),
					renewals:  current.renewals + 1,
				})
				if err != nil {
					http.Error(w, "failed to renew session", http.StatusInternalServerError)
					return
				}
				http.SetCookie(w, cookie)
			}
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodHead)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.auth == nil || s.auth.sessionFromRequest(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, "web/client/index/login.html")
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
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
		Authenticated: s.auth != nil && s.auth.sessionFromRequest(r) != nil,
	})
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.auth == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")

	r.Body = http.MaxBytesReader(w, r.Body, maxLoginBodyBytes)
	var request struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "bad login request", http.StatusBadRequest)
		return
	}
	if !s.auth.loginAttempts.Allow() {
		w.Header().Set("Retry-After", strconv.Itoa(int(loginAttemptInterval/time.Second)))
		http.Error(w, "too many login attempts", http.StatusTooManyRequests)
		return
	}
	select {
	case s.auth.loginChecks <- struct{}{}:
		defer func() { <-s.auth.loginChecks }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "too many login attempts", http.StatusTooManyRequests)
		return
	}
	if bcrypt.CompareHashAndPassword(s.auth.passwordHash, []byte(request.Password)) != nil {
		http.Error(w, "invalid password", http.StatusUnauthorized)
		return
	}

	cookie, err := s.auth.createSession(session{expiresAt: time.Now().Add(sessionTTL)})
	if err != nil {
		http.Error(w, "failed to create session", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, cookie)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.auth == nil {
		http.NotFound(w, r)
		return
	}
	http.SetCookie(w, sessionCookie("", time.Unix(1, 0), -1))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (a *authenticator) createSession(s session) (*http.Cookie, error) {
	expiresAt := s.expiresAt.Truncate(time.Second)
	maxAge := int(math.Ceil(time.Until(expiresAt).Seconds()))
	if maxAge <= 0 {
		return nil, errors.New("session expired")
	}
	nonce := make([]byte, sessionNonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	payload := strings.Join([]string{
		sessionVersion,
		strconv.FormatInt(expiresAt.Unix(), 10),
		base64.RawURLEncoding.EncodeToString(nonce),
		strconv.Itoa(s.renewals),
	}, ".")
	signature := base64.RawURLEncoding.EncodeToString(a.sign(payload))
	return sessionCookie(payload+"."+signature, expiresAt, maxAge), nil
}

func (a *authenticator) sessionFromRequest(r *http.Request) *session {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return nil
	}
	parts := strings.Split(cookie.Value, ".")
	renewals := maxSessionRenewals
	switch {
	case len(parts) == 4 && parts[0] == "v1":
		// Legacy sessions remain usable, but cannot acquire a new renewal budget.
	case len(parts) == 5 && parts[0] == sessionVersion:
		renewals, err = strconv.Atoi(parts[3])
		if err != nil || renewals < 0 || renewals > maxSessionRenewals {
			return nil
		}
	default:
		return nil
	}
	nonce, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(nonce) != sessionNonceBytes {
		return nil
	}
	signatureIndex := len(parts) - 1
	providedSignature, err := base64.RawURLEncoding.DecodeString(parts[signatureIndex])
	if err != nil {
		return nil
	}
	payload := strings.Join(parts[:signatureIndex], ".")
	if !hmac.Equal(providedSignature, a.sign(payload)) {
		return nil
	}
	expires, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return nil
	}
	expiresAt := time.Unix(expires, 0)
	if !expiresAt.After(time.Now()) {
		return nil
	}
	return &session{expiresAt: expiresAt, renewals: renewals}
}

func (a *authenticator) sign(payload string) []byte {
	mac := hmac.New(sha256.New, a.sessionKey)
	_, _ = mac.Write([]byte(payload))
	return mac.Sum(nil)
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
