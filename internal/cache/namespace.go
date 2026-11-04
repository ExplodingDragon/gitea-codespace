// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cache

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Namespace returns a stable, unguessable registry path scoped to one Gitea
// site, repository, and user. The signed cache credential independently binds
// and authorizes this namespace on every registry request.
func Namespace(secret []byte, siteUID string, repositoryID, userID int64) string {
	scope := sha256.Sum256(fmt.Appendf(nil, "%s/%d/%d", siteUID, repositoryID, userID))
	prefix := "v1-" + hex.EncodeToString(scope[:])
	signer := hmac.New(sha256.New, secret)
	_, _ = signer.Write([]byte(prefix))
	return prefix + "-" + hex.EncodeToString(signer.Sum(nil))
}
