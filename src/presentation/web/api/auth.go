package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/time/rate"
)

const (
	sessionCookieName        = "__Host-cinemator_session"
	sessionTTL               = 7 * 24 * time.Hour
	maxLoginBodyBytes        = 4 << 10
	loginAttemptInterval     = 10 * time.Second
	loginAttemptBurst        = 5
	maxConcurrentLoginChecks = 2
	minSessionSecret         = 32
	sessionNonceBytes        = 32
	sessionVersion           = "v1"
)

type authenticator struct {
	passwordHash  []byte
	sessionKey    []byte
	loginAttempts *rate.Limiter
	loginChecks   chan struct{}
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
	}, nil
}

func (s *HttpServer) requireAuthentication(next http.Handler) http.Handler {
	if s.auth == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if s.auth.authenticated(r) {
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
	http.SetCookie(w, sessionCookie("", time.Unix(1, 0), -1))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (a *authenticator) createSession() (string, time.Time, error) {
	expiresAt := time.Now().Add(sessionTTL)
	token, err := a.newSessionToken(expiresAt)
	if err != nil {
		return "", time.Time{}, err
	}
	return token, expiresAt, nil
}

func (a *authenticator) authenticated(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 4 || parts[0] != sessionVersion {
		return false
	}
	nonce, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(nonce) != sessionNonceBytes {
		return false
	}
	providedSignature, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	payload := strings.Join(parts[:3], ".")
	if !hmac.Equal(providedSignature, a.sign(payload)) {
		return false
	}
	expiresAt, err := strconv.ParseInt(parts[1], 10, 64)
	return err == nil && time.Unix(expiresAt, 0).After(time.Now())
}

func (a *authenticator) newSessionToken(expiresAt time.Time) (string, error) {
	nonce := make([]byte, sessionNonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	payload := strings.Join([]string{
		sessionVersion,
		strconv.FormatInt(expiresAt.Unix(), 10),
		base64.RawURLEncoding.EncodeToString(nonce),
	}, ".")
	signature := base64.RawURLEncoding.EncodeToString(a.sign(payload))
	return payload + "." + signature, nil
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
