// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/docker/distribution/configuration"
	"github.com/docker/distribution/registry/handlers"
	_ "github.com/docker/distribution/registry/storage/driver/filesystem"
	dockerunits "github.com/docker/go-units"

	"gitea.dev/codespace/devcontainer"
	"gitea.dev/codespace/internal/provisioner"
)

const registryCacheUsername = "gitea-codespace"

type registryCache struct {
	enabled           bool
	listen            string
	publicURL         string
	host              string
	storageDir        string
	maxBytes          int64
	maxAge            time.Duration
	gcInterval        time.Duration
	secret            []byte
	codeServerVersion string
	upstreams         map[string]registryCacheUpstream
	handler           http.Handler
}

type registryCacheUpstream struct {
	allow []string
}

type registryCacheToken struct {
	Repository string `json:"repository"`
	Expires    int64  `json:"expires"`
}

func newRegistryCache(config Config, managerSecret string) (*registryCache, error) {
	registry := config.Runtime.Cache.Registry
	if !registry.Enabled {
		return &registryCache{}, nil
	}
	if strings.TrimSpace(managerSecret) == "" {
		return nil, fmt.Errorf("manager secret is required")
	}
	parsed, err := url.Parse(registry.PublicURL)
	if err != nil {
		return nil, fmt.Errorf("parse cache registry public_url: %w", err)
	}
	maxBytes := int64(0)
	if strings.TrimSpace(registry.MaxSize) != "" {
		maxBytes, err = dockerunits.RAMInBytes(registry.MaxSize)
		if err != nil {
			return nil, fmt.Errorf("parse cache registry max_size: %w", err)
		}
		if maxBytes <= 0 {
			return nil, fmt.Errorf("cache registry max_size must be positive")
		}
	}
	upstreams := make(map[string]registryCacheUpstream, len(registry.Upstreams))
	for host, upstream := range registry.Upstreams {
		upstreams[host] = registryCacheUpstream{
			allow: append([]string(nil), upstream.Allow...),
		}
	}
	cache := &registryCache{
		enabled:           true,
		listen:            registry.Listen,
		publicURL:         strings.TrimRight(registry.PublicURL, "/"),
		host:              parsed.Host,
		storageDir:        registry.StoragePath,
		maxBytes:          maxBytes,
		maxAge:            registry.MaxAge.ToStdlib(),
		gcInterval:        registry.GCInterval.ToStdlib(),
		secret:            []byte(managerSecret),
		codeServerVersion: config.Runtime.WebIDE.CodeServerVersion,
		upstreams:         upstreams,
	}
	cache.handler = cache.wrapDistributionHandler()
	return cache, nil
}

func (c *registryCache) OpenListener() (net.Listener, error) {
	if c == nil || !c.enabled {
		return nil, nil
	}
	if err := os.MkdirAll(c.storageDir, 0o700); err != nil {
		return nil, fmt.Errorf("create cache registry storage: %w", err)
	}
	listener, err := net.Listen("tcp", c.listen)
	if err != nil {
		return nil, fmt.Errorf("listen cache registry %s: %w", c.listen, err)
	}
	return listener, nil
}

func (c *registryCache) Handler() http.Handler {
	if c == nil || !c.enabled {
		return http.NotFoundHandler()
	}
	return c.handler
}

func newRegistryCacheHTTPServer(cache *registryCache) *http.Server {
	if cache == nil || !cache.enabled {
		return nil
	}
	return &http.Server{
		Handler:           cache.Handler(),
		ReadHeaderTimeout: gatewayHTTPReadHeaderTime,
		MaxHeaderBytes:    gatewayHTTPMaxHeaderBytes,
	}
}

func registryCacheBuildRegistry(cache *registryCache) string {
	if cache == nil || !cache.enabled {
		return ""
	}
	return cache.publicURL + "/cache"
}

func registryCacheMirrors(cache *registryCache) map[string]string {
	if cache == nil || !cache.enabled {
		return nil
	}
	mirrors := make(map[string]string, len(cache.upstreams))
	for host := range cache.upstreams {
		mirrors[host] = cache.publicURL + "/mirror/" + host
	}
	return mirrors
}

func (c *registryCache) RunGC(ctx context.Context) {
	if c == nil || !c.enabled || c.gcInterval <= 0 || (c.maxAge <= 0 && c.maxBytes <= 0) {
		return
	}
	ticker := time.NewTicker(c.gcInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.prune(ctx); err != nil {
				log.Printf("cache registry cleanup: %v", err)
			}
		}
	}
}

func (c *registryCache) CacheOptions(request provisioner.LifecycleRequest) devcontainer.CacheOptions {
	if c == nil || !c.enabled {
		return devcontainer.CacheOptions{}
	}
	repoHash := registryCacheRepoHash(request.RepoFullName)
	credential := devcontainer.RegistryCredential{
		Username: registryCacheUsername,
		Password: c.issueToken(repoHash, time.Now().Add(24*time.Hour)),
	}
	mirrors := make(map[string]string, len(c.upstreams))
	for host := range c.upstreams {
		mirrors[host] = c.publicURL + "/mirror/" + host
	}
	return devcontainer.CacheOptions{
		BuildRegistry: c.publicURL + "/cache/" + repoHash,
		Mirrors:       mirrors,
		BuildScope:    provisioner.RuntimeBuildCacheScope(request, c.codeServerVersion),
		Credentials: map[string]devcontainer.RegistryCredential{
			c.host:      credential,
			c.publicURL: credential,
		},
	}
}

func (c *registryCache) wrapDistributionHandler() http.Handler {
	config := &configuration.Configuration{
		Version: "0.1",
		Storage: configuration.Storage{
			"filesystem": configuration.Parameters{"rootdirectory": c.storageDir},
			"delete":     configuration.Parameters{"enabled": true},
			"maintenance": configuration.Parameters{
				"uploadpurging": configuration.Parameters{
					"enabled":  true,
					"age":      "24h",
					"interval": "1h",
					"dryrun":   false,
				},
			},
		},
	}
	config.Log.AccessLog.Disabled = true
	config.Log.Level = "error"
	config.HTTP.Secret = hex.EncodeToString(c.sign([]byte("http-secret")))
	app := handlers.NewApp(context.Background(), config)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		repository, action, ok := registryCacheAccess(request)
		if ok {
			if err := c.authorize(request, repository, action); err != nil {
				writer.Header().Set("WWW-Authenticate", `Basic realm="gitea-codespace-registry"`)
				http.Error(writer, err.Error(), http.StatusUnauthorized)
				return
			}
		}
		app.ServeHTTP(writer, request)
	})
}

func registryCacheAccess(request *http.Request) (string, string, bool) {
	pathValue := strings.TrimPrefix(request.URL.EscapedPath(), "/")
	if pathValue == "v2" || pathValue == "v2/" {
		return "", "", false
	}
	if !strings.HasPrefix(pathValue, "v2/") {
		return "", "", false
	}
	pathValue = strings.TrimPrefix(pathValue, "v2/")
	for _, marker := range []string{"/manifests/", "/blobs/", "/tags/"} {
		if repository, _, ok := strings.Cut(pathValue, marker); ok {
			return repository, registryCacheAction(request.Method), true
		}
	}
	return "", registryCacheAction(request.Method), true
}

func registryCacheAction(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead:
		return "pull"
	case http.MethodPost, http.MethodPatch, http.MethodPut:
		return "push"
	default:
		return "other"
	}
}

func (c *registryCache) authorize(request *http.Request, repository, action string) error {
	username, password, ok := request.BasicAuth()
	if !ok || username != registryCacheUsername {
		return errors.New("cache registry credentials are required")
	}
	token, err := c.verifyToken(password, time.Now())
	if err != nil {
		return err
	}
	if strings.HasPrefix(repository, "cache/"+token.Repository+"/") {
		if action == "pull" || action == "push" {
			if err := validateRegistryCacheBlobMount(request, repository); err != nil {
				return err
			}
			return nil
		}
		return errors.New("cache registry action is not allowed")
	}
	if strings.HasPrefix(repository, "mirror/") {
		if action != "pull" && action != "push" {
			return errors.New("cache registry mirror action is not allowed")
		}
		host, imagePath, ok := strings.Cut(strings.TrimPrefix(repository, "mirror/"), "/")
		if !ok || !c.upstreamAllows(host, imagePath) {
			return errors.New("cache registry mirror repository is not allowed")
		}
		if err := validateRegistryCacheBlobMount(request, repository); err != nil {
			return err
		}
		return nil
	}
	return errors.New("cache registry repository is not allowed")
}

func validateRegistryCacheBlobMount(request *http.Request, repository string) error {
	values := request.URL.Query()
	mount := strings.TrimSpace(values.Get("mount"))
	from := strings.TrimSpace(values.Get("from"))
	if mount == "" && from == "" {
		return nil
	}
	if request.Method != http.MethodPost || mount == "" || from == "" {
		return errors.New("cache registry blob mount is invalid")
	}
	if from != repository {
		return errors.New("cache registry blob mount source is not allowed")
	}
	return nil
}

func (c *registryCache) upstreamAllows(host, imagePath string) bool {
	upstream, ok := c.upstreams[host]
	if !ok {
		return false
	}
	if len(upstream.allow) == 0 {
		return true
	}
	for _, pattern := range upstream.allow {
		if strings.HasSuffix(pattern, "*") {
			if strings.HasPrefix(imagePath, strings.TrimSuffix(pattern, "*")) {
				return true
			}
			continue
		}
		if imagePath == pattern {
			return true
		}
	}
	return false
}

func (c *registryCache) issueToken(repository string, expires time.Time) string {
	payload, _ := json.Marshal(registryCacheToken{Repository: repository, Expires: expires.Unix()})
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	signature := base64.RawURLEncoding.EncodeToString(c.sign([]byte(encodedPayload)))
	return encodedPayload + "." + signature
}

func (c *registryCache) verifyToken(value string, now time.Time) (registryCacheToken, error) {
	encodedPayload, encodedSignature, ok := strings.Cut(value, ".")
	if !ok {
		return registryCacheToken{}, errors.New("cache registry token is invalid")
	}
	expected := c.sign([]byte(encodedPayload))
	signature, err := base64.RawURLEncoding.DecodeString(encodedSignature)
	if err != nil || !hmac.Equal(signature, expected) {
		return registryCacheToken{}, errors.New("cache registry token is invalid")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		return registryCacheToken{}, errors.New("cache registry token is invalid")
	}
	var token registryCacheToken
	if err := json.Unmarshal(payload, &token); err != nil {
		return registryCacheToken{}, errors.New("cache registry token is invalid")
	}
	if strings.TrimSpace(token.Repository) == "" || now.Unix() > token.Expires {
		return registryCacheToken{}, errors.New("cache registry token has expired")
	}
	return token, nil
}

func (c *registryCache) sign(payload []byte) []byte {
	mac := hmac.New(sha256.New, c.secret)
	mac.Write(payload)
	return mac.Sum(nil)
}

func registryCacheRepoHash(repoFullName string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(repoFullName))))
	return hex.EncodeToString(sum[:])
}

type registryCacheFile struct {
	path    string
	size    int64
	modTime time.Time
}

func (c *registryCache) prune(ctx context.Context) error {
	files, total, err := c.cacheBlobFiles(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	var errs []error
	for _, file := range files {
		if c.maxAge > 0 && now.Sub(file.modTime) > c.maxAge {
			if err := os.Remove(file.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
				continue
			}
			total -= file.size
		}
	}
	if c.maxBytes <= 0 || total <= c.maxBytes {
		return errors.Join(errs...)
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.Before(files[j].modTime)
	})
	for _, file := range files {
		if total <= c.maxBytes {
			break
		}
		if time.Since(file.modTime) < time.Hour {
			continue
		}
		if err := os.Remove(file.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		total -= file.size
	}
	return errors.Join(errs...)
}

func (c *registryCache) cacheBlobFiles(ctx context.Context) ([]registryCacheFile, int64, error) {
	var files []registryCacheFile
	var total int64
	err := filepath.WalkDir(filepath.Join(c.storageDir, "docker", "registry", "v2", "blobs"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if entry.IsDir() || filepath.Base(path) != "data" {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		files = append(files, registryCacheFile{path: path, size: info.Size(), modTime: info.ModTime()})
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	return files, total, err
}
