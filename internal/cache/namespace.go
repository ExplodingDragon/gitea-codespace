// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cache

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// Namespace returns a stable, unguessable registry path scoped to one Gitea
// site, repository, and user. BuildKit can use the path as an HTTP registry
// capability without transmitting credentials over the cluster network.
func Namespace(secret []byte, siteID, repositoryID, userID int64) string {
	prefix := fmt.Sprintf("v1-%s-%s-%s", strconv.FormatInt(siteID, 36), strconv.FormatInt(repositoryID, 36), strconv.FormatInt(userID, 36))
	signer := hmac.New(sha256.New, secret)
	_, _ = signer.Write([]byte(prefix))
	return prefix + "-" + hex.EncodeToString(signer.Sum(nil))
}

// ValidateNamespace verifies that a build-cache path was issued by this Cache.
func ValidateNamespace(secret []byte, namespace string) bool {
	parts := strings.Split(namespace, "-")
	if len(parts) != 5 || parts[0] != "v1" {
		return false
	}
	for _, value := range parts[1:4] {
		id, err := strconv.ParseInt(value, 36, 64)
		if err != nil || id <= 0 {
			return false
		}
	}
	provided, err := hex.DecodeString(parts[4])
	if err != nil || len(provided) != sha256.Size {
		return false
	}
	signer := hmac.New(sha256.New, secret)
	_, _ = signer.Write([]byte(strings.Join(parts[:4], "-")))
	return subtle.ConstantTimeCompare(provided, signer.Sum(nil)) == 1
}
