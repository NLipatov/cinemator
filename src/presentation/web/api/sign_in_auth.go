package api

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	qrcode "github.com/yeqown/go-qrcode/v2"
)

const (
	signInRequestTTL      = 5 * time.Minute
	signInRequestInterval = time.Second
	signInRequestBurst    = 10
	signInTokenBytes      = 32
	signInCodeAlphabet    = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	qrQuietZoneModules    = 4
	qrModulePixels        = 8
)

type signInRequest struct {
	approvalToken string
	approvalURL   string
	code          string
	expiresAt     time.Time
	approved      chan struct{}
	sessionWaiter chan struct{}
}

func (s *HttpServer) handleStartSignInRequest(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	origin, ok := requestOrigin(r)
	if !ok {
		http.Error(w, "valid origin required", http.StatusBadRequest)
		return
	}
	if !s.auth.signInAttempts.Allow() {
		w.Header().Set("Retry-After", strconv.Itoa(int(signInRequestInterval/time.Second)))
		http.Error(w, "too many sign-in requests", http.StatusTooManyRequests)
		return
	}

	deviceToken, request, err := s.auth.startSignInRequest(origin, time.Now())
	if err != nil {
		http.Error(w, "failed to create sign-in request", http.StatusInternalServerError)
		return
	}
	writeJSON(w, struct {
		DeviceToken string    `json:"deviceToken"`
		Code        string    `json:"code"`
		QRCode      string    `json:"qrCode"`
		ExpiresAt   time.Time `json:"expiresAt"`
	}{
		DeviceToken: deviceToken,
		Code:        request.code,
		QRCode:      "/api/auth/sign-in-requests/" + url.PathEscape(deviceToken) + "/qr",
		ExpiresAt:   request.expiresAt,
	})
}

func (s *HttpServer) handleSignInRequestQRCode(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	request, ok := s.auth.signInRequestForDevice(r.PathValue("deviceToken"), time.Now())
	if !ok {
		http.Error(w, "sign-in request expired", http.StatusGone)
		return
	}

	pngData, err := renderQRCode(request.approvalURL)
	if err != nil {
		http.Error(w, "failed to render QR code", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(pngData)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(pngData)
}

func (s *HttpServer) handleSignInRequestSession(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	request, ok := s.auth.signInRequestForDevice(r.PathValue("deviceToken"), time.Now())
	if !ok {
		http.Error(w, "sign-in request expired", http.StatusGone)
		return
	}
	select {
	case request.sessionWaiter <- struct{}{}:
		defer func() { <-request.sessionWaiter }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "sign-in session already waiting", http.StatusTooManyRequests)
		return
	}

	timer := time.NewTimer(time.Until(request.expiresAt))
	defer timer.Stop()
	select {
	case <-request.approved:
		if !request.expiresAt.After(time.Now()) {
			http.Error(w, "sign-in request expired", http.StatusGone)
			return
		}
	case <-timer.C:
		http.Error(w, "sign-in request expired", http.StatusGone)
		return
	case <-r.Context().Done():
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

func (s *HttpServer) handleSignInApproval(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !s.auth.authenticated(r) {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	request, ok := s.auth.signInRequestForApproval(r.PathValue("approvalToken"), time.Now())
	if !ok {
		http.Error(w, "sign-in request expired", http.StatusGone)
		return
	}
	writeJSON(w, struct {
		Code      string    `json:"code"`
		ExpiresAt time.Time `json:"expiresAt"`
	}{Code: request.code, ExpiresAt: request.expiresAt})
}

func (s *HttpServer) handleApproveSignInRequest(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !s.auth.authenticated(r) {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if !s.auth.approveSignInRequest(r.PathValue("approvalToken"), time.Now()) {
		http.Error(w, "sign-in request expired", http.StatusGone)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *HttpServer) handleSignInApprovalPage(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !s.auth.authenticated(r) {
		next := url.QueryEscape(r.URL.RequestURI())
		http.Redirect(w, r, "/login?next="+next, http.StatusSeeOther)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.ServeFile(w, r, "presentation/web/client/index/sign-in-approval.html")
}

func (a *authenticator) startSignInRequest(origin string, now time.Time) (string, *signInRequest, error) {
	deviceToken, err := newSignInToken()
	if err != nil {
		return "", nil, err
	}
	approvalToken, err := newSignInToken()
	if err != nil {
		return "", nil, err
	}
	code, err := newSignInCode()
	if err != nil {
		return "", nil, err
	}
	request := &signInRequest{
		approvalToken: approvalToken,
		approvalURL:   origin + "/sign-in-approvals/" + url.PathEscape(approvalToken),
		code:          code,
		expiresAt:     now.Add(signInRequestTTL),
		approved:      make(chan struct{}),
		sessionWaiter: make(chan struct{}, 1),
	}

	a.signInMu.Lock()
	defer a.signInMu.Unlock()
	a.removeExpiredSignInRequestsLocked(now)
	a.signInRequests[deviceToken] = request
	return deviceToken, request, nil
}

func (a *authenticator) signInRequestForDevice(deviceToken string, now time.Time) (*signInRequest, bool) {
	a.signInMu.Lock()
	defer a.signInMu.Unlock()
	a.removeExpiredSignInRequestsLocked(now)
	request, ok := a.signInRequests[deviceToken]
	return request, ok
}

func (a *authenticator) signInRequestForApproval(approvalToken string, now time.Time) (*signInRequest, bool) {
	a.signInMu.Lock()
	defer a.signInMu.Unlock()
	a.removeExpiredSignInRequestsLocked(now)
	for _, request := range a.signInRequests {
		if subtle.ConstantTimeCompare([]byte(request.approvalToken), []byte(approvalToken)) == 1 {
			return request, true
		}
	}
	return nil, false
}

func (a *authenticator) approveSignInRequest(approvalToken string, now time.Time) bool {
	a.signInMu.Lock()
	defer a.signInMu.Unlock()
	a.removeExpiredSignInRequestsLocked(now)
	for _, request := range a.signInRequests {
		if subtle.ConstantTimeCompare([]byte(request.approvalToken), []byte(approvalToken)) != 1 {
			continue
		}
		select {
		case <-request.approved:
		default:
			close(request.approved)
		}
		return true
	}
	return false
}

func (a *authenticator) removeExpiredSignInRequestsLocked(now time.Time) {
	for deviceToken, request := range a.signInRequests {
		if !request.expiresAt.After(now) {
			delete(a.signInRequests, deviceToken)
		}
	}
}

func newSignInToken() (string, error) {
	data := make([]byte, signInTokenBytes)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func newSignInCode() (string, error) {
	data := make([]byte, 5)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	code := base32.NewEncoding(signInCodeAlphabet).WithPadding(base32.NoPadding).EncodeToString(data)
	return code[:4] + "-" + code[4:], nil
}

func requestOrigin(r *http.Request) (string, bool) {
	if parsed, err := url.Parse(r.Header.Get("Origin")); err == nil &&
		(parsed.Scheme == "http" || parsed.Scheme == "https") &&
		parsed.Host != "" && strings.EqualFold(parsed.Host, r.Host) {
		return parsed.Scheme + "://" + parsed.Host, true
	}
	return "", false
}

type qrPNGWriter struct {
	buffer *bytes.Buffer
}

func (w qrPNGWriter) Write(matrix qrcode.Matrix) error {
	bitmap := matrix.Bitmap()
	size := (len(bitmap) + 2*qrQuietZoneModules) * qrModulePixels
	qrImage := image.NewGray(image.Rect(0, 0, size, size))
	for index := range qrImage.Pix {
		qrImage.Pix[index] = 0xff
	}
	for y, row := range bitmap {
		for x, dark := range row {
			if !dark {
				continue
			}
			left := (x + qrQuietZoneModules) * qrModulePixels
			top := (y + qrQuietZoneModules) * qrModulePixels
			for pixelY := top; pixelY < top+qrModulePixels; pixelY++ {
				for pixelX := left; pixelX < left+qrModulePixels; pixelX++ {
					qrImage.SetGray(pixelX, pixelY, color.Gray{})
				}
			}
		}
	}
	return png.Encode(w.buffer, qrImage)
}

func (w qrPNGWriter) Close() error {
	return nil
}

func renderQRCode(value string) ([]byte, error) {
	code, err := qrcode.New(value)
	if err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	if err := code.Save(qrPNGWriter{buffer: &buffer}); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
