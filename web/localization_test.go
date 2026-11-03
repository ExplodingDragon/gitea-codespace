// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package web

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLocalize(t *testing.T) {
	require.Equal(t, "请求内容无效。", Localize("zh-CN", "errors.invalid_request", nil))
	require.Equal(t, "リクエストは拒否されました。", Localize("ja-JP", "errors.request_forbidden", nil))
	require.Equal(t, "Request failed (503).", Localize("unknown", "errors.request_failed", map[string]string{"status": "503"}))
}
