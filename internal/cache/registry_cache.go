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
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	configpkg "gitea.dev/codespace/internal/config"
	"github.com/distribution/distribution/v3"
	"github.com/distribution/distribution/v3/configuration"
	"github.com/distribution/distribution/v3/registry/handlers"
	"github.com/distribution/distribution/v3/registry/storage"
	storagedriver "github.com/distribution/distribution/v3/registry/storage/driver"
	"github.com/distribution/distribution/v3/registry/storage/driver/factory"
	_ "github.com/distribution/distribution/v3/registry/storage/driver/filesystem"
	_ "github.com/distribution/distribution/v3/registry/storage/driver/s3-aws"
	"github.com/distribution/reference"
	dockerunits "github.com/docker/go-units"
	"github.com/opencontainers/go-digest"
	"golang.org/x/sys/unix"
)

const Username = "gitea-codespace"

type registryCache struct {
	id             string
	configRevision int64
	enabled        bool
	listen         string
	publicURL      string
	host           string
	storageDir     string
	storageConfig  configpkg.CacheStorageConfig
	driver         storagedriver.StorageDriver
	mirrorDrivers  map[string]storagedriver.StorageDriver
	maxBytes       int64
	minFreeBytes   int64
	maxAge         time.Duration
	gcInterval     time.Duration
	secret         []byte
	tokenKey       []byte
	upstreams      map[string]registryCacheUpstream
	handler        http.Handler
	registries     []*handlers.App
	storageLock    *os.File
	owner          Owner
	maintenance    sync.RWMutex
	sessionsMu     sync.Mutex
	sessions       map[string]cachedCacheSession
	cleanupMu      sync.Mutex
	lastCleanup    time.Time
	cleanupResult  string
}

type cachedCacheSession struct {
	Session
	expires time.Time
}

type registryCacheUpstream struct {
	allow []string
}

// New opens a registry with scoped authorization supplied by the Manager client.
// The caller must close it to release its storage lock and upstream registries.
func New(ctx context.Context, config configpkg.CacheConfig, cacheKey string, tokenKey []byte) (*registryCache, error) {
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
	for host := range cache.upstreams {
		driver, err := factory.Create(ctx, config.Storage.Driver, cache.storageParameters(host))
		if err != nil {
			_ = cache.Close()
			return nil, err
		}
		cache.mirrorDrivers[host] = driver
	}
	cache.handler = cache.wrapDistributionHandler(ctx)
	return cache, nil
}

func newRegistryCacheConfig(registry configpkg.CacheConfig) (*registryCache, error) {
	if !registry.Enabled {
		return &registryCache{}, nil
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
	minFreeBytes := int64(1 << 30)
	if registry.Storage.MinFreeSpace != "" {
		minFreeBytes, err = dockerunits.RAMInBytes(registry.Storage.MinFreeSpace)
		if err != nil {
			return nil, err
		}
	}
	for host, upstream := range registry.Upstreams {
		upstreams[host] = registryCacheUpstream{
			allow: append([]string(nil), upstream.Allow...),
		}
	}
	cache := &registryCache{
		id:             registry.ID,
		configRevision: registry.Revision,
		enabled:        true,
		listen:         registry.Listen,
		publicURL:      strings.TrimRight(registry.PublicURL, "/"),
		host:           parsed.Host,
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

func (c *registryCache) OpenListener() (net.Listener, error) {
	if c == nil || !c.enabled {
		return nil, nil
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
	for host := range c.upstreams {
		remoteURL := "https://" + host
		if host == "docker.io" {
			remoteURL = "https://registry-1.docker.io"
		}
		proxyConfig := *config
		proxyConfig.Storage = configuration.Storage{
			c.storageConfig.Driver: configuration.Parameters(c.storageParameters(host)),
			"redirect":             configuration.Parameters{"disable": true},
			"delete":               configuration.Parameters{"enabled": true},
			"maintenance":          config.Storage["maintenance"],
		}
		proxyConfig.Proxy = configuration.Proxy{RemoteURL: remoteURL, TTL: &c.maxAge}
		proxy := handlers.NewApp(ctx, &proxyConfig)
		mirrors[host] = proxy
		c.registries = append(c.registries, proxy)
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// Wait for in-flight requests before GC and reject new requests during it.
		if !c.maintenance.TryRLock() {
			writer.Header().Set("Retry-After", "30")
			http.Error(writer, "cache registry maintenance in progress", http.StatusServiceUnavailable)
			return
		}
		defer c.maintenance.RUnlock()
		repository, action, ok := registryCacheAccess(request)
		if ok {
			if err := c.authorize(request, repository, action); err != nil {
				writer.Header().Set("WWW-Authenticate", `Basic realm="gitea-codespace-registry"`)
				http.Error(writer, err.Error(), http.StatusUnauthorized)
				return
			}
		}
		if c.storageConfig.Driver == "filesystem" && (action == "push" || strings.HasPrefix(repository, "mirror/")) {
			var disk unix.Statfs_t
			if err := unix.Statfs(c.storageDir, &disk); err != nil || disk.Bavail*uint64(disk.Bsize) < uint64(c.minFreeBytes) {
				http.Error(writer, "cache disk free space is below the write threshold", http.StatusServiceUnavailable)
				return
			}
		}
		if strings.HasPrefix(repository, "mirror/") {
			host, _, _ := strings.Cut(strings.TrimPrefix(repository, "mirror/"), "/")
			forward := request.Clone(request.Context())
			forward.URL.Path = "/v2/" + strings.TrimPrefix(request.URL.Path, "/v2/mirror/"+host+"/")
			forward.URL.RawPath = ""
			forward.Header.Del("Authorization")
			mirrors[host].ServeHTTP(writer, forward)
			return
		}
		app.ServeHTTP(writer, request)
	})
}

func registryCacheAccess(request *http.Request) (string, string, bool) {
	pathValue := strings.TrimPrefix(request.URL.EscapedPath(), "/")
	if pathValue == "v2" || pathValue == "v2/" {
		return "", "ping", true
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
	if value, _, ok := strings.Cut(strings.TrimPrefix(repository, "cache/"), "/"); ok && ValidateNamespace(c.secret, value) {
		if action != "pull" && action != "push" {
			return errors.New("cache registry action is not allowed")
		}
		return validateRegistryCacheBlobMount(request, repository)
	}
	username, password, ok := request.BasicAuth()
	if !ok || username != Username {
		return errors.New("cache registry credentials are required")
	}
	token, err := c.verifyToken(request.Context(), password)
	if err != nil {
		return err
	}
	if repository == "" && action == "ping" && (request.Method == http.MethodGet || request.Method == http.MethodHead) {
		return nil
	}
	if token.Policy.Build && strings.HasPrefix(repository, "cache/"+token.Namespace+"/") {
		if action == "pull" || action == "push" {
			if err := validateRegistryCacheBlobMount(request, repository); err != nil {
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

func (c *registryCache) prune(ctx context.Context) (err error) {
	result := "completed"
	defer func() {
		c.cleanupMu.Lock()
		defer c.cleanupMu.Unlock()
		c.lastCleanup = time.Now().UTC()
		if err != nil {
			result = "failed; inspect the manager log"
		}
		c.cleanupResult = result
	}()
	c.maintenance.Lock()
	defer c.maintenance.Unlock()
	total, err := cacheBlobBytes(ctx, c.driver)
	if err != nil {
		return err
	}
	driver := c.driver
	if total == 0 {
		if _, err := driver.Stat(ctx, "/docker/registry/v2/repositories"); err != nil {
			var missing storagedriver.PathNotFoundError
			if errors.As(err, &missing) {
				return nil
			}
			return err
		}
	}
	if _, errs := storage.PurgeUploads(ctx, driver, time.Now().Add(-24*time.Hour), true); len(errs) != 0 {
		var unexpected []error
		for _, err := range errs {
			var missing storagedriver.PathNotFoundError
			if !errors.As(err, &missing) {
				unexpected = append(unexpected, err)
			}
		}
		if err := errors.Join(unexpected...); err != nil {
			return err
		}
	}
	if total == 0 {
		return nil
	}
	registry, err := storage.NewRegistry(ctx, driver, storage.EnableDelete)
	if err != nil {
		return err
	}
	// Retention evicts complete repositories. Shared layer blobs are collected
	// only after all surviving manifests have been marked by Distribution.
	type retentionCandidate struct {
		name    string
		updated time.Time
	}
	var candidates []retentionCandidate
	err = registry.(distribution.RepositoryEnumerator).Enumerate(ctx, func(name string) error {
		latest := time.Time{}
		root := path.Join("/docker/registry/v2/repositories", name, "_manifests")
		err := driver.Walk(ctx, root, func(info storagedriver.FileInfo) error {
			if !info.IsDir() && info.ModTime().After(latest) {
				latest = info.ModTime()
			}
			return nil
		})
		var missing storagedriver.PathNotFoundError
		if errors.As(err, &missing) {
			return nil
		}
		if err == nil && !latest.IsZero() {
			candidates = append(candidates, retentionCandidate{name: name, updated: latest})
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("enumerate cache retention candidates: %w", err)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].updated.Before(candidates[j].updated) })
	for _, candidate := range candidates {
		age := time.Since(candidate.updated)
		expired := c.maxAge > 0 && age > c.maxAge
		overCapacity := c.maxBytes > 0 && total > c.maxBytes && age >= time.Hour
		if !expired && !overCapacity {
			continue
		}
		name, err := reference.WithName(candidate.name)
		if err != nil {
			return err
		}
		repository, err := registry.Repository(ctx, name)
		if err != nil {
			return err
		}
		manifests, err := repository.Manifests(ctx)
		if err != nil {
			return err
		}
		var digests []digest.Digest
		if err := manifests.(distribution.ManifestEnumerator).Enumerate(ctx, func(value digest.Digest) error {
			digests = append(digests, value)
			return nil
		}); err != nil {
			return err
		}
		tags, err := repository.Tags(ctx).All(ctx)
		if err != nil {
			return err
		}
		for _, tag := range tags {
			if err := repository.Tags(ctx).Untag(ctx, tag); err != nil {
				return err
			}
		}
		for _, value := range digests {
			if err := manifests.Delete(ctx, value); err != nil {
				return err
			}
		}
		if err := storage.MarkAndSweep(ctx, driver, registry, storage.GCOpts{}); err != nil {
			return fmt.Errorf("collect cache registry: %w", err)
		}
		total, err = cacheBlobBytes(ctx, driver)
		if err != nil {
			return err
		}
	}
	// Abandoned uploads can leave committed blobs without any manifest.
	// Collect them even when retention did not evict a repository this time.
	return storage.MarkAndSweep(ctx, driver, registry, storage.GCOpts{})
}

func cacheBlobBytes(ctx context.Context, driver storagedriver.StorageDriver) (int64, error) {
	var total int64
	err := driver.Walk(ctx, "/docker/registry/v2/blobs", func(info storagedriver.FileInfo) error {
		if !info.IsDir() && path.Base(info.Path()) == "data" {
			total += info.Size()
		}
		return nil
	})
	var missing storagedriver.PathNotFoundError
	if errors.As(err, &missing) {
		return 0, nil
	}
	return total, err
}
