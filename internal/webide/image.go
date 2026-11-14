// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package webide

import (
	_ "crypto/sha256" // Register the digest algorithm used by image references.
	"fmt"
	"strings"

	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
)

// ParseImageReference normalizes Docker image names and keeps the selected tag
// or digest. Omitted registries and tags use Docker Hub and latest.
func ParseImageReference(image string) (reference.Named, error) {
	image = strings.TrimSpace(image)
	if image == "" || len(image) > 4096 {
		return nil, fmt.Errorf("web IDE image is required and must be at most 4096 characters")
	}
	named, err := reference.ParseDockerRef(image)
	if err != nil {
		return nil, fmt.Errorf("invalid Web IDE image: %w", err)
	}
	if digested, ok := named.(reference.Digested); ok {
		if digested.Digest().Algorithm() != digest.SHA256 {
			return nil, fmt.Errorf("web IDE image digest must use sha256")
		}
	}
	return named, nil
}
