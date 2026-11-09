// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

func TestParseWebIDEImageMetadata(t *testing.T) {
	raw := `{
		"metadataVersion":1,
		"protocolVersion":1,
		"provider":{"id":"example","name":"Example IDE"},
		"optionsSchema":{
			"$schema":"https://json-schema.org/draft/2020-12/schema",
			"type":"object",
			"properties":{"theme":{"type":"string","default":"dark"},"nested":{"type":"object","properties":{"enabled":{"type":"boolean","default":true}}}},
			"additionalProperties":false
		}
	}`
	metadata, schema, defaults, err := parseWebIDEImageMetadata(raw)
	require.NoError(t, err)
	require.Equal(t, "example", metadata.Provider.ID)
	require.JSONEq(t, `{"theme":"dark","nested":{"enabled":true}}`, string(defaults))

	resolved := resolvedWebIDEImage{Image: "registry.example/ide@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Provider: metadata.Provider.ID, compiledSchema: schema}
	configuration, err := resolved.configuration(apiextensionsv1.JSON{Raw: []byte(`{"theme":"light","nested":{"enabled":false}}`)})
	require.NoError(t, err)
	require.Equal(t, "example", string(configuration.Provider))
	require.True(t, json.Valid(configuration.Options.Raw))

	_, err = resolved.configuration(apiextensionsv1.JSON{Raw: []byte(`{"unknown":true}`)})
	require.ErrorContains(t, err, "do not match")
}

func TestResolveMultiPlatformWebIDEImage(t *testing.T) {
	store := memory.New()
	metadata := `{"metadataVersion":1,"protocolVersion":1,"provider":{"id":"example","name":"Example IDE"},"optionsSchema":{"type":"object","properties":{"enabled":{"type":"boolean","default":true}},"additionalProperties":false}}`
	manifests := make([]ocispec.Descriptor, 0, 2)
	for _, architecture := range []string{"amd64", "arm64"} {
		configuration := ocispec.Image{
			Platform: ocispec.Platform{OS: "linux", Architecture: architecture},
			Config: ocispec.ImageConfig{Labels: map[string]string{
				webIDEImageMetadataLabel:   metadata,
				ocispec.AnnotationVersion:  "1.2.3",
				ocispec.AnnotationSource:   "https://example.test/provider",
				ocispec.AnnotationRevision: "revision-1",
			}},
		}
		configurationDescriptor := pushWebIDEJSON(t, store, ocispec.MediaTypeImageConfig, configuration)
		manifestDescriptor := pushWebIDEJSON(t, store, ocispec.MediaTypeImageManifest, ocispec.Manifest{
			Versioned: specs.Versioned{SchemaVersion: 2}, Config: configurationDescriptor,
		})
		manifestDescriptor.Platform = &ocispec.Platform{OS: "linux", Architecture: architecture}
		manifests = append(manifests, manifestDescriptor)
	}
	index := pushWebIDEJSON(t, store, ocispec.MediaTypeImageIndex, ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: manifests,
	})
	require.NoError(t, store.Tag(t.Context(), index, "main"))

	resolved, err := resolveWebIDEImage(t.Context(), store, "registry.example/example/ide", "main")
	require.NoError(t, err)
	require.Equal(t, "registry.example/example/ide@"+index.Digest.String(), resolved.Image)
	require.Equal(t, "example", resolved.Provider)
	require.Equal(t, []string{"linux/amd64", "linux/arm64"}, resolved.Platforms)
}

func TestResolveAuthenticatedRegistryWebIDEImage(t *testing.T) {
	metadata := `{"metadataVersion":1,"protocolVersion":1,"provider":{"id":"example","name":"Example IDE"},"optionsSchema":{"type":"object","additionalProperties":false}}`
	configuration := ocispec.Image{
		Platform: ocispec.Platform{OS: "linux", Architecture: "amd64"},
		Config: ocispec.ImageConfig{Labels: map[string]string{
			webIDEImageMetadataLabel:   metadata,
			ocispec.AnnotationVersion:  "1.2.3",
			ocispec.AnnotationSource:   "https://example.test/provider",
			ocispec.AnnotationRevision: "revision-1",
		}},
	}
	configurationData, err := json.Marshal(configuration)
	require.NoError(t, err)
	configurationDescriptor := content.NewDescriptorFromBytes(ocispec.MediaTypeImageConfig, configurationData)
	manifestData, err := json.Marshal(ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2}, Config: configurationDescriptor,
	})
	require.NoError(t, err)
	manifestDescriptor := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, manifestData)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != "reader" || password != "secret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="web-ide-test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var descriptor ocispec.Descriptor
		var data []byte
		switch {
		case strings.Contains(r.URL.Path, "/manifests/"):
			descriptor, data = manifestDescriptor, manifestData
		case strings.HasSuffix(r.URL.Path, "/blobs/"+configurationDescriptor.Digest.String()):
			descriptor, data = configurationDescriptor, configurationData
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", descriptor.MediaType)
		w.Header().Set("Docker-Content-Digest", descriptor.Digest.String())
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		if r.Method != http.MethodHead {
			_, _ = w.Write(data)
		}
	}))
	t.Cleanup(server.Close)

	host := strings.TrimPrefix(server.URL, "http://")
	repository, err := remote.NewRepository(host + "/example/ide")
	require.NoError(t, err)
	repository.PlainHTTP = true
	repository.Client = &auth.Client{
		Client: server.Client(),
		Credential: func(context.Context, string) (auth.Credential, error) {
			return auth.Credential{Username: "reader", Password: "secret"}, nil
		},
	}
	resolved, err := resolveWebIDEImage(t.Context(), repository, host+"/example/ide", "main")
	require.NoError(t, err)
	require.Equal(t, host+"/example/ide@"+manifestDescriptor.Digest.String(), resolved.Image)
}

func pushWebIDEJSON(t *testing.T, store *memory.Store, mediaType string, value any) ocispec.Descriptor {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	descriptor := content.NewDescriptorFromBytes(mediaType, data)
	require.NoError(t, store.Push(context.Background(), descriptor, bytes.NewReader(data)))
	return descriptor
}

func TestWebIDEImageMetadataRejectsRemoteReferences(t *testing.T) {
	_, _, _, err := parseWebIDEImageMetadata(`{
		"metadataVersion":1,"protocolVersion":1,"provider":{"id":"example","name":"Example"},
		"optionsSchema":{"type":"object","properties":{"value":{"$ref":"https://example.com/schema.json"}}}
	}`)
	require.ErrorContains(t, err, "references must stay within")
}
