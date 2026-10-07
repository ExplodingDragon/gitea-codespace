// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package config

import (
	"fmt"
	"strings"
)

// CacheConfig is an independently deployed registry's persistent configuration.
type CacheConfig struct {
	ID         string                                `json:"id"`
	Name       string                                `json:"name"`
	Revision   int64                                 `json:"revision,string"`
	Enabled    bool                                  `json:"enabled"`
	Listen     string                                `json:"listen"`
	PublicURL  string                                `json:"public_url"`
	Storage    CacheStorageConfig                    `json:"storage"`
	MaxSize    string                                `json:"max_size"`
	MaxAge     Duration                              `json:"max_age"`
	GCInterval Duration                              `json:"gc_interval"`
	Upstreams  map[string]RuntimeCacheUpstreamConfig `json:"upstreams"`
}

// CacheStorageConfig selects one Distribution storage driver.
type CacheStorageConfig struct {
	Driver       string        `json:"driver"`
	Path         string        `json:"path"`
	MinFreeSpace string        `json:"min_free_space"`
	S3           CacheS3Config `json:"s3"`
}

// CacheS3Config uses node IAM when explicit credentials are empty.
type CacheS3Config struct {
	Endpoint       string `json:"endpoint"`
	Region         string `json:"region"`
	Bucket         string `json:"bucket"`
	Prefix         string `json:"prefix"`
	ForcePathStyle bool   `json:"force_path_style"`
	AccessKey      string `json:"access_key,omitempty"`
	SecretKey      string `json:"secret_key,omitempty"`
}

func ValidCacheID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, ch := range id {
		if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '-' && ch != '_' {
			return false
		}
	}
	return true
}

func (c CacheConfig) Validate() error {
	if !ValidCacheID(c.ID) || strings.TrimSpace(c.Name) == "" || len(c.Name) > 100 {
		return fmt.Errorf("cache ID and name are required")
	}
	if c.Revision < 1 {
		return fmt.Errorf("cache revision must be positive")
	}
	if c.Storage.Driver != "filesystem" && c.Storage.Driver != "s3" {
		return fmt.Errorf("cache storage driver must be filesystem or s3")
	}
	if c.Storage.Driver == "filesystem" && c.Storage.Path == "" {
		return fmt.Errorf("cache storage path is required")
	}
	if c.Storage.Driver == "s3" {
		if c.Storage.S3.Endpoint != "" {
			if err := ValidateRegistryRootURL(c.Storage.S3.Endpoint); err != nil {
				return fmt.Errorf("S3 endpoint: %w", err)
			}
		}
		for _, component := range strings.Split(c.Storage.S3.Prefix, "/") {
			if component == "." || component == ".." {
				return fmt.Errorf("S3 prefix must use explicit directory names")
			}
		}
		if c.Storage.S3.Bucket == "" || c.Storage.S3.Region == "" {
			return fmt.Errorf("S3 bucket and region are required")
		}
		if (c.Storage.S3.AccessKey == "") != (c.Storage.S3.SecretKey == "") {
			return fmt.Errorf("S3 access key and secret key must be configured together")
		}
		if c.Storage.MinFreeSpace != "" {
			return fmt.Errorf("free space applies to filesystem storage")
		}
	}
	return c.validateRegistry()
}
