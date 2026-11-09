// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"gitea.dev/codespace/internal/devcontainerruntime"
	"gitea.dev/codespace/internal/webide"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	webIDEImageMetadataLabel = "dev.gitea.codespace.web-ide.metadata"
	webIDEMetadataVersion    = 1
	maxWebIDEMetadataBytes   = 32 * 1024
	maxWebIDEContentBytes    = 4 * 1024 * 1024
	dockerManifestMediaType  = "application/vnd.docker.distribution.manifest.v2+json"
	dockerIndexMediaType     = "application/vnd.docker.distribution.manifest.list.v2+json"
)

var webIDEImageSuggestions = []string{
	"ghcr.io/explodingdragon/gitea-codespace-web-ide-vscode:main",
	"ghcr.io/explodingdragon/gitea-codespace-web-ide-dsh:main",
}

type webIDEImageMetadata struct {
	MetadataVersion int `json:"metadataVersion"`
	ProtocolVersion int `json:"protocolVersion"`
	Provider        struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"provider"`
	OptionsSchema json.RawMessage `json:"optionsSchema"`
}

// resolvedWebIDEImage is safe to return through the administration API. It
// contains no registry credentials and its Image is always digest-pinned.
type resolvedWebIDEImage struct {
	Image           string          `json:"image"`
	Provider        string          `json:"provider"`
	ProviderName    string          `json:"providerName"`
	Version         string          `json:"version"`
	ProtocolVersion int             `json:"protocolVersion"`
	Platforms       []string        `json:"platforms"`
	OptionDefaults  json.RawMessage `json:"optionDefaults"`
	compiledSchema  *jsonschema.Schema
}

func (r resolvedWebIDEImage) configuration(options apiextensionsv1.JSON) (devcontainerruntime.WebIDEConfiguration, error) {
	value := any(map[string]any{})
	if len(bytes.TrimSpace(options.Raw)) != 0 {
		if len(options.Raw) > 16*1024 || json.Unmarshal(options.Raw, &value) != nil {
			return devcontainerruntime.WebIDEConfiguration{}, fmt.Errorf("web IDE options must be a JSON object no larger than 16 KiB")
		}
		if _, ok := value.(map[string]any); !ok {
			return devcontainerruntime.WebIDEConfiguration{}, fmt.Errorf("web IDE options must be a JSON object no larger than 16 KiB")
		}
	}
	if err := r.compiledSchema.Validate(value); err != nil {
		return devcontainerruntime.WebIDEConfiguration{}, fmt.Errorf("web IDE options do not match the image schema: %w", err)
	}
	normalized, err := json.Marshal(value)
	if err != nil {
		return devcontainerruntime.WebIDEConfiguration{}, err
	}
	return devcontainerruntime.WebIDEConfiguration{
		Image: r.Image, Provider: devcontainerruntime.WebIDEProvider(r.Provider), Options: apiextensionsv1.JSON{Raw: normalized},
	}, nil
}

type webIDEImageResolver interface {
	Resolve(context.Context, string) (resolvedWebIDEImage, error)
}

type registryWebIDEImageResolver struct {
	client           client.Reader
	namespace        string
	imagePullSecrets []string
}

func newRegistryWebIDEImageResolver(reader client.Reader, namespace string, imagePullSecrets []string) webIDEImageResolver {
	return &registryWebIDEImageResolver{client: reader, namespace: namespace, imagePullSecrets: imagePullSecrets}
}

func (r *registryWebIDEImageResolver) Resolve(ctx context.Context, image string) (resolvedWebIDEImage, error) {
	image = strings.TrimSpace(image)
	if len(image) == 0 || len(image) > 4096 {
		return resolvedWebIDEImage{}, fmt.Errorf("web IDE image is required and must be at most 4096 characters")
	}
	parsed, err := registry.ParseReference(image)
	if err != nil || parsed.Reference == "" {
		return resolvedWebIDEImage{}, fmt.Errorf("web IDE image must contain a registry, repository, and explicit tag or digest")
	}
	if parsed.Registry != "localhost" && !strings.ContainsAny(parsed.Registry, ".:") {
		return resolvedWebIDEImage{}, fmt.Errorf("web IDE image must use an explicit registry hostname")
	}
	if parsed.ValidateReferenceAsTag() != nil && parsed.ValidateReferenceAsDigest() != nil {
		return resolvedWebIDEImage{}, fmt.Errorf("web IDE image must contain an explicit tag or sha256 digest")
	}
	if parsed.ValidateReferenceAsDigest() == nil {
		value, _ := parsed.Digest()
		if value.Algorithm() != digest.SHA256 {
			return resolvedWebIDEImage{}, fmt.Errorf("web IDE image digest must use sha256")
		}
	}
	repository, err := remote.NewRepository(parsed.Registry + "/" + parsed.Repository)
	if err != nil {
		return resolvedWebIDEImage{}, fmt.Errorf("open Web IDE image repository: %w", err)
	}
	credentials, err := r.registryCredentials(ctx)
	if err != nil {
		return resolvedWebIDEImage{}, err
	}
	repository.Client = &auth.Client{
		Client: retry.DefaultClient,
		Cache:  auth.NewCache(),
		Credential: func(_ context.Context, host string) (auth.Credential, error) {
			return credentials[normalizeRegistryHost(host)], nil
		},
	}
	return resolveWebIDEImage(ctx, repository, parsed.Registry+"/"+parsed.Repository, parsed.Reference)
}

func (r *registryWebIDEImageResolver) registryCredentials(ctx context.Context) (map[string]auth.Credential, error) {
	credentials := make(map[string]auth.Credential)
	for _, name := range r.imagePullSecrets {
		var secret corev1.Secret
		if err := r.client.Get(ctx, client.ObjectKey{Namespace: r.namespace, Name: name}, &secret); err != nil {
			return nil, fmt.Errorf("read image pull Secret %q: %w", name, err)
		}
		var entries map[string]dockerAuthEntry
		switch secret.Type {
		case corev1.SecretTypeDockerConfigJson:
			var configuration struct {
				Auths map[string]dockerAuthEntry `json:"auths"`
			}
			if err := json.Unmarshal(secret.Data[corev1.DockerConfigJsonKey], &configuration); err != nil {
				return nil, fmt.Errorf("decode image pull Secret %q: %w", name, err)
			}
			entries = configuration.Auths
		case corev1.SecretTypeDockercfg:
			if err := json.Unmarshal(secret.Data[corev1.DockerConfigKey], &entries); err != nil {
				return nil, fmt.Errorf("decode image pull Secret %q: %w", name, err)
			}
		default:
			return nil, fmt.Errorf("image pull Secret %q has unsupported type %q", name, secret.Type)
		}
		for host, entry := range entries {
			credential, err := entry.credential()
			if err != nil {
				return nil, fmt.Errorf("decode image pull Secret %q registry %q: %w", name, host, err)
			}
			credentials[normalizeRegistryHost(host)] = credential
		}
	}
	return credentials, nil
}

type dockerAuthEntry struct {
	Auth          string `json:"auth"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	IdentityToken string `json:"identitytoken"`
	RegistryToken string `json:"registrytoken"`
}

func (e dockerAuthEntry) credential() (auth.Credential, error) {
	if e.Username == "" && e.Password == "" && e.Auth != "" {
		decoded, err := base64.StdEncoding.DecodeString(e.Auth)
		if err != nil {
			return auth.Credential{}, err
		}
		var found bool
		e.Username, e.Password, found = strings.Cut(string(decoded), ":")
		if !found {
			return auth.Credential{}, fmt.Errorf("auth value does not contain a username and password")
		}
	}
	return auth.Credential{Username: e.Username, Password: e.Password, RefreshToken: e.IdentityToken, AccessToken: e.RegistryToken}, nil
}

func normalizeRegistryHost(value string) string {
	if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
		value = parsed.Host
	} else {
		value = strings.Split(strings.TrimPrefix(strings.TrimPrefix(value, "https://"), "http://"), "/")[0]
	}
	if value == "index.docker.io" {
		return "registry-1.docker.io"
	}
	return strings.ToLower(value)
}

type webIDEContent interface {
	content.Resolver
	content.Fetcher
}

type resolvedWebIDEPlatform struct {
	metadata webIDEImageMetadata
	version  string
	source   string
	revision string
	platform string
	schema   *jsonschema.Schema
	defaults json.RawMessage
}

func resolveWebIDEImage(ctx context.Context, source webIDEContent, repository, imageReference string) (resolvedWebIDEImage, error) {
	descriptor, err := source.Resolve(ctx, imageReference)
	if err != nil {
		return resolvedWebIDEImage{}, fmt.Errorf("resolve Web IDE image: %w", err)
	}
	if descriptor.Digest.Algorithm() != digest.SHA256 {
		return resolvedWebIDEImage{}, fmt.Errorf("web IDE image manifest digest must use sha256")
	}
	var platforms []resolvedWebIDEPlatform
	switch descriptor.MediaType {
	case ocispec.MediaTypeImageIndex, dockerIndexMediaType:
		var index ocispec.Index
		if err := fetchWebIDEJSON(ctx, source, descriptor, &index); err != nil {
			return resolvedWebIDEImage{}, fmt.Errorf("read Web IDE image index: %w", err)
		}
		for _, manifest := range index.Manifests {
			if manifest.Platform == nil || manifest.Platform.OS != "linux" || (manifest.Platform.Architecture != "amd64" && manifest.Platform.Architecture != "arm64") {
				continue
			}
			resolved, err := resolveWebIDEManifest(ctx, source, manifest, manifest.Platform.OS, manifest.Platform.Architecture)
			if err != nil {
				return resolvedWebIDEImage{}, err
			}
			platforms = append(platforms, resolved)
		}
	case ocispec.MediaTypeImageManifest, dockerManifestMediaType:
		resolved, err := resolveWebIDEManifest(ctx, source, descriptor, "", "")
		if err != nil {
			return resolvedWebIDEImage{}, err
		}
		platforms = append(platforms, resolved)
	default:
		return resolvedWebIDEImage{}, fmt.Errorf("web IDE image uses unsupported manifest media type %q", descriptor.MediaType)
	}
	if len(platforms) == 0 {
		return resolvedWebIDEImage{}, fmt.Errorf("web IDE image has no linux/amd64 or linux/arm64 manifest")
	}
	sort.Slice(platforms, func(i, j int) bool { return platforms[i].platform < platforms[j].platform })
	first := platforms[0]
	canonicalMetadata, _ := json.Marshal(first.metadata)
	result := resolvedWebIDEImage{
		Image: repository + "@" + descriptor.Digest.String(), Provider: first.metadata.Provider.ID, ProviderName: first.metadata.Provider.Name,
		Version: first.version, ProtocolVersion: first.metadata.ProtocolVersion,
		OptionDefaults: first.defaults, compiledSchema: first.schema,
	}
	seenPlatforms := make(map[string]struct{}, len(platforms))
	for _, platform := range platforms {
		currentMetadata, _ := json.Marshal(platform.metadata)
		if !bytes.Equal(canonicalMetadata, currentMetadata) || platform.version != first.version || platform.source != first.source || platform.revision != first.revision {
			return resolvedWebIDEImage{}, fmt.Errorf("web IDE image metadata differs between supported platforms")
		}
		if _, exists := seenPlatforms[platform.platform]; exists {
			return resolvedWebIDEImage{}, fmt.Errorf("web IDE image contains duplicate platform %s", platform.platform)
		}
		seenPlatforms[platform.platform] = struct{}{}
		result.Platforms = append(result.Platforms, platform.platform)
	}
	return result, nil
}

func resolveWebIDEManifest(ctx context.Context, source content.Fetcher, descriptor ocispec.Descriptor, expectedOS, expectedArchitecture string) (resolvedWebIDEPlatform, error) {
	if descriptor.MediaType != ocispec.MediaTypeImageManifest && descriptor.MediaType != dockerManifestMediaType {
		return resolvedWebIDEPlatform{}, fmt.Errorf("web IDE image uses unsupported platform manifest media type %q", descriptor.MediaType)
	}
	var manifest ocispec.Manifest
	if err := fetchWebIDEJSON(ctx, source, descriptor, &manifest); err != nil {
		return resolvedWebIDEPlatform{}, fmt.Errorf("read Web IDE platform manifest: %w", err)
	}
	var configuration ocispec.Image
	if err := fetchWebIDEJSON(ctx, source, manifest.Config, &configuration); err != nil {
		return resolvedWebIDEPlatform{}, fmt.Errorf("read Web IDE image configuration: %w", err)
	}
	if expectedOS != "" && (configuration.OS != expectedOS || configuration.Architecture != expectedArchitecture) {
		return resolvedWebIDEPlatform{}, fmt.Errorf("web IDE image index platform does not match its image configuration")
	}
	if configuration.OS != "linux" || (configuration.Architecture != "amd64" && configuration.Architecture != "arm64") {
		return resolvedWebIDEPlatform{}, fmt.Errorf("web IDE image platform %s/%s is unsupported", configuration.OS, configuration.Architecture)
	}
	metadata, schema, defaults, err := parseWebIDEImageMetadata(configuration.Config.Labels[webIDEImageMetadataLabel])
	if err != nil {
		return resolvedWebIDEPlatform{}, err
	}
	version := strings.TrimSpace(configuration.Config.Labels[ocispec.AnnotationVersion])
	if version == "" || len(version) > 128 || strings.ContainsAny(version, "\r\n\x00") {
		return resolvedWebIDEPlatform{}, fmt.Errorf("web IDE image label %q is required and must be at most 128 characters", ocispec.AnnotationVersion)
	}
	sourceURL := strings.TrimSpace(configuration.Config.Labels[ocispec.AnnotationSource])
	parsedSource, err := url.Parse(sourceURL)
	if err != nil || len(sourceURL) > 2048 || (parsedSource.Scheme != "https" && parsedSource.Scheme != "http") || parsedSource.Host == "" {
		return resolvedWebIDEPlatform{}, fmt.Errorf("web IDE image label %q must be an HTTP or HTTPS source URL", ocispec.AnnotationSource)
	}
	revision := strings.TrimSpace(configuration.Config.Labels[ocispec.AnnotationRevision])
	if revision == "" || len(revision) > 128 || strings.ContainsAny(revision, "\r\n\x00") {
		return resolvedWebIDEPlatform{}, fmt.Errorf("web IDE image label %q is required and must be at most 128 characters", ocispec.AnnotationRevision)
	}
	return resolvedWebIDEPlatform{metadata: metadata, schema: schema, defaults: defaults, version: version, source: sourceURL, revision: revision, platform: configuration.OS + "/" + configuration.Architecture}, nil
}

func fetchWebIDEJSON(ctx context.Context, source content.Fetcher, descriptor ocispec.Descriptor, target any) error {
	if descriptor.Size < 0 || descriptor.Size > maxWebIDEContentBytes {
		return fmt.Errorf("descriptor is larger than %d bytes", maxWebIDEContentBytes)
	}
	reader, err := source.Fetch(ctx, descriptor)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	data, err := content.ReadAll(reader, descriptor)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func parseWebIDEImageMetadata(raw string) (webIDEImageMetadata, *jsonschema.Schema, json.RawMessage, error) {
	if raw == "" || len(raw) > maxWebIDEMetadataBytes {
		return webIDEImageMetadata{}, nil, nil, fmt.Errorf("web IDE image label %q is required and must be no larger than 32 KiB", webIDEImageMetadataLabel)
	}
	var metadata webIDEImageMetadata
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		return metadata, nil, nil, fmt.Errorf("decode Web IDE image metadata: %w", err)
	}
	if metadata.MetadataVersion != webIDEMetadataVersion {
		return metadata, nil, nil, fmt.Errorf("web IDE image metadata version %d is unsupported", metadata.MetadataVersion)
	}
	if metadata.ProtocolVersion != webide.ProviderProtocolVersion {
		return metadata, nil, nil, fmt.Errorf("web IDE provider protocol %d is unsupported", metadata.ProtocolVersion)
	}
	if err := devcontainerruntime.ValidateWebIDEProvider(metadata.Provider.ID); err != nil {
		return metadata, nil, nil, err
	}
	metadata.Provider.Name = strings.TrimSpace(metadata.Provider.Name)
	if metadata.Provider.Name == "" || len(metadata.Provider.Name) > 64 || strings.ContainsAny(metadata.Provider.Name, "\r\n\x00") {
		return metadata, nil, nil, fmt.Errorf("web IDE provider display name is required and must be at most 64 characters")
	}
	var schemaDocument any
	if len(metadata.OptionsSchema) == 0 || len(metadata.OptionsSchema) > maxWebIDEMetadataBytes || json.Unmarshal(metadata.OptionsSchema, &schemaDocument) != nil {
		return metadata, nil, nil, fmt.Errorf("web IDE options schema must be a JSON object within the metadata size limit")
	}
	schemaObject, ok := schemaDocument.(map[string]any)
	if !ok || schemaObject["type"] != "object" {
		return metadata, nil, nil, fmt.Errorf("web IDE options schema root type must be object")
	}
	if err := validateLocalSchemaReferences(schemaDocument); err != nil {
		return metadata, nil, nil, err
	}
	normalizedSchema, _ := json.Marshal(schemaDocument)
	metadata.OptionsSchema = normalizedSchema
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	if err := compiler.AddResource("web-ide-options.json", schemaDocument); err != nil {
		return metadata, nil, nil, fmt.Errorf("load Web IDE options schema: %w", err)
	}
	compiled, err := compiler.Compile("web-ide-options.json")
	if err != nil {
		return metadata, nil, nil, fmt.Errorf("compile Web IDE options schema: %w", err)
	}
	defaultValues := schemaDefaults(schemaObject)
	if err := compiled.Validate(defaultValues); err != nil {
		return metadata, nil, nil, fmt.Errorf("web IDE option defaults do not match their schema: %w", err)
	}
	defaults, _ := json.Marshal(defaultValues)
	return metadata, compiled, defaults, nil
}

func validateLocalSchemaReferences(value any) error {
	switch current := value.(type) {
	case map[string]any:
		for key, nested := range current {
			if (key == "$ref" || key == "$dynamicRef") && !strings.HasPrefix(fmt.Sprint(nested), "#") {
				return fmt.Errorf("web IDE options schema references must stay within the image metadata")
			}
			if err := validateLocalSchemaReferences(nested); err != nil {
				return err
			}
		}
	case []any:
		for _, nested := range current {
			if err := validateLocalSchemaReferences(nested); err != nil {
				return err
			}
		}
	}
	return nil
}

func schemaDefaults(schema map[string]any) map[string]any {
	if value, ok := schema["default"].(map[string]any); ok {
		return value
	}
	defaults := make(map[string]any)
	properties, _ := schema["properties"].(map[string]any)
	for name, raw := range properties {
		property, _ := raw.(map[string]any)
		if value, ok := property["default"]; ok {
			defaults[name] = value
			continue
		}
		if property["type"] == "object" {
			nested := schemaDefaults(property)
			if len(nested) != 0 {
				defaults[name] = nested
			}
		}
	}
	return defaults
}
