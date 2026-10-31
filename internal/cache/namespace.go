// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cache

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
)

// Namespace returns a stable, unguessable registry path scoped to one Gitea
// site, repository, and user. BuildKit can use the path as an HTTP registry
// capability without transmitting credentials over the cluster network.
func Namespace(secret []byte, siteUID string, repositoryID, userID int64) string {
	scope := sha256.Sum256(fmt.Appendf(nil, "%s/%d/%d", siteUID, repositoryID, userID))
	prefix := "v1-" + hex.EncodeToString(scope[:])
	signer := hmac.New(sha256.New, secret)
	_, _ = signer.Write([]byte(prefix))
	return prefix + "-" + hex.EncodeToString(signer.Sum(nil))
}

// ValidateNamespace verifies that a build-cache path was issued by this Cache.
func ValidateNamespace(secret []byte, namespace string) bool {
	parts := strings.Split(namespace, "-")
	if len(parts) != 3 || parts[0] != "v1" {
		return false
	}
	if scope, err := hex.DecodeString(parts[1]); err != nil || len(scope) != sha256.Size {
		return false
	}
	provided, err := hex.DecodeString(parts[2])
	if err != nil || len(provided) != sha256.Size {
		return false
	}
	signer := hmac.New(sha256.New, secret)
	_, _ = signer.Write([]byte(strings.Join(parts[:2], "-")))
	return subtle.ConstantTimeCompare(provided, signer.Sum(nil)) == 1
}
