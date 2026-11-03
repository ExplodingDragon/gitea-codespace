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
	"strconv"
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

type adminProblem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
}

type AdminServer struct {
	Client                 client.Client
	Namespace              string
	PublicURL              *url.URL
	TokenFile              string
	RegistrationHTTPClient *http.Client
	loginLimit             *rate.Limiter
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
	s := &AdminServer{
		Client: reader, Namespace: namespace, PublicURL: u, TokenFile: tokenFile,
		RegistrationHTTPClient: &http.Client{
			Timeout: 15 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		loginLimit: rate.NewLimiter(1, 5),
	}
	if _, err := s.readAdminToken(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *AdminServer) readAdminToken() ([]byte, error) {
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
	mux.Handle("/api/admin/", s.authenticate(http.HandlerFunc(s.handleResources)))
	mux.Handle("/", web.Handler())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("Origin") != s.PublicURL.String() {
			writeAdminProblem(w, r, http.StatusForbidden, "invalid_origin", "")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *AdminServer) login(w http.ResponseWriter, r *http.Request) {
	if !s.loginLimit.Allow() {
		w.Header().Set("Retry-After", "1")
		writeAdminProblem(w, r, http.StatusTooManyRequests, "rate_limited", "")
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if !decodeAdminJSON(w, r, &body) {
		return
	}
	key, err := s.readAdminToken()
	if err != nil {
		writeAdminError(w, r, err)
		return
	}
	provided, expected := sha256.Sum256([]byte(body.Token)), sha256.Sum256(key)
	if subtle.ConstantTimeCompare(provided[:], expected[:]) != 1 {
		writeAdminProblem(w, r, http.StatusUnauthorized, "authentication_failed", "")
		return
	}
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		writeAdminError(w, r, err)
		return
	}
	token := hex.EncodeToString(value)
	hash := sha256.Sum256([]byte(token))
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "admin-session-" + hex.EncodeToString(hash[:]), Namespace: s.Namespace, Labels: map[string]string{adminSessionLabel: "true"}},
		Data: map[string][]byte{"expires": []byte(time.Now().Add(12 * time.Hour).UTC().Format(time.RFC3339)), "key": expected[:]}}
	if err := s.Client.Create(r.Context(), secret); err != nil {
		writeAdminError(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "codespace_admin_session", Value: token, Path: "/api/admin", HttpOnly: true, Secure: s.PublicURL.Scheme == "https", SameSite: http.SameSiteStrictMode, MaxAge: 12 * 60 * 60})
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(token))
	writeAdminJSON(w, http.StatusOK, map[string]string{"csrf": hex.EncodeToString(mac.Sum(nil))})
}

func (s *AdminServer) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("codespace_admin_session")
		if err != nil || len(cookie.Value) != 64 {
			writeAdminProblem(w, r, http.StatusUnauthorized, "authentication_required", "")
			return
		}
		hash := sha256.Sum256([]byte(cookie.Value))
		var session corev1.Secret
		err = s.Client.Get(r.Context(), types.NamespacedName{Namespace: s.Namespace, Name: "admin-session-" + hex.EncodeToString(hash[:])}, &session)
		if apierrors.IsNotFound(err) {
			writeAdminProblem(w, r, http.StatusUnauthorized, "authentication_required", "")
			return
		}
		if err != nil {
			writeAdminError(w, r, err)
			return
		}
		key, err := s.readAdminToken()
		if err != nil {
			writeAdminError(w, r, err)
			return
		}
		keyHash := sha256.Sum256(key)
		expires, err := time.Parse(time.RFC3339, string(session.Data["expires"]))
		if err != nil || !time.Now().Before(expires) || session.Labels[adminSessionLabel] != "true" || !session.DeletionTimestamp.IsZero() || !hmac.Equal(session.Data["key"], keyHash[:]) {
			writeAdminProblem(w, r, http.StatusUnauthorized, "session_expired", "")
			return
		}
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write([]byte(cookie.Value))
		csrf := hex.EncodeToString(mac.Sum(nil))
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !hmac.Equal([]byte(r.Header.Get("X-CSRF-Token")), []byte(csrf)) {
			writeAdminProblem(w, r, http.StatusForbidden, "request_forbidden", "")
			return
		}
		if r.URL.Path == "/api/admin/session" {
			switch r.Method {
			case http.MethodGet:
				writeAdminJSON(w, http.StatusOK, map[string]string{"csrf": csrf})
			case http.MethodDelete:
				if err := s.Client.Delete(r.Context(), &session, client.Preconditions{UID: &session.UID}); err != nil && !apierrors.IsNotFound(err) {
					writeAdminError(w, r, err)
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

func decodeAdminJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, api.MaxObjectBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	err := decoder.Decode(value)
	if err == nil && !errors.Is(decoder.Decode(new(any)), io.EOF) {
		err = fmt.Errorf("multiple JSON values")
	}
	if err != nil {
		writeAdminProblem(w, r, http.StatusBadRequest, "invalid_request", "invalid JSON request")
		return false
	}
	return true
}

func writeAdminJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAdminProblem(w http.ResponseWriter, r *http.Request, status int, code, detail string) {
	message := web.Localize(r.Header.Get("Accept-Language"), "errors."+code, map[string]string{"status": strconv.Itoa(status)})
	writeAdminJSON(w, status, adminProblem{Code: code, Message: message, Detail: detail})
}

func writeAdminError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, detail := http.StatusServiceUnavailable, "cluster_unavailable", err.Error()
	switch {
	case apierrors.IsNotFound(err):
		status, code, detail = http.StatusNotFound, "resource_not_found", ""
	case apierrors.IsConflict(err), apierrors.IsAlreadyExists(err):
		status, code, detail = http.StatusConflict, "resource_conflict", ""
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		status, code, detail = http.StatusBadRequest, "invalid_configuration", err.Error()
	}
	writeAdminProblem(w, r, status, code, detail)
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
	key, err := s.readAdminToken()
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
