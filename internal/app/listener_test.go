// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestGatewayHTTPServerUsesFixedHeaderLimits(t *testing.T) {
	t.Parallel()

	server := newGatewayHTTPServer(http.NewServeMux())
	if server.ReadHeaderTimeout != gatewayHTTPReadHeaderTime {
		t.Fatalf("read header timeout = %s", server.ReadHeaderTimeout)
	}
	if server.MaxHeaderBytes != gatewayHTTPMaxHeaderBytes {
		t.Fatalf("max header bytes = %d", server.MaxHeaderBytes)
	}
}

func TestGatewayHTTPServerRejectsOversizedHeader(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errorChannel := make(chan error, 1)
	go serveHTTP(ctx, errorChannel, "gateway http", newGatewayHTTPServer(http.NewServeMux()), listener)

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		cancel()
		_ = listener.Close()
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if _, err := fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: example.test\r\nX-Large: %s\r\n\r\n", strings.Repeat("a", gatewayHTTPMaxHeaderBytes*2)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("status = %d", response.StatusCode)
	}
	cancel()
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	assertNoListenerError(t, errorChannel)
}

func TestServeHTTPReportsUnexpectedListenerClose(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	errorChannel := make(chan error, 1)
	go serveHTTP(context.Background(), errorChannel, "runtime api", &http.Server{Handler: http.NewServeMux()}, listener)

	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	err = waitListenerError(t, errorChannel)
	if !strings.Contains(err.Error(), "runtime api listener stopped unexpectedly") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestServeHTTPIgnoresExpectedListenerClose(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errorChannel := make(chan error, 1)
	go serveHTTP(ctx, errorChannel, "runtime api", &http.Server{Handler: http.NewServeMux()}, listener)

	cancel()
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	assertNoListenerError(t, errorChannel)
}

func TestServeSSHReportsUnexpectedListenerClose(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	errorChannel := make(chan error, 1)
	go serveSSH(context.Background(), errorChannel, listener)

	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	err = waitListenerError(t, errorChannel)
	if !strings.Contains(err.Error(), "gateway ssh listener") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestServeSSHIgnoresExpectedListenerClose(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errorChannel := make(chan error, 1)
	go serveSSH(ctx, errorChannel, listener)

	cancel()
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	assertNoListenerError(t, errorChannel)
}

func waitListenerError(t *testing.T, errorChannel <-chan error) error {
	t.Helper()
	select {
	case err := <-errorChannel:
		return err
	case <-time.After(time.Second):
		t.Fatalf("listener error was not reported")
		return nil
	}
}

func assertNoListenerError(t *testing.T, errorChannel <-chan error) {
	t.Helper()
	select {
	case err := <-errorChannel:
		t.Fatalf("unexpected listener error: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
}
