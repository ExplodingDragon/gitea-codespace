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
	"testing"

	"gitea.dev/codespace/devcontainer"
	"github.com/stretchr/testify/require"
)

func TestImageCacheReportsAvailability(t *testing.T) {
	for _, test := range []struct {
		status  int
		message string
	}{
		{http.StatusOK, "[INFO] [build] Using features image cache"},
		{http.StatusNotFound, "[INFO] [build] features image cache not found; building locally"},
		{http.StatusForbidden, "[WARN] [build] features image cache unavailable; building locally:"},
	} {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/_ping":
					w.Header().Set("OSType", "linux")
				case strings.HasSuffix(r.URL.Path, "/version"):
					_, _ = io.WriteString(w, `{"Os":"linux","Arch":"amd64","ApiVersion":"1.51"}`)
				case strings.HasSuffix(r.URL.Path, "/images/create"):
					w.WriteHeader(test.status)
					_, _ = io.WriteString(w, `{"message":"cache registry response"}`)
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
			require.Equal(t, test.status == http.StatusOK, engine.useCachedImage(t.Context(), "registry.example.com/cache:features", "features"))
			require.Contains(t, output.String(), "[INFO] [build] Checking features image cache\n"+test.message)
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
			if source, destination := registryBuildCache(test.cache, "repository"); source != "" {
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
