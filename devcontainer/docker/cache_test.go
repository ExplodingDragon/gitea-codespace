// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"gitea.dev/codespace/devcontainer"
	"github.com/stretchr/testify/require"
)

func TestUseCachedImage(t *testing.T) {
	for _, test := range []struct {
		name          string
		localImage    string
		inspectStatus int
		pullStatus    int
		pulls         int32
		cached        bool
		wantError     string
		message       string
	}{
		{name: "local", inspectStatus: http.StatusOK, localImage: `{"Os":"linux","Architecture":"amd64","Config":{}}`, pullStatus: http.StatusOK, cached: true, message: "Using features image cache (local)"},
		{name: "remote", pullStatus: http.StatusOK, pulls: 1, cached: true, message: "Using features image cache (pulled in"},
		{name: "other platform", inspectStatus: http.StatusOK, localImage: `{"Os":"linux","Architecture":"arm64","Config":{}}`, pullStatus: http.StatusOK, pulls: 1, cached: true, message: "Using features image cache (pulled in"},
		{name: "missing", pullStatus: http.StatusNotFound, pulls: 1, message: "features image cache not found"},
		{name: "unavailable", pullStatus: http.StatusForbidden, pulls: 1, message: "features image cache unavailable"},
		{name: "inspect failure", inspectStatus: http.StatusInternalServerError, pullStatus: http.StatusOK, wantError: "local inspection failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var pulls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/_ping":
					w.Header().Set("OSType", "linux")
				case strings.HasSuffix(r.URL.Path, "/version"):
					_, _ = io.WriteString(w, `{"Os":"linux","Arch":"amd64","ApiVersion":"1.51"}`)
				case strings.HasSuffix(r.URL.Path, "/images/create"):
					pulls.Add(1)
					w.WriteHeader(test.pullStatus)
					_, _ = io.WriteString(w, `{"message":"cache registry response"}`)
				case strings.HasSuffix(r.URL.Path, "/images/registry.example.com/cache:features/json"):
					if test.inspectStatus == 0 {
						w.WriteHeader(http.StatusNotFound)
					} else {
						w.WriteHeader(test.inspectStatus)
					}
					if test.localImage != "" {
						_, _ = io.WriteString(w, test.localImage)
					} else {
						_, _ = io.WriteString(w, `{"message":"local inspection failed"}`)
					}
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			t.Setenv("DOCKER_HOST", "tcp://"+server.Listener.Addr().String())
			t.Setenv("DOCKER_API_VERSION", "1.51")
			t.Setenv("DOCKER_CONFIG", t.TempDir())
			t.Setenv("DOCKER_CONTEXT", "")
			var output bytes.Buffer
			engine, err := New(t.Context(), &output, &output)
			require.NoError(t, err)
			defer engine.Close()
			cached, err := engine.useCachedImage(t.Context(), "registry.example.com/cache:features", "features")
			if test.wantError != "" {
				require.ErrorContains(t, err, test.wantError)
			} else {
				require.NoError(t, err)
				require.Contains(t, output.String(), test.message)
			}
			require.Equal(t, test.cached, cached)
			require.Equal(t, test.pulls, pulls.Load())
		})
	}
}

func TestBuildImageOptionsUseBuildx(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "docker"), []byte(`#!/bin/sh
printf '%s\0' "$DOCKER_HOST" "$DOCKER_CONFIG" "$@" > "$BUILD_CAPTURE"
exit "$BUILD_EXIT_CODE"
`), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_HOST", "unix:///test/docker.sock")
	t.Setenv("DOCKER_CONFIG", filepath.Join(directory, "config"))
	t.Setenv("BUILD_CAPTURE", filepath.Join(directory, "arguments"))
	cache := devcontainer.CacheOptions{BuildRegistry: "https://registry.example.com/codespace", BuildScope: "scope"}
	for _, test := range []struct {
		name     string
		cache    devcontainer.CacheOptions
		exitCode int
	}{
		{name: "registry cache", cache: cache},
		{name: "build failure", cache: cache, exitCode: 17},
		{name: "user cache only"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("BUILD_EXIT_CODE", strconv.Itoa(test.exitCode))
			engine := &Engine{stdout: io.Discard, stderr: io.Discard}
			value := "with spaces"
			options := []string{"--label", "example.label=with spaces", "--cache-to=type=local,dest=/cache/export", "--no-cache"}
			err := engine.buildImage(t.Context(), directory, "Dockerfile", "example:local", "development",
				map[string]*string{"EXAMPLE": &value}, []string{"example:cache"}, options, test.cache, "repository",
				map[string]string{"HTTPS_PROXY": "http://proxy.example:3128"})
			if test.exitCode == 0 {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != test.exitCode {
					t.Fatalf("build error = %v", err)
				}
			}
			content, err := os.ReadFile(os.Getenv("BUILD_CAPTURE"))
			if err != nil {
				t.Fatal(err)
			}
			arguments := strings.Split(strings.TrimSuffix(string(content), "\x00"), "\x00")
			want := []string{"unix:///test/docker.sock", filepath.Join(directory, "config"),
				"buildx", "build", "--load", "--progress=plain", "--file", filepath.Join(directory, "Dockerfile"), "--tag", "example:local",
				"--target", "development", "--build-arg", "EXAMPLE=with spaces", "--build-arg", "HTTPS_PROXY=http://proxy.example:3128",
				"--cache-from", "example:cache"}
			if source, destination := registryBuildCache(test.cache, "repository", ""); source != "" {
				want = append(want, "--cache-from", source, "--cache-to", destination)
			}
			want = append(want, options...)
			want = append(want, directory)
			if !slices.Equal(arguments, want) {
				t.Fatalf("Buildx arguments = %#v, want %#v", arguments, want)
			}
		})
	}
}
