// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"fmt"

	"github.com/distribution/reference"
	corev1 "k8s.io/api/core/v1"
)

func platformImagePullPolicy(image string) (corev1.PullPolicy, error) {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return "", fmt.Errorf("invalid platform image: %w", err)
	}
	if _, ok := named.(reference.Digested); ok {
		return corev1.PullIfNotPresent, nil
	}
	if _, ok := named.(reference.Tagged); ok {
		return corev1.PullAlways, nil
	}
	return "", fmt.Errorf("platform image must include an explicit tag or digest")
}
