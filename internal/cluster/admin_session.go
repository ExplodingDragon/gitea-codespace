// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"gitea.dev/codespace/web"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const adminSessionLabel = "codespace.gitea.dev/admin-session"

type AdminServer struct {
	Client     client.Client
	Namespace  string
	PublicURL  *url.URL
	TokenFile  string
	loginLimit *rate.Limiter
}

func NewAdminServer(reader client.Client, namespace, publicURL, tokenFile string) (*AdminServer, error) {
	u, err := url.Parse(publicURL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("admin public URL must be an absolute origin")
	}
	if u.Scheme != "https" {
		address, err := netip.ParseAddr(u.Hostname())
		if u.Scheme != "http" || err != nil || !address.IsLoopback() {
			return nil, fmt.Errorf("admin public URL requires HTTPS except on a loopback address")
		}
	}
	u.Host = strings.ToLower(u.Host)
	if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
		u.Host = u.Hostname()
		if strings.Contains(u.Host, ":") {
			u.Host = "[" + u.Host + "]"
		}
	}
	u.Path = ""
	s := &AdminServer{Client: reader, Namespace: namespace, PublicURL: u, TokenFile: tokenFile, loginLimit: rate.NewLimiter(1, 5)}
	if _, err := s.token(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *AdminServer) token() ([]byte, error) {
	f, err := os.Open(s.TokenFile)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(string(data))
	if len(token) < 32 || len(data) > 4096 {
		return nil, fmt.Errorf("admin token file must contain between 32 and 4096 bytes")
	}
	return []byte(token), nil
}

func (s *AdminServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/admin/login", s.login)
	mux.Handle("/api/admin/", s.authenticate(http.HandlerFunc(s.resources)))
	mux.Handle("/", web.Handler())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("Origin") != s.PublicURL.String() {
			adminJSON(w, http.StatusForbidden, map[string]string{"error": "request origin does not match the administration URL"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *AdminServer) login(w http.ResponseWriter, r *http.Request) {
	if !s.loginLimit.Allow() {
		w.Header().Set("Retry-After", "1")
		adminJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many sign-in attempts"})
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if !adminDecode(w, r, &body) {
		return
	}
	key, err := s.token()
	if err != nil {
		adminError(w, err)
		return
	}
	provided, expected := sha256.Sum256([]byte(body.Token)), sha256.Sum256(key)
	if subtle.ConstantTimeCompare(provided[:], expected[:]) != 1 {
		adminJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid administration token"})
		return
	}
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		adminError(w, err)
		return
	}
	token := hex.EncodeToString(value)
	hash := sha256.Sum256([]byte(token))
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "admin-session-" + hex.EncodeToString(hash[:]), Namespace: s.Namespace, Labels: map[string]string{adminSessionLabel: "true"}},
		Data: map[string][]byte{"expires": []byte(time.Now().Add(12 * time.Hour).UTC().Format(time.RFC3339)), "key": expected[:]}}
	if err := s.Client.Create(r.Context(), secret); err != nil {
		adminError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "codespace_admin_session", Value: token, Path: "/api/admin", HttpOnly: true, Secure: s.PublicURL.Scheme == "https", SameSite: http.SameSiteStrictMode, MaxAge: 12 * 60 * 60})
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(token))
	adminJSON(w, http.StatusOK, map[string]string{"csrf": hex.EncodeToString(mac.Sum(nil))})
}

func (s *AdminServer) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("codespace_admin_session")
		if err != nil || len(cookie.Value) != 64 {
			adminJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in required"})
			return
		}
		hash := sha256.Sum256([]byte(cookie.Value))
		var session corev1.Secret
		err = s.Client.Get(r.Context(), types.NamespacedName{Namespace: s.Namespace, Name: "admin-session-" + hex.EncodeToString(hash[:])}, &session)
		if apierrors.IsNotFound(err) {
			adminJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in required"})
			return
		}
		if err != nil {
			adminError(w, err)
			return
		}
		key, err := s.token()
		if err != nil {
			adminError(w, err)
			return
		}
		keyHash := sha256.Sum256(key)
		expires, err := time.Parse(time.RFC3339, string(session.Data["expires"]))
		if err != nil || !time.Now().Before(expires) || session.Labels[adminSessionLabel] != "true" || !session.DeletionTimestamp.IsZero() || !hmac.Equal(session.Data["key"], keyHash[:]) {
			adminJSON(w, http.StatusUnauthorized, map[string]string{"error": "session expired; sign in again"})
			return
		}
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write([]byte(cookie.Value))
		csrf := hex.EncodeToString(mac.Sum(nil))
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !hmac.Equal([]byte(r.Header.Get("X-CSRF-Token")), []byte(csrf)) {
			adminJSON(w, http.StatusForbidden, map[string]string{"error": "invalid CSRF token"})
			return
		}
		if r.URL.Path == "/api/admin/session" {
			switch r.Method {
			case http.MethodGet:
				adminJSON(w, http.StatusOK, map[string]string{"csrf": csrf})
			case http.MethodDelete:
				if err := s.Client.Delete(r.Context(), &session, client.Preconditions{UID: &session.UID}); err != nil && !apierrors.IsNotFound(err) {
					adminError(w, err)
					return
				}
				http.SetCookie(w, &http.Cookie{Name: cookie.Name, Path: "/api/admin", MaxAge: -1, HttpOnly: true, Secure: s.PublicURL.Scheme == "https", SameSite: http.SameSiteStrictMode})
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

func adminDecode(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, api.MaxObjectBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	err := decoder.Decode(value)
	if err == nil && !errors.Is(decoder.Decode(new(any)), io.EOF) {
		err = fmt.Errorf("multiple JSON values")
	}
	if err != nil {
		adminJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON request"})
		return false
	}
	return true
}

func adminJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func adminError(w http.ResponseWriter, err error) {
	status, message := http.StatusServiceUnavailable, "Kubernetes request failed; retry after checking Manager and cluster availability"
	switch {
	case apierrors.IsNotFound(err):
		status, message = http.StatusNotFound, "resource not found"
	case apierrors.IsConflict(err), apierrors.IsAlreadyExists(err):
		status, message = http.StatusConflict, "resource changed; reload before saving"
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		status, message = http.StatusBadRequest, "resource configuration is invalid"
		if apierrors.IsBadRequest(err) {
			message = err.Error()
		}
	}
	adminJSON(w, status, map[string]string{"error": message})
}

func (s *AdminServer) NeedLeaderElection() bool { return true }

func (s *AdminServer) Start(ctx context.Context) error {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		if err := s.pruneSessions(ctx); err != nil && ctx.Err() == nil {
			// Authentication still checks expiration and rotation on every request.
			// Cleanup failures only delay removal of already invalid sessions.
			ctrl.Log.Error(err, "clean administration sessions")
		}
		if err := s.pruneCredentials(ctx); err != nil && ctx.Err() == nil {
			ctrl.Log.Error(err, "clean site credentials")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (s *AdminServer) pruneSessions(ctx context.Context) error {
	key, err := s.token()
	if err != nil {
		return err
	}
	hash := sha256.Sum256(key)
	var sessions corev1.SecretList
	if err := s.Client.List(ctx, &sessions, client.InNamespace(s.Namespace), client.MatchingLabels{adminSessionLabel: "true"}); err != nil {
		return err
	}
	for _, session := range sessions.Items {
		expires, err := time.Parse(time.RFC3339, string(session.Data["expires"]))
		if err == nil && time.Now().Before(expires) && hmac.Equal(session.Data["key"], hash[:]) {
			continue
		}
		if err := s.Client.Delete(ctx, &session, client.Preconditions{UID: &session.UID}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
