// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cache

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"time"

	"github.com/distribution/distribution/v3"
	"github.com/distribution/distribution/v3/registry/storage"
	storagedriver "github.com/distribution/distribution/v3/registry/storage/driver"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	"golang.org/x/sys/unix"
)

func (c *registryCache) lockMaintenance(ctx context.Context) error {
	for {
		if c.maintenance.TryLock() {
			return nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func deleteCacheRepository(ctx context.Context, registry distribution.Namespace, repositoryName string) error {
	name, err := reference.WithName(repositoryName)
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
	var unknown distribution.ErrRepositoryUnknown
	if err != nil && !errors.As(err, &unknown) {
		return err
	}
	for _, tag := range tags {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := repository.Tags(ctx).Untag(ctx, tag); err != nil {
			return err
		}
	}
	for _, value := range digests {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := manifests.Delete(ctx, value); err != nil {
			return err
		}
	}
	return nil
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

type cacheStoreScan struct {
	driver       storagedriver.StorageDriver
	registry     distribution.Namespace
	mirror       bool
	bytes        int64
	needsCleanup bool
}

type cacheRepositoryAge struct {
	store    *cacheStoreScan
	name     string
	updated  time.Time
	untagged bool
}

type cacheStorageScan struct {
	stores       []*cacheStoreScan
	repositories []cacheRepositoryAge
	needsCleanup bool
}

// scanStorage is read-only. Concurrent uploads may make a scan fail; only a
// complete snapshot is published, and maintenance repeats it under the gate.
func (c *registryCache) scanStorage(ctx context.Context) (*cacheStorageScan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scan := &cacheStorageScan{}
	drivers := maps.Clone(c.mirrorDrivers)
	if drivers == nil {
		drivers = make(map[string]storagedriver.StorageDriver)
	}
	drivers[""] = c.driver
	for _, name := range slices.Sorted(maps.Keys(drivers)) {
		driver := maintenanceStorage{drivers[name]}
		registry, err := storage.NewRegistry(ctx, driver, storage.EnableDelete)
		if err != nil {
			return nil, err
		}
		store := &cacheStoreScan{driver: driver, registry: registry, mirror: name != ""}
		scan.stores = append(scan.stores, store)
		// Build roots are current tags; mirrors also retain digest-only pulls.
		graph := make(map[digest.Digest][]distribution.Descriptor)
		var roots []digest.Digest
		err = registry.(distribution.RepositoryEnumerator).Enumerate(ctx, func(repositoryName string) error {
			named, err := reference.WithName(repositoryName)
			if err != nil {
				return err
			}
			repository, err := registry.Repository(ctx, named)
			if err != nil {
				return err
			}
			manifests, err := repository.Manifests(ctx)
			if err != nil {
				return err
			}
			err = manifests.(distribution.ManifestEnumerator).Enumerate(ctx, func(value digest.Digest) error {
				manifest, err := manifests.Get(ctx, value)
				if err != nil {
					return err
				}
				graph[value] = manifest.References()
				if store.mirror {
					roots = append(roots, value)
				}
				return nil
			})
			var missing storagedriver.PathNotFoundError
			if err != nil && !errors.As(err, &missing) {
				return err
			}
			untagged := false
			if !store.mirror {
				tags, err := repository.Tags(ctx).All(ctx)
				var unknown distribution.ErrRepositoryUnknown
				if err != nil && !errors.As(err, &unknown) {
					return err
				}
				untagged = len(tags) == 0
				for _, tag := range tags {
					value, err := repository.Tags(ctx).Get(ctx, tag)
					if err != nil {
						return err
					}
					roots = append(roots, value.Digest)
				}
			}
			latest := time.Time{}
			err = driver.Walk(ctx, path.Join("/docker/registry/v2/repositories", repositoryName, "_manifests"), func(info storagedriver.FileInfo) error {
				if info.IsDir() {
					return nil
				}
				if info.ModTime().After(latest) {
					latest = info.ModTime()
				}
				return nil
			})
			if !latest.IsZero() {
				scan.repositories = append(scan.repositories, cacheRepositoryAge{store: store, name: repositoryName, updated: latest, untagged: untagged})
				if c.maxAge > 0 && time.Since(latest) > c.maxAge {
					store.needsCleanup = true
				}
			}
			return err
		})
		var missing storagedriver.PathNotFoundError
		if err != nil && !(errors.As(err, &missing) && missing.Path == "/docker/registry/v2/repositories") {
			return nil, fmt.Errorf("scan cache %q: %w", name, err)
		}
		if _, err := driver.Stat(ctx, "/docker/registry/v2/repositories"); err == nil {
			uploads, errs := storage.PurgeUploads(ctx, driver, time.Now().Add(-24*time.Hour), false)
			if err := errors.Join(errs...); err != nil {
				return nil, err
			}
			store.needsCleanup = store.needsCleanup || len(uploads) > 0
		} else if !errors.As(err, &missing) {
			return nil, err
		}
		marked := make(map[digest.Digest]struct{})
		for len(roots) > 0 {
			value := roots[len(roots)-1]
			roots = roots[:len(roots)-1]
			if _, exists := marked[value]; exists {
				continue
			}
			marked[value] = struct{}{}
			for _, descriptor := range graph[value] {
				roots = append(roots, descriptor.Digest)
			}
		}
		err = driver.Walk(ctx, "/docker/registry/v2/blobs", func(info storagedriver.FileInfo) error {
			if info.IsDir() || path.Base(info.Path()) != "data" {
				return nil
			}
			store.bytes += info.Size()
			directory := path.Dir(info.Path())
			value := digest.Digest(path.Base(path.Dir(path.Dir(directory))) + ":" + path.Base(directory))
			// Give just-committed blobs the same grace period as credentials.
			if _, referenced := marked[value]; !referenced && time.Since(info.ModTime()) >= time.Hour {
				store.needsCleanup = true
			}
			return nil
		})
		if err != nil && !(errors.As(err, &missing) && missing.Path == "/docker/registry/v2/blobs") {
			return nil, fmt.Errorf("scan cache %q blobs: %w", name, err)
		}
		scan.needsCleanup = scan.needsCleanup || store.needsCleanup
	}
	slices.SortFunc(scan.repositories, func(a, b cacheRepositoryAge) int { return a.updated.Compare(b.updated) })
	status := scan.usage()
	pressure, err := c.storagePressure(status.CacheBytes + status.MirrorBytes)
	if err != nil {
		return nil, err
	}
	if pressure {
		for _, repository := range scan.repositories {
			if time.Since(repository.updated) >= time.Hour {
				scan.needsCleanup = true
				break
			}
		}
	}
	c.statusMu.Lock()
	c.status.CacheBytes, c.status.MirrorBytes = status.CacheBytes, status.MirrorBytes
	c.statusMu.Unlock()
	return scan, nil
}

func (s *cacheStorageScan) usage() Status {
	var status Status
	for _, store := range s.stores {
		if store.mirror {
			status.MirrorBytes += store.bytes
		} else {
			status.CacheBytes = store.bytes
		}
	}
	return status
}

func (c *registryCache) storagePressure(bytes int64) (bool, error) {
	pressure := c.maxBytes > 0 && bytes > c.maxBytes
	if c.storageConfig.Driver == "filesystem" && c.minFreeBytes > 0 {
		var disk unix.Statfs_t
		if err := unix.Statfs(c.storageDir, &disk); err != nil {
			return false, err
		}
		pressure = pressure || disk.Bavail*uint64(disk.Bsize) < uint64(c.minFreeBytes)
	}
	return pressure, nil
}

func (c *registryCache) prune(ctx context.Context) (err error) {
	defer func() {
		c.statusMu.Lock()
		defer c.statusMu.Unlock()
		c.lastCleanup = time.Now().UTC()
		c.status.CleanupResult = "completed"
		if err != nil {
			c.status.CleanupResult = "failed; inspect the manager log"
		}
	}()
	c.maintenanceRequested.Store(true)
	defer c.maintenanceRequested.Store(false)
	if err := c.lockMaintenance(ctx); err != nil {
		return err
	}
	defer c.maintenance.Unlock()
	scan, err := c.scanStorage(ctx)
	if err != nil {
		return err
	}
	var remaining []cacheRepositoryAge
	for _, repository := range scan.repositories {
		if repository.untagged || c.maxAge > 0 && time.Since(repository.updated) > c.maxAge {
			if err := deleteCacheRepository(ctx, repository.store.registry, repository.name); err != nil {
				return err
			}
			repository.store.needsCleanup = true
		} else {
			remaining = append(remaining, repository)
		}
	}
	var affected []*cacheStoreScan
	for _, store := range scan.stores {
		if store.needsCleanup {
			affected = append(affected, store)
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, store := range affected {
			_, statErr := store.driver.Stat(ctx, "/docker/registry/v2/repositories")
			var missing storagedriver.PathNotFoundError
			if errors.As(statErr, &missing) {
				// A failed upload can commit blobs before any repository links exist.
				if store.bytes > 0 {
					if err := store.driver.Delete(ctx, "/docker/registry/v2/blobs"); err != nil {
						return err
					}
					store.bytes = 0
				}
				continue
			}
			if statErr != nil {
				return statErr
			}
			if _, errs := storage.PurgeUploads(ctx, store.driver, time.Now().Add(-24*time.Hour), true); len(errs) > 0 {
				return errors.Join(errs...)
			}
			if store.bytes > 0 {
				if err := storage.MarkAndSweep(ctx, store.driver, store.registry, storage.GCOpts{RemoveUntagged: !store.mirror, Quiet: true}); err != nil {
					return err
				}
			}
			store.bytes, err = cacheBlobBytes(ctx, store.driver)
			if err != nil {
				return err
			}
		}
		status := scan.usage()
		c.statusMu.Lock()
		c.status.CacheBytes, c.status.MirrorBytes = status.CacheBytes, status.MirrorBytes
		c.statusMu.Unlock()
		pressure, err := c.storagePressure(status.CacheBytes + status.MirrorBytes)
		if err != nil || !pressure {
			return err
		}
		affected = nil
		// Reclaim in bounded batches, then measure actual bytes: shared layers
		// mean repository sizes cannot predict how much space deletion frees.
		batch := min(16, len(remaining))
		for _, repository := range remaining[:batch] {
			if time.Since(repository.updated) < time.Hour {
				continue
			}
			if err := deleteCacheRepository(ctx, repository.store.registry, repository.name); err != nil {
				return err
			}
			if !slices.Contains(affected, repository.store) {
				affected = append(affected, repository.store)
			}
		}
		remaining = remaining[batch:]
		if len(affected) == 0 {
			return nil
		}
	}
}

// Distribution's filesystem driver ignores context. Maintenance must stop its
// scans and remaining deletions when the Manager permit expires.
type maintenanceStorage struct {
	storagedriver.StorageDriver
}

func (d maintenanceStorage) Delete(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return d.StorageDriver.Delete(ctx, path)
}

func (d maintenanceStorage) Walk(ctx context.Context, path string, visit storagedriver.WalkFn, options ...func(*storagedriver.WalkOptions)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return d.StorageDriver.Walk(ctx, path, func(info storagedriver.FileInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return visit(info)
	}, options...)
}
