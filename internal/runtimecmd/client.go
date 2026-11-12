// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtimecmd

import (
	"context"
	"net"
	"net/http"

	"gitea.dev/codespace/internal/runtimeendpoint"
)

func runtimeHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", runtimeendpoint.ContainerSocketPath)
	}}}
}
