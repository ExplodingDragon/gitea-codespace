// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
)

type gatewayWorkspaceTCPBackend interface {
	OpenWorkspaceTCP(ctx context.Context, instanceName string, port uint32) (net.Conn, error)
}

type gatewayWorkspaceIDE struct {
	state   gatewayWorkspaceTargetStore
	backend gatewayWorkspaceTCPBackend
}

func newGatewayWorkspaceIDE(state gatewayWorkspaceTargetStore, backend gatewayWorkspaceTCPBackend) *gatewayWorkspaceIDE {
	return &gatewayWorkspaceIDE{state: state, backend: backend}
}

func (ide *gatewayWorkspaceIDE) ServeHTTP(writer http.ResponseWriter, request *http.Request, codespaceUUID, upstreamPath string, proxyContext gatewayProxyRequestContext) {
	if ide == nil || ide.state == nil || ide.backend == nil {
		writeGatewayError(writer, request, http.StatusServiceUnavailable, "Workspace is starting", "Codespace Gateway cannot open the Web IDE yet. Try again shortly.", "gateway workspace IDE is not ready")
		return
	}
	target, ok, err := ide.state.LoadGatewayWorkspaceTarget(codespaceUUID)
	if err != nil || !ok {
		writeGatewayError(writer, request, http.StatusServiceUnavailable, "Workspace is not ready", "The Web IDE is not available for this codespace yet.", "gateway workspace IDE target is unavailable")
		return
	}

	upstreamHost := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(target.editorPort)))
	upstreamURL := &url.URL{Scheme: "http", Host: upstreamHost}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return ide.backend.OpenWorkspaceTCP(ctx, target.instanceName, target.editorPort)
		},
	}
	defer transport.CloseIdleConnections()
	proxy := httputil.NewSingleHostReverseProxy(upstreamURL)
	proxy.Transport = transport
	proxy.Director = func(upstream *http.Request) {
		upstream.URL.Scheme = upstreamURL.Scheme
		upstream.URL.Host = upstreamURL.Host
		upstream.URL.Path = upstreamPath
		upstream.URL.RawPath = ""
		upstream.Host = upstreamURL.Host
		prepareGatewayProxyRequest(upstream, proxyContext)
	}
	proxy.ModifyResponse = func(response *http.Response) error {
		normalizeGatewayProxyResponse(response.Header, gatewayProxyResponseContext{
			externalScheme: proxyContext.externalScheme,
			externalHost:   proxyContext.externalHost,
			upstreamScheme: upstreamURL.Scheme,
			upstreamHost:   upstreamURL.Host,
		})
		return nil
	}
	proxy.ErrorHandler = func(writer http.ResponseWriter, request *http.Request, err error) {
		log.Printf("gateway workspace IDE %s: %v", codespaceUUID, err)
		writeGatewayError(writer, request, http.StatusBadGateway, "Workspace is unavailable", "Codespace Gateway could not connect to the Web IDE. It may still be starting.", "gateway workspace IDE upstream unavailable")
	}
	proxy.ServeHTTP(writer, request)
}
