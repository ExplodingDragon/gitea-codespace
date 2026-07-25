// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
)

func newGiteaManagerServiceServer(t *testing.T, service codespacev1connect.ManagerServiceHandler) *httptest.Server {
	t.Helper()

	_, handler := codespacev1connect.NewManagerServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(managerServicePath+"/", http.StripPrefix(managerServicePath, handler))
	return httptest.NewServer(mux)
}
