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

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"gitea.dev/codespace/internal/webide"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type webIDEImageResolverFunc func(context.Context, string) (resolvedWebIDEImage, error)

func (f webIDEImageResolverFunc) Resolve(ctx context.Context, image string) (resolvedWebIDEImage, error) {
	return f(ctx, image)
}

func TestSaveEnvironmentWebIDEReference(t *testing.T) {
	_, schema, _, err := parseWebIDEImageMetadata(`{"metadataVersion":1,"protocolVersion":1,"provider":{"id":"example","name":"Example IDE"},"optionsSchema":{"type":"object","properties":{"enabled":{"type":"boolean"}},"additionalProperties":false}}`)
	require.NoError(t, err)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		&nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "sysbox-runc", Labels: map[string]string{api.RuntimeIsolationLabel: api.IsolationSysbox}}, Handler: "sysbox-runc"},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "local-path"}, Provisioner: "rancher.io/local-path"},
	).Build()
	server := &AdminServer{Client: c, WebIDEResolver: webIDEImageResolverFunc(func(_ context.Context, image string) (resolvedWebIDEImage, error) {
		named, err := webide.ParseImageReference(image)
		if err != nil {
			return resolvedWebIDEImage{}, err
		}
		return resolvedWebIDEImage{Image: named.String(), Provider: "example", compiledSchema: schema}, nil
	})}
	for i, image := range []string{"team/ide", "registry.example/ide:1.2.3", "registry.example/ide@sha256:" + strings.Repeat("a", 64)} {
		t.Run(image, func(t *testing.T) {
			template := &api.EnvironmentTemplate{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("test-%d", i)}}
			spec := api.EnvironmentTemplateSpec{Runtime: testEnvironment()}
			spec.Runtime.DevContainer.WebIDE.Image = image
			spec.Runtime.DevContainer.WebIDE.Options.Raw = []byte(`{"enabled":true}`)
			require.NoError(t, server.saveEnvironment(t.Context(), template, adminEnvironmentWrite{Spec: spec}, true))
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(template), template))
			named, err := webide.ParseImageReference(image)
			require.NoError(t, err)
			require.Equal(t, named.String(), template.Spec.Runtime.DevContainer.WebIDE.Image)
			spec = *template.Spec.DeepCopy()
			spec.Description = "Updated description"
			serverWithoutRegistry := &AdminServer{Client: c}
			require.NoError(t, serverWithoutRegistry.saveEnvironment(t.Context(), template, adminEnvironmentWrite{Spec: spec}, false))
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(template), template))
			require.Equal(t, spec.Runtime.DevContainer.WebIDE, template.Spec.Runtime.DevContainer.WebIDE)
		})
	}
}

func TestWebIDERegistryCredentials(t *testing.T) {
	for _, host := range []string{"docker.io", "index.docker.io", "https://index.docker.io/v1/", "registry-1.docker.io"} {
		t.Run(host, func(t *testing.T) {
			data, err := json.Marshal(map[string]any{"auths": map[string]any{host: map[string]string{"username": "reader", "password": "secret"}}})
			require.NoError(t, err)
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "registry", Namespace: "system"}, Type: corev1.SecretTypeDockerConfigJson, Data: map[string][]byte{corev1.DockerConfigJsonKey: data}}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(secret).Build()
			resolver := &registryWebIDEImageResolver{client: c, namespace: "system", imagePullSecrets: []string{"registry"}}
			credentials, err := resolver.registryCredentials(t.Context())
			require.NoError(t, err)
			require.Equal(t, auth.Credential{Username: "reader", Password: "secret"}, credentials[normalizeRegistryHost("registry-1.docker.io")])
		})
	}
}

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
	for _, tag := range []string{"latest", "1.2.3", index.Digest.String()} {
		require.NoError(t, store.Tag(t.Context(), index, tag))
	}
	for _, test := range []struct{ image, reference string }{
		{"registry.example/example/ide:latest", "latest"},
		{"registry.example/example/ide:1.2.3", "1.2.3"},
		{"registry.example/example/ide@" + index.Digest.String(), index.Digest.String()},
	} {
		t.Run(test.reference, func(t *testing.T) {
			resolved, err := resolveWebIDEImage(t.Context(), store, test.image, test.reference)
			require.NoError(t, err)
			require.Equal(t, test.image, resolved.Image)
			require.Equal(t, "example", resolved.Provider)
			require.Equal(t, "1.2.3", resolved.Version)
			require.Equal(t, []string{"linux/amd64", "linux/arm64"}, resolved.Platforms)
			configuration, err := resolved.configuration(apiextensionsv1.JSON{Raw: []byte(`{"enabled":false}`)})
			require.NoError(t, err)
			require.Equal(t, test.image, configuration.Image)
			require.JSONEq(t, `{"enabled":false}`, string(configuration.Options.Raw))
		})
	}
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
	resolved, err := resolveWebIDEImage(t.Context(), repository, host+"/example/ide:1.2.3", "1.2.3")
	require.NoError(t, err)
	require.Equal(t, host+"/example/ide:1.2.3", resolved.Image)
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
