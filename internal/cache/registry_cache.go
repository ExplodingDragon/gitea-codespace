// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cache

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	configpkg "gitea.dev/codespace/internal/config"
	"github.com/distribution/distribution/v3/configuration"
	"github.com/distribution/distribution/v3/registry/handlers"
	storagedriver "github.com/distribution/distribution/v3/registry/storage/driver"
	"github.com/distribution/distribution/v3/registry/storage/driver/factory"
	// Storage drivers register themselves with the distribution factory.
	_ "github.com/distribution/distribution/v3/registry/storage/driver/filesystem"
	_ "github.com/distribution/distribution/v3/registry/storage/driver/s3-aws"
	"github.com/distribution/reference"
	dockerunits "github.com/docker/go-units"
	"github.com/opencontainers/go-digest"
	"golang.org/x/sys/unix"
)

// Username is the fixed registry username paired with Manager-issued cache credentials.
const Username = "gitea-codespace"

type registryCache struct {
	id                   string
	configRevision       int64
	enabled              bool
	listen               string
	storageDir           string
	storageConfig        configpkg.CacheStorageConfig
	driver               storagedriver.StorageDriver
	mirrorDrivers        map[string]storagedriver.StorageDriver
	maxBytes             int64
	minFreeBytes         int64
	maxAge               time.Duration
	gcInterval           time.Duration
	secret               []byte
	tokenKey             []byte
	upstreams            map[string]registryCacheUpstream
	handler              http.Handler
	registries           []*handlers.App
	storageLock          *os.File
	owner                Owner
	maintenance          sync.RWMutex
	maintenanceRequested atomic.Bool
	sessionsMu           sync.Mutex
	sessions             map[string]cachedCacheSession
	statusMu             sync.Mutex
	lastCleanup          time.Time
	status               Status
}

type cachedCacheSession struct {
	Session
	expires time.Time
}

type registryCacheUpstream struct {
	host  string
	allow []string
}

// newRegistryCache opens a registry with scoped authorization supplied by the Manager client.
// The caller must close it to release its storage lock and upstream registries.
func newRegistryCache(ctx context.Context, config configpkg.CacheConfig, cacheKey string, tokenKey []byte) (*registryCache, error) {
	cache, err := newRegistryCacheConfig(config)
	if err != nil || !cache.enabled {
		return cache, err
	}
	if strings.TrimSpace(cacheKey) == "" || len(tokenKey) < 32 {
		return nil, fmt.Errorf("cache registry keys are invalid")
	}
	if cache.storageConfig.Driver == "filesystem" {
		if err := os.MkdirAll(cache.storageDir, 0o700); err != nil {
			return nil, fmt.Errorf("create cache registry storage: %w", err)
		}
		lock, err := os.OpenFile(filepath.Join(cache.storageDir, "registry.lock"), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, err
		}
		if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			_ = lock.Close()
			return nil, fmt.Errorf("lock registry cache storage: %w", err)
		}
		cache.storageLock = lock
	}
	cache.secret = []byte(cacheKey)
	cache.tokenKey = append([]byte(nil), tokenKey...)
	cache.driver, err = factory.Create(ctx, config.Storage.Driver, cache.storageParameters(""))
	if err != nil {
		_ = cache.Close()
		return nil, err
	}
	// Verify write/delete permissions before advertising a ready service.
	probe := "/health/" + rand.Text()
	if err := cache.driver.PutContent(ctx, probe, []byte("ready")); err != nil {
		_ = cache.Close()
		return nil, fmt.Errorf("check cache storage: %w", err)
	}
	if err := cache.driver.Delete(ctx, probe); err != nil {
		_ = cache.Close()
		return nil, err
	}
	cache.mirrorDrivers = make(map[string]storagedriver.StorageDriver, len(cache.upstreams))
	for _, upstream := range cache.upstreams {
		driver, err := factory.Create(ctx, config.Storage.Driver, cache.storageParameters(upstream.host))
		if err != nil {
			_ = cache.Close()
			return nil, err
		}
		cache.mirrorDrivers[upstream.host] = driver
	}
	cache.handler = cache.wrapDistributionHandler(ctx)
	return cache, nil
}

func newRegistryCacheConfig(registry configpkg.CacheConfig) (*registryCache, error) {
	if !registry.Enabled {
		return &registryCache{}, nil
	}
	var err error
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
	minFreeBytes := int64(0)
	if registry.Storage.MinFreeSpace != "" {
		minFreeBytes, err = dockerunits.RAMInBytes(registry.Storage.MinFreeSpace)
		if err != nil {
			return nil, err
		}
	}
	for host, upstream := range registry.Upstreams {
		upstreams[configpkg.CacheUpstreamID(host)] = registryCacheUpstream{
			host:  host,
			allow: append([]string(nil), upstream.Allow...),
		}
	}
	cache := &registryCache{
		id:             registry.ID,
		configRevision: registry.Revision,
		enabled:        true,
		listen:         registry.Listen,
		storageDir:     filepath.Join(registry.Storage.Path, registry.ID),
		storageConfig:  registry.Storage,
		maxBytes:       maxBytes,
		minFreeBytes:   minFreeBytes,
		maxAge:         registry.MaxAge.ToStdlib(),
		gcInterval:     registry.GCInterval.ToStdlib(),
		upstreams:      upstreams,
	}
	return cache, nil
}

func (c *registryCache) Close() error {
	var errs []error
	for _, registry := range c.registries {
		errs = append(errs, registry.Shutdown())
	}
	if c.storageLock != nil {
		errs = append(errs, c.storageLock.Close())
	}
	return errors.Join(errs...)
}

func (c *registryCache) storageParameters(mirror string) map[string]any {
	root := "builds"
	if mirror != "" {
		root = path.Join("mirrors", mirror)
	}
	if c.storageConfig.Driver == "filesystem" {
		return map[string]any{"rootdirectory": filepath.Join(c.storageDir, root)}
	}
	s3 := c.storageConfig.S3
	return map[string]any{"bucket": s3.Bucket, "region": s3.Region, "regionendpoint": s3.Endpoint, "forcepathstyle": s3.ForcePathStyle, "accesskey": s3.AccessKey, "secretkey": s3.SecretKey, "rootdirectory": path.Join(s3.Prefix, c.id, root)}
}

func (c *registryCache) openListener() (net.Listener, error) {
	if c == nil || !c.enabled {
		return nil, nil
	}
	listener, err := net.Listen("tcp", c.listen)
	if err != nil {
		return nil, fmt.Errorf("listen cache registry %s: %w", c.listen, err)
	}
	return listener, nil
}

func (c *registryCache) httpHandler() http.Handler {
	if c == nil || !c.enabled {
		return http.NotFoundHandler()
	}
	return c.handler
}

func (c *registryCache) wrapDistributionHandler(ctx context.Context) http.Handler {
	config := &configuration.Configuration{
		Version: "0.1",
		Storage: configuration.Storage{
			c.storageConfig.Driver: configuration.Parameters(c.storageParameters("")),
			"redirect":             configuration.Parameters{"disable": true},
			"delete":               configuration.Parameters{"enabled": true},
			"maintenance": configuration.Parameters{
				"uploadpurging": map[any]any{
					"enabled":  false,
					"age":      "24h",
					"interval": "1h",
					"dryrun":   false,
				},
			},
		},
	}
	config.Log.AccessLog.Disabled = true
	config.Log.Level = "error"
	config.HTTP.Secret = string(c.secret)
	app := handlers.NewApp(ctx, config)
	c.registries = append(c.registries, app)
	mirrors := make(map[string]*handlers.App, len(c.upstreams))
	localMirrors := make(map[string]*handlers.App, len(c.upstreams))
	for upstreamID, upstream := range c.upstreams {
		remoteURL := "https://" + upstream.host
		if upstream.host == "docker.io" {
			remoteURL = "https://registry-1.docker.io"
		}
		proxyConfig := *config
		proxyConfig.Storage = configuration.Storage{
			c.storageConfig.Driver: configuration.Parameters(c.storageParameters(upstream.host)),
			"redirect":             configuration.Parameters{"disable": true},
			"delete":               configuration.Parameters{"enabled": true},
			"maintenance":          config.Storage["maintenance"],
		}
		localConfig := proxyConfig
		local := handlers.NewApp(ctx, &localConfig)
		localMirrors[upstreamID] = local
		c.registries = append(c.registries, local)
		// The Manager-authorized maintenance pass owns all storage deletion.
		ttl := time.Duration(0)
		proxyConfig.Proxy = configuration.Proxy{RemoteURL: remoteURL, TTL: &ttl}
		proxy := handlers.NewApp(ctx, &proxyConfig)
		mirrors[upstreamID] = proxy
		c.registries = append(c.registries, proxy)
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// A maintenance pass first marks the cache unavailable to new requests,
		// then waits for existing requests to leave the read section.
		if c.maintenanceRequested.Load() || !c.maintenance.TryRLock() {
			writer.Header().Set("Retry-After", "30")
			http.Error(writer, "cache registry maintenance in progress", http.StatusServiceUnavailable)
			return
		}
		defer c.maintenance.RUnlock()
		repository, action := registryCacheAccess(request)
		if err := c.authorize(request, repository, action); err != nil {
			writer.Header().Set("WWW-Authenticate", registryAuthenticationChallenge)
			http.Error(writer, err.Error(), http.StatusUnauthorized)
			return
		}
		mirrorRepository, mirrored := strings.CutPrefix(repository, "mirror/")
		lowSpace := false
		if c.storageConfig.Driver == "filesystem" && (action == "push" || mirrored) {
			var disk unix.Statfs_t
			err := unix.Statfs(c.storageDir, &disk)
			lowSpace = err != nil || disk.Bavail*uint64(disk.Bsize) < uint64(c.minFreeBytes)
			if lowSpace && action == "push" {
				http.Error(writer, "cache disk free space is below the write threshold", http.StatusServiceUnavailable)
				return
			}
		}
		if mirrored {
			upstreamID, _, _ := strings.Cut(mirrorRepository, "/")
			forward := request.Clone(request.Context())
			forward.URL.Path = "/v2/" + strings.TrimPrefix(request.URL.Path, "/v2/mirror/"+upstreamID+"/")
			forward.URL.RawPath = ""
			forward.Header.Del("Authorization")
			if lowSpace {
				// Immutable reads can use local storage without downloading or
				// refreshing a mutable tag. A miss lets the client fall back.
				_, value, found := strings.Cut(forward.URL.Path, "/manifests/")
				if !found {
					_, value, _ = strings.Cut(forward.URL.Path, "/blobs/")
				}
				if _, err := digest.Parse(value); err != nil {
					http.Error(writer, "cache disk free space is below the write threshold", http.StatusServiceUnavailable)
					return
				}
				localMirrors[upstreamID].ServeHTTP(writer, forward)
				return
			}
			mirrors[upstreamID].ServeHTTP(writer, forward)
			return
		}
		app.ServeHTTP(writer, request)
	})
}

func registryCacheAccess(request *http.Request) (string, string) {
	pathValue := strings.TrimPrefix(request.URL.EscapedPath(), "/")
	if pathValue == "v2" || pathValue == "v2/" {
		return "", "ping"
	}
	if !strings.HasPrefix(pathValue, "v2/") {
		return "", registryCacheAction(request.Method)
	}
	pathValue = strings.TrimPrefix(pathValue, "v2/")
	for _, marker := range []string{"/manifests/", "/blobs/", "/tags/"} {
		if repository, _, ok := strings.Cut(pathValue, marker); ok {
			return repository, registryCacheAction(request.Method)
		}
	}
	return "", registryCacheAction(request.Method)
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
	token, err := c.registrySession(request)
	if err != nil {
		return err
	}
	if repository == "" && action == "ping" && (request.Method == http.MethodGet || request.Method == http.MethodHead) {
		return nil
	}
	if token.Policy.Build && strings.HasPrefix(repository, "cache/"+token.Namespace+"/") {
		if _, err := reference.WithName(repository); err != nil {
			return errors.New("cache registry repository is invalid")
		}
		if action == "pull" || action == "push" {
			if err := validateRegistryCacheBlobMount(request, "cache/"+token.Namespace+"/"); err != nil {
				return err
			}
			return nil
		}
		return errors.New("cache registry action is not allowed")
	}
	if token.Policy.Public && strings.HasPrefix(repository, "mirror/") {
		if action != "pull" {
			return errors.New("cache registry mirror action is not allowed")
		}
		upstreamID, imagePath, ok := strings.Cut(strings.TrimPrefix(repository, "mirror/"), "/")
		if !ok || !c.upstreamAllows(upstreamID, imagePath) {
			return errors.New("cache registry mirror repository is not allowed")
		}
		if err := validateRegistryCacheBlobMount(request, repository); err != nil {
			return err
		}
		return nil
	}
	return errors.New("cache registry repository is not allowed")
}

func validateRegistryCacheBlobMount(request *http.Request, namespacePrefix string) error {
	values := request.URL.Query()
	mount := strings.TrimSpace(values.Get("mount"))
	from := values.Get("from")
	if mount == "" && from == "" {
		return nil
	}
	if request.Method != http.MethodPost || mount == "" || from == "" {
		return errors.New("cache registry blob mount is invalid")
	}
	if _, err := reference.WithName(from); err != nil || !strings.HasPrefix(from, namespacePrefix) {
		return errors.New("cache registry blob mount source is not allowed")
	}
	return nil
}

func (c *registryCache) upstreamAllows(upstreamID, imagePath string) bool {
	upstream, ok := c.upstreams[upstreamID]
	if !ok {
		return false
	}
	for _, pattern := range upstream.allow {
		if prefix, wildcard := strings.CutSuffix(pattern, "*"); wildcard {
			if strings.HasPrefix(imagePath, prefix) {
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
