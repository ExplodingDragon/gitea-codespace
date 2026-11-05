// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/Masterminds/semver/v3"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"

	"gitea.dev/codespace/devcontainer"
)

const (
	devContainerFeatureMediaType = "application/vnd.devcontainers"
)

type resolvedFeature struct {
	reference        string
	references       []string
	resolved         string
	digest           string
	directory        string
	options          map[string]string
	providedOptions  map[string]string
	metadata         featureMetadata
	lockable         bool
	hardDependencies []*resolvedFeature
	softDependencies []*resolvedFeature
}

type featureMetadata struct {
	ID                   string                     `json:"id"`
	Version              string                     `json:"version"`
	Name                 string                     `json:"name"`
	Description          string                     `json:"description"`
	DocumentationURL     string                     `json:"documentationURL"`
	LicenseURL           string                     `json:"licenseURL"`
	Keywords             []string                   `json:"keywords"`
	Deprecated           bool                       `json:"deprecated"`
	Options              map[string]featureOption   `json:"options"`
	DependsOn            map[string]json.RawMessage `json:"dependsOn"`
	InstallsAfter        []string                   `json:"installsAfter"`
	LegacyIDs            []string                   `json:"legacyIds"`
	Entrypoint           string                     `json:"entrypoint"`
	ContainerEnv         map[string]string          `json:"containerEnv"`
	Mounts               []devcontainer.Mount       `json:"mounts"`
	Init                 bool                       `json:"init"`
	Privileged           bool                       `json:"privileged"`
	CapAdd               []string                   `json:"capAdd"`
	SecurityOpt          []string                   `json:"securityOpt"`
	OnCreateCommand      devcontainer.Command       `json:"onCreateCommand"`
	UpdateContentCommand devcontainer.Command       `json:"updateContentCommand"`
	PostCreateCommand    devcontainer.Command       `json:"postCreateCommand"`
	PostStartCommand     devcontainer.Command       `json:"postStartCommand"`
	PostAttachCommand    devcontainer.Command       `json:"postAttachCommand"`
	Customizations       map[string]json.RawMessage `json:"customizations"`
}

type featureOption struct {
	Type        string          `json:"type"`
	Description string          `json:"description"`
	Default     json.RawMessage `json:"default"`
	Proposals   []string        `json:"proposals"`
	Enum        []string        `json:"enum"`
}

func (e *Engine) applyFeatures(ctx context.Context, baseImage string, resolved *devcontainer.ResolvedConfiguration, imageConfiguration devcontainer.Configuration, imageSources []devcontainer.Configuration, repositoryConfiguration devcontainer.Configuration) (string, map[string]string, error) {
	lockfile, lockfileExists, err := devcontainer.ReadLockfile(resolved.ConfigurationPath)
	if err != nil {
		return "", nil, err
	}
	if resolved.FrozenLockfile && !resolved.Synthetic && !lockfileExists {
		return "", nil, fmt.Errorf("dev container lockfile does not exist")
	}
	requestedFeatures := make(map[string]json.RawMessage, len(resolved.Features))
	for reference, options := range resolved.Features {
		enabled, normalized, err := normalizeFeatureRequest(options)
		if err != nil {
			return "", nil, fmt.Errorf("dev container feature %s: %w", reference, err)
		}
		if enabled {
			requestedFeatures[reference] = normalized
		}
	}
	buildContext, err := os.MkdirTemp("", "devcontainer-features-*")
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = os.RemoveAll(buildContext) }()
	var features []*resolvedFeature
	requests := map[string]*resolvedFeature{}
	directoryIndex := 0
	var resolveFeature func(string, json.RawMessage) (*resolvedFeature, error)
	resolveFeature = func(reference string, rawOptions json.RawMessage) (*resolvedFeature, error) {
		enabled, normalized, err := normalizeFeatureRequest(rawOptions)
		if err != nil {
			return nil, err
		}
		if !enabled {
			return nil, fmt.Errorf("dependency Feature %s cannot be disabled", reference)
		}
		requestKey := strings.ToLower(strings.TrimSpace(reference)) + "\x00" + string(normalized)
		if feature := requests[requestKey]; feature != nil {
			return feature, nil
		}
		directory := filepath.Join(buildContext, strconv.Itoa(directoryIndex))
		directoryIndex++
		feature, err := e.fetchFeature(ctx, reference, normalized, directory, lockfile.Features[strings.ToLower(reference)], resolved.Cache, resolved.ConfigurationDir, resolved.AllowedPathRoot)
		if err != nil {
			return nil, err
		}
		for _, existing := range features {
			if featuresEqual(existing, feature) {
				existing.references = append(existing.references, reference)
				requests[requestKey] = existing
				_ = os.RemoveAll(directory)
				return existing, nil
			}
		}
		feature.references = []string{reference}
		features = append(features, feature)
		requests[requestKey] = feature
		dependencies := make([]string, 0, len(feature.metadata.DependsOn))
		for dependency := range feature.metadata.DependsOn {
			dependencies = append(dependencies, dependency)
		}
		sort.Strings(dependencies)
		for _, dependency := range dependencies {
			options := feature.metadata.DependsOn[dependency]
			if requestedOptions, explicitlyRequested := requestedFeatures[dependency]; explicitlyRequested {
				options = requestedOptions
			}
			resolvedDependency, err := resolveFeature(dependency, options)
			if err != nil {
				return nil, fmt.Errorf("resolve dependency of Feature %s: %w", reference, err)
			}
			feature.hardDependencies = append(feature.hardDependencies, resolvedDependency)
		}
		return feature, nil
	}
	references := make([]string, 0, len(requestedFeatures))
	for reference := range requestedFeatures {
		references = append(references, reference)
	}
	sort.Strings(references)
	for _, reference := range references {
		if _, err := resolveFeature(reference, requestedFeatures[reference]); err != nil {
			return "", nil, err
		}
	}
	for _, feature := range features {
		for _, requested := range feature.metadata.InstallsAfter {
			feature.softDependencies = append(feature.softDependencies, findFeatureMatches(features, requested)...)
		}
	}
	orderedFeatures, err := orderFeatures(features, resolved.OverrideFeatureInstallOrder)
	if err != nil {
		return "", nil, err
	}
	for _, feature := range orderedFeatures {
		_, _ = fmt.Fprintf(e.stdout, "Dev Container Feature %s resolved to %s\n", feature.reference, feature.digest)
	}
	imageConfiguration, err = devcontainer.ResolveLocalVariables(imageConfiguration, resolved.Workspace, resolved.LocalEnvironment, resolved.DevContainerID)
	if err != nil {
		return "", nil, err
	}
	inspect, err := e.inspectImage(ctx, baseImage)
	if err != nil {
		return "", nil, fmt.Errorf("inspect Dev Container base image: %w", err)
	}
	containerEnvironment := map[string]string{}
	if inspect.Config != nil {
		for _, item := range inspect.Config.Env {
			if name, value, ok := strings.Cut(item, "="); ok {
				containerEnvironment[name] = value
			}
		}
	}
	imageConfiguration, err = devcontainer.ResolveContainerVariables(imageConfiguration, resolved.WorkspaceFolder, containerEnvironment)
	if err != nil {
		return "", nil, err
	}
	for name, value := range imageConfiguration.ContainerEnv {
		containerEnvironment[name] = value
	}
	effectiveConfiguration := imageConfiguration
	var lifecycle devcontainer.LifecycleCommands
	for _, source := range imageSources {
		source, err = devcontainer.ResolveLocalVariables(source, resolved.Workspace, resolved.LocalEnvironment, resolved.DevContainerID)
		if err != nil {
			return "", nil, err
		}
		source, err = devcontainer.ResolveContainerVariables(source, resolved.WorkspaceFolder, containerEnvironment)
		if err != nil {
			return "", nil, err
		}
		lifecycle.Append(source)
	}
	for _, feature := range orderedFeatures {
		featureConfiguration, err := devcontainer.ResolveLocalVariables(featureConfigurationFromMetadata(feature.metadata), resolved.Workspace, resolved.LocalEnvironment, resolved.DevContainerID)
		if err != nil {
			return "", nil, err
		}
		featureConfiguration, err = devcontainer.ResolveContainerVariables(featureConfiguration, resolved.WorkspaceFolder, containerEnvironment)
		if err != nil {
			return "", nil, err
		}
		for name, value := range featureConfiguration.ContainerEnv {
			value = os.Expand(value, func(name string) string { return containerEnvironment[name] })
			featureConfiguration.ContainerEnv[name] = value
			containerEnvironment[name] = value
		}
		effectiveConfiguration = devcontainer.Merge(effectiveConfiguration, featureConfiguration)
		lifecycle.Append(featureConfiguration)
		if strings.TrimSpace(feature.metadata.Entrypoint) != "" {
			installOnly := len(feature.references) > 0
			for _, reference := range feature.references {
				if _, ok := resolved.InstallOnlyFeatures[reference]; !ok {
					installOnly = false
					break
				}
			}
			if !installOnly {
				resolved.FeatureEntrypoints = append(resolved.FeatureEntrypoints, feature.metadata.Entrypoint)
			}
		}
	}
	repositoryEnvironment := make(map[string]string, len(containerEnvironment)+len(repositoryConfiguration.ContainerEnv))
	for name, value := range containerEnvironment {
		repositoryEnvironment[name] = value
	}
	for name, value := range repositoryConfiguration.ContainerEnv {
		if !strings.Contains(value, "${containerEnv:") {
			repositoryEnvironment[name] = value
		}
	}
	repositoryConfiguration, err = devcontainer.ResolveContainerVariables(repositoryConfiguration, resolved.WorkspaceFolder, repositoryEnvironment)
	if err != nil {
		return "", nil, err
	}
	resolved.Configuration = devcontainer.Merge(effectiveConfiguration, repositoryConfiguration)
	lifecycle.Append(repositoryConfiguration)
	resolved.LifecycleCommands = lifecycle
	if err := resolved.Finalize(); err != nil {
		return "", nil, err
	}
	if err := checkHostRequirements(resolved.HostRequirements, resolved.Workspace); err != nil {
		return "", nil, err
	}
	digests := make(map[string]string, len(orderedFeatures))
	for _, feature := range orderedFeatures {
		for _, reference := range feature.references {
			digests[reference] = feature.digest
		}
	}
	if !resolved.Synthetic {
		generated := devcontainer.Lockfile{Features: map[string]devcontainer.LockedFeature{}}
		for _, feature := range orderedFeatures {
			if !feature.lockable {
				continue
			}
			dependencies := make([]string, 0, len(feature.metadata.DependsOn))
			for dependency := range feature.metadata.DependsOn {
				dependencies = append(dependencies, strings.ToLower(dependency))
			}
			sort.Strings(dependencies)
			for _, reference := range feature.references {
				if _, excluded := resolved.PlatformFeatureReferences[strings.ToLower(strings.TrimSpace(reference))]; excluded {
					continue
				}
				generated.Features[strings.ToLower(reference)] = devcontainer.LockedFeature{
					Version: feature.metadata.Version, Resolved: feature.resolved, Integrity: feature.digest, DependsOn: dependencies,
				}
			}
		}
		if err := devcontainer.WriteLockfile(resolved.ConfigurationPath, generated, resolved.FrozenLockfile); err != nil {
			return "", nil, err
		}
	}
	containerUser := strings.TrimSpace(resolved.ContainerUser)
	if containerUser == "" && inspect.Config != nil {
		containerUser = strings.TrimSpace(inspect.Config.User)
	}
	if containerUser == "" {
		containerUser = "root"
	}
	resolved.ContainerUser = containerUser
	if strings.TrimSpace(resolved.RemoteUser) == "" {
		resolved.RemoteUser = findRunArgsUser(resolved.RunArgs)
		if resolved.RemoteUser == "" {
			resolved.RemoteUser = containerUser
		}
	}
	if len(orderedFeatures) == 0 {
		return baseImage, map[string]string{}, nil
	}
	cacheReference := featureImageArtifactCacheReference(resolved.Cache, inspect.ID, orderedFeatures, resolved.ContainerUser, resolved.RemoteUser, resolved.ContainerEnv)
	if e.useCachedImage(ctx, cacheReference, "features") {
		return cacheReference, digests, nil
	}
	if err := writeFeatureBuildContext(buildContext, baseImage, orderedFeatures, resolved.ContainerUser, resolved.RemoteUser, resolved.ContainerEnv); err != nil {
		return "", nil, err
	}
	imageName := "devcontainer-feature:" + resolved.DevContainerID
	if err := e.buildImage(ctx, buildContext, "Dockerfile.features", imageName, "", nil, nil, nil, resolved.Cache, "features"); err != nil {
		return "", nil, fmt.Errorf("build Dev Container Features: %w", err)
	}
	e.publishCachedImage(ctx, imageName, cacheReference, "features")
	return imageName, digests, nil
}

func featureImageArtifactCacheReference(cache devcontainer.CacheOptions, baseImageID string, features []*resolvedFeature, containerUser, remoteUser string, containerEnvironment map[string]string) string {
	parts := []string{
		"base", strings.TrimSpace(baseImageID),
		"container-user", strings.TrimSpace(containerUser),
		"remote-user", strings.TrimSpace(remoteUser),
		"container-env", stringMapCachePart(containerEnvironment),
	}
	for _, feature := range features {
		parts = append(parts,
			"feature", strings.TrimSpace(feature.reference),
			"digest", strings.TrimSpace(feature.digest),
			"options", stringMapCachePart(feature.options),
		)
	}
	return imageArtifactCacheReference(cache, "features", parts...)
}

func (e *Engine) fetchFeature(ctx context.Context, featureReference string, rawOptions json.RawMessage, directory string, locked devcontainer.LockedFeature, cache devcontainer.CacheOptions, configurationDir, allowedRoot string) (*resolvedFeature, error) {
	if strings.HasPrefix(featureReference, "./") || strings.HasPrefix(featureReference, "../") {
		path := filepath.Join(configurationDir, filepath.FromSlash(featureReference))
		path, err := resolvePathInsideRoot(allowedRoot, path)
		if err != nil {
			return nil, fmt.Errorf("resolve local Dev Container Feature %s: %w", featureReference, err)
		}
		if err := copyDirectory(path, directory); err != nil {
			return nil, fmt.Errorf("copy local Dev Container Feature %s: %w", featureReference, err)
		}
		return loadFeatureDirectory(featureReference, featureReference, "", rawOptions, directory, false)
	}
	if strings.HasPrefix(featureReference, "https://") || strings.HasPrefix(featureReference, "http://") {
		resolveReference := featureReference
		if locked.Resolved != "" {
			resolveReference = locked.Resolved
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, resolveReference, nil)
		if err != nil {
			return nil, fmt.Errorf("parse Dev Container Feature URL: %w", err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return nil, fmt.Errorf("download Dev Container Feature %s: %w", featureReference, err)
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return nil, fmt.Errorf("download Dev Container Feature %s: %s", featureReference, response.Status)
		}
		hash := sha256.New()
		if err := extractFeatureLayer(io.TeeReader(response.Body, hash), directory); err != nil {
			return nil, fmt.Errorf("extract Dev Container Feature %s: %w", featureReference, err)
		}
		digest := fmt.Sprintf("sha256:%x", hash.Sum(nil))
		if locked.Integrity != "" && locked.Integrity != digest {
			return nil, fmt.Errorf("dev container feature %s lockfile integrity does not match", featureReference)
		}
		return loadFeatureDirectory(featureReference, resolveReference, digest, rawOptions, directory, true)
	}
	resolveReference := featureReference
	if strings.TrimSpace(locked.Integrity) != "" {
		if strings.TrimSpace(locked.Resolved) == "" {
			return nil, fmt.Errorf("dev container feature %s lockfile entry is incomplete", featureReference)
		}
		resolveReference = locked.Resolved
	}
	fetchReference, mirrored, err := mirroredReference(resolveReference, cache.Mirrors)
	if err != nil {
		return nil, fmt.Errorf("resolve OCI mirror for Dev Container Feature %s: %w", featureReference, err)
	}
	registryClient, err := featureRegistryClient(cache.RootCAPEM)
	if err != nil {
		return nil, fmt.Errorf("configure OCI client for Dev Container Feature %s: %w", featureReference, err)
	}
	if mirrored {
		feature, err := fetchFeatureReference(ctx, featureReference, fetchReference, rawOptions, directory, locked, cache.Credentials, registryClient)
		if err == nil {
			_, _ = fmt.Fprintf(e.stdout, "Dev Container Feature %s fetched through mirror %s\n", featureReference, fetchReference)
			return feature, nil
		}
		_, _ = fmt.Fprintf(e.stderr, "Warning: OCI mirror for Dev Container Feature %s is unavailable, falling back to the original registry: %v\n", featureReference, err)
		if err := os.RemoveAll(directory); err != nil {
			return nil, err
		}
	}
	return fetchFeatureReference(ctx, featureReference, resolveReference, rawOptions, directory, locked, cache.Credentials, registryClient)
}

func featureRegistryClient(rootCAPEM []byte) (*http.Client, error) {
	if len(rootCAPEM) == 0 {
		return retry.DefaultClient, nil
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("load system CA pool: %w", err)
	}
	if !roots.AppendCertsFromPEM(rootCAPEM) {
		return nil, fmt.Errorf("internal registry CA is invalid")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	return &http.Client{Transport: retry.NewTransport(transport)}, nil
}

func fetchFeatureReference(ctx context.Context, featureReference, resolveReference string, rawOptions json.RawMessage, directory string, locked devcontainer.LockedFeature, registryCredentials map[string]devcontainer.RegistryCredential, registryClient *http.Client) (*resolvedFeature, error) {
	original, err := registry.ParseReference(featureReference)
	if err != nil {
		return nil, fmt.Errorf("parse Dev Container Feature reference %s: %w", featureReference, err)
	}
	parsed, err := registry.ParseReference(resolveReference)
	if err != nil {
		return nil, fmt.Errorf("parse Dev Container Feature %q: %w", featureReference, err)
	}
	if parsed.Reference == "" {
		parsed.Reference = "latest"
	}
	repository, err := remote.NewRepository(parsed.Registry + "/" + parsed.Repository)
	if err != nil {
		return nil, err
	}
	credentialStore, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
	if err != nil {
		return nil, fmt.Errorf("open Docker credential store: %w", err)
	}
	credential := credentials.Credential(credentialStore)
	if configured, ok := registryCredentials[parsed.Registry]; ok {
		credential = auth.StaticCredential(parsed.Registry, auth.Credential{Username: configured.Username, Password: configured.Password})
	}
	repository.Client = &auth.Client{Client: registryClient, Cache: auth.NewCache(), Credential: credential}
	descriptor, err := repository.Resolve(ctx, parsed.Reference)
	if err != nil {
		return nil, fmt.Errorf("resolve Dev Container Feature %s: %w", featureReference, err)
	}
	if locked.Integrity != "" && descriptor.Digest.String() != locked.Integrity {
		return nil, fmt.Errorf("dev container feature %s lockfile integrity does not match", featureReference)
	}
	manifestReader, err := repository.Fetch(ctx, descriptor)
	if err != nil {
		return nil, fmt.Errorf("fetch Dev Container Feature manifest %s: %w", featureReference, err)
	}
	var manifest ocispec.Manifest
	decodeErr := json.NewDecoder(manifestReader).Decode(&manifest)
	closeErr := manifestReader.Close()
	if err := errors.Join(decodeErr, closeErr); err != nil {
		return nil, fmt.Errorf("decode Dev Container Feature manifest %s: %w", featureReference, err)
	}
	if !strings.HasPrefix(manifest.Config.MediaType, devContainerFeatureMediaType) {
		return nil, fmt.Errorf("oci artifact %s is not a Dev Container Feature", featureReference)
	}
	if len(manifest.Layers) != 1 {
		return nil, fmt.Errorf("dev container feature %s must contain one layer", featureReference)
	}
	layer, err := repository.Fetch(ctx, manifest.Layers[0])
	if err != nil {
		return nil, fmt.Errorf("fetch Dev Container Feature layer %s: %w", featureReference, err)
	}
	if err := extractFeatureLayer(layer, directory); err != nil {
		_ = layer.Close()
		return nil, fmt.Errorf("extract Dev Container Feature %s: %w", featureReference, err)
	}
	if err := layer.Close(); err != nil {
		return nil, err
	}
	return loadFeatureDirectory(featureReference, original.Registry+"/"+original.Repository+"@"+descriptor.Digest.String(), descriptor.Digest.String(), rawOptions, directory, true)
}

func loadFeatureDirectory(reference, resolved, digest string, rawOptions json.RawMessage, directory string, lockable bool) (*resolvedFeature, error) {
	metadataFile, err := os.Open(filepath.Join(directory, "devcontainer-feature.json"))
	if err != nil {
		return nil, fmt.Errorf("open Feature metadata: %w", err)
	}
	decoder := json.NewDecoder(metadataFile)
	decoder.DisallowUnknownFields()
	var metadata featureMetadata
	decodeErr := decoder.Decode(&metadata)
	if err := errors.Join(decodeErr, metadataFile.Close()); err != nil {
		return nil, fmt.Errorf("decode Feature metadata: %w", err)
	}
	if strings.TrimSpace(metadata.ID) == "" || strings.TrimSpace(metadata.Version) == "" {
		return nil, fmt.Errorf("dev container feature %s identity is incomplete", reference)
	}
	if _, err := os.Stat(filepath.Join(directory, "install.sh")); err != nil {
		return nil, fmt.Errorf("dev container feature %s install.sh is missing", reference)
	}
	options, providedOptions, err := resolveFeatureOptions(metadata.Options, rawOptions)
	if err != nil {
		return nil, fmt.Errorf("dev container feature %s: %w", reference, err)
	}
	return &resolvedFeature{reference: reference, resolved: resolved, digest: digest, directory: directory, options: options, providedOptions: providedOptions, metadata: metadata, lockable: lockable}, nil
}

func extractFeatureLayer(reader io.Reader, directory string) error {
	buffered := bufio.NewReader(reader)
	var tarReader *tar.Reader
	if header, _ := buffered.Peek(2); len(header) == 2 && header[0] == 0x1f && header[1] == 0x8b {
		compressed, err := gzip.NewReader(buffered)
		if err != nil {
			return err
		}
		defer func() { _ = compressed.Close() }()
		tarReader = tar.NewReader(compressed)
	} else {
		tarReader = tar.NewReader(buffered)
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(header.Name))
		if name == "." {
			if header.Typeflag == tar.TypeDir {
				continue
			}
			return fmt.Errorf("feature archive path %q is invalid", header.Name)
		}
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("feature archive path %q is invalid", header.Name)
		}
		target := filepath.Join(directory, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)&0o777); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode)&0o777)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(file, io.LimitReader(tarReader, header.Size))
			if err := errors.Join(copyErr, file.Close()); err != nil {
				return err
			}
		case tar.TypeSymlink:
			link := filepath.Clean(filepath.FromSlash(header.Linkname))
			resolved := filepath.Clean(filepath.Join(filepath.Dir(name), link))
			if filepath.IsAbs(link) || resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
				return fmt.Errorf("feature archive link %q leaves the Feature root", header.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
		case tar.TypeLink:
			link := filepath.Clean(filepath.FromSlash(header.Linkname))
			if filepath.IsAbs(link) || link == ".." || strings.HasPrefix(link, ".."+string(filepath.Separator)) {
				return fmt.Errorf("feature archive link %q leaves the Feature root", header.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Link(filepath.Join(directory, link), target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("feature archive entry %q has unsupported type", header.Name)
		}
	}
}

func normalizeFeatureRequest(raw json.RawMessage) (bool, json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("true")) {
		return true, json.RawMessage(`{}`), nil
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return false, nil, fmt.Errorf("options must be a string, boolean, or object")
	}
	if bytes.Equal(trimmed, []byte("false")) {
		return false, nil, nil
	}
	var version string
	if json.Unmarshal(trimmed, &version) == nil {
		encoded, _ := json.Marshal(map[string]string{"version": version})
		return true, encoded, nil
	}
	var options map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &options); err != nil || options == nil {
		return false, nil, fmt.Errorf("options must be a string, boolean, or object")
	}
	for name, value := range options {
		var scalar any
		if err := json.Unmarshal(value, &scalar); err != nil {
			return false, nil, fmt.Errorf("option %q is invalid", name)
		}
		if _, stringValue := scalar.(string); !stringValue {
			if _, booleanValue := scalar.(bool); !booleanValue {
				return false, nil, fmt.Errorf("option %q must be a string or boolean", name)
			}
		}
	}
	encoded, err := json.Marshal(options)
	return true, encoded, err
}

func resolveFeatureOptions(declared map[string]featureOption, raw json.RawMessage) (map[string]string, map[string]string, error) {
	enabled, normalized, err := normalizeFeatureRequest(raw)
	if err != nil {
		return nil, nil, err
	}
	if !enabled {
		return nil, nil, fmt.Errorf("feature is disabled")
	}
	provided := map[string]json.RawMessage{}
	if err := json.Unmarshal(normalized, &provided); err != nil {
		return nil, nil, err
	}
	for name := range provided {
		if _, ok := declared[name]; !ok {
			return nil, nil, fmt.Errorf("option %q is not declared", name)
		}
	}
	result := make(map[string]string, len(declared))
	providedValues := make(map[string]string, len(provided))
	for name, option := range declared {
		environmentName := featureOptionEnvironmentName(name)
		if environmentName == "" {
			return nil, nil, fmt.Errorf("option name %q is invalid", name)
		}
		value, userProvided := provided[name]
		if !userProvided {
			value = option.Default
		}
		if len(value) == 0 {
			continue
		}
		var scalar any
		if err := json.Unmarshal(value, &scalar); err != nil {
			return nil, nil, fmt.Errorf("option %q is invalid", name)
		}
		var setting string
		switch option.Type {
		case "", "string":
			switch value := scalar.(type) {
			case string:
				setting = value
			case bool:
				if !userProvided {
					return nil, nil, fmt.Errorf("option %q default must be a string", name)
				}
				setting = strconv.FormatBool(value)
			default:
				return nil, nil, fmt.Errorf("option %q must be a string or boolean", name)
			}
			if len(option.Enum) > 0 {
				if !slices.Contains(option.Enum, setting) {
					return nil, nil, fmt.Errorf("option %q value %q is not allowed", name, setting)
				}
			}
		case "boolean":
			switch value := scalar.(type) {
			case bool:
				setting = strconv.FormatBool(value)
			case string:
				if !userProvided || value != "true" && value != "false" {
					return nil, nil, fmt.Errorf("option %q must be a boolean or boolean text", name)
				}
				setting = value
			default:
				return nil, nil, fmt.Errorf("option %q must be a boolean or boolean text", name)
			}
		default:
			return nil, nil, fmt.Errorf("option %q has unsupported type %q", name, option.Type)
		}
		result[environmentName] = setting
		if userProvided {
			providedValues[name] = setting
		}
	}
	return result, providedValues, nil
}

func featuresEqual(left, right *resolvedFeature) bool {
	return left.lockable && right.lockable && left.digest == right.digest && maps.Equal(left.options, right.options)
}

func featureOptionEnvironmentName(name string) string {
	var result strings.Builder
	for _, char := range strings.ToUpper(name) {
		if char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' {
			result.WriteRune(char)
		} else {
			result.WriteByte('_')
		}
	}
	value := result.String()
	leading := 0
	for leading < len(value) && (value[leading] == '_' || value[leading] >= '0' && value[leading] <= '9') {
		leading++
	}
	if leading > 0 {
		value = "_" + value[leading:]
	}
	return value
}

func orderFeatures(features []*resolvedFeature, override []string) ([]*resolvedFeature, error) {
	priorities := map[*resolvedFeature]int{}
	for index, requested := range override {
		matches := findFeatureMatches(features, requested)
		if len(matches) == 0 {
			return nil, fmt.Errorf("overrideFeatureInstallOrder: Feature %s is not resolved", requested)
		}
		for _, feature := range matches {
			priorities[feature] = len(override) - index
		}
	}
	installed := map[*resolvedFeature]bool{}
	ordered := make([]*resolvedFeature, 0, len(features))
	for len(ordered) < len(features) {
		var candidates []*resolvedFeature
		for _, feature := range features {
			if installed[feature] {
				continue
			}
			ready := true
			for _, dependency := range append(slices.Clone(feature.hardDependencies), feature.softDependencies...) {
				if !installed[dependency] {
					ready = false
					break
				}
			}
			if ready {
				candidates = append(candidates, feature)
			}
		}
		if len(candidates) == 0 {
			return nil, fmt.Errorf("dev container feature dependency graph contains a cycle")
		}
		maxPriority := 0
		for _, feature := range candidates {
			if priorities[feature] > maxPriority {
				maxPriority = priorities[feature]
			}
		}
		candidates = slices.DeleteFunc(candidates, func(feature *resolvedFeature) bool { return priorities[feature] != maxPriority })
		slices.SortFunc(candidates, compareFeatures)
		for _, feature := range candidates {
			installed[feature] = true
			ordered = append(ordered, feature)
		}
	}
	return ordered, nil
}

func findFeatureMatches(features []*resolvedFeature, requested string) []*resolvedFeature {
	requestedID, _ := featureReferenceID(requested)
	var matches []*resolvedFeature
	for _, feature := range features {
		candidateID, _ := featureReferenceID(feature.reference)
		matched := slices.ContainsFunc(feature.references, func(reference string) bool { return strings.EqualFold(reference, requested) }) || requestedID != "" && strings.EqualFold(candidateID, requestedID) || strings.EqualFold(feature.metadata.ID, requested)
		if !matched {
			for _, legacyID := range feature.metadata.LegacyIDs {
				if strings.EqualFold(legacyID, requested) {
					matched = true
					break
				}
			}
		}
		if matched {
			matches = append(matches, feature)
		}
	}
	return matches
}

func compareFeatures(left, right *resolvedFeature) int {
	leftID, _ := featureReferenceID(left.reference)
	rightID, _ := featureReferenceID(right.reference)
	if compared := strings.Compare(leftID, rightID); compared != 0 {
		return compared
	}
	leftTag, rightTag := featureReferenceVersion(left.reference), featureReferenceVersion(right.reference)
	if leftTag != rightTag {
		if leftTag == "latest" {
			return 1
		}
		if rightTag == "latest" {
			return -1
		}
		leftVersion, leftErr := semver.NewVersion(leftTag)
		rightVersion, rightErr := semver.NewVersion(rightTag)
		if leftErr == nil && rightErr == nil {
			if compared := leftVersion.Compare(rightVersion); compared != 0 {
				return compared
			}
		} else if compared := strings.Compare(leftTag, rightTag); compared != 0 {
			return compared
		}
	}
	if len(left.providedOptions) != len(right.providedOptions) {
		return len(right.providedOptions) - len(left.providedOptions)
	}
	leftOptions, _ := json.Marshal(left.providedOptions)
	rightOptions, _ := json.Marshal(right.providedOptions)
	if compared := bytes.Compare(leftOptions, rightOptions); compared != 0 {
		return compared
	}
	return strings.Compare(left.resolved+"\x00"+left.reference, right.resolved+"\x00"+right.reference)
}

func featureReferenceVersion(reference string) string {
	parsed, err := registry.ParseReference(reference)
	if err != nil || parsed.Reference == "" || strings.HasPrefix(parsed.Reference, "sha256:") {
		return "latest"
	}
	return parsed.Reference
}

func featureReferenceID(reference string) (string, error) {
	if strings.HasPrefix(reference, "./") || strings.HasPrefix(reference, "../") {
		return "local:" + strings.ToLower(filepath.ToSlash(filepath.Clean(reference))), nil
	}
	if strings.HasPrefix(reference, "https://") || strings.HasPrefix(reference, "http://") {
		return strings.ToLower(reference), nil
	}
	parsed, err := registry.ParseReference(reference)
	if err != nil {
		return "", fmt.Errorf("parse Feature %q: %w", reference, err)
	}
	return strings.ToLower(parsed.Registry + "/" + parsed.Repository), nil
}

func writeFeatureBuildContext(root, baseImage string, features []*resolvedFeature, containerUser, remoteUser string, containerEnvironment map[string]string) error {
	var dockerfile strings.Builder
	_, _ = fmt.Fprintf(&dockerfile, "FROM %s\nUSER root\n", baseImage)
	environmentNames := make([]string, 0, len(containerEnvironment))
	for name := range containerEnvironment {
		environmentNames = append(environmentNames, name)
	}
	sort.Strings(environmentNames)
	for _, name := range environmentNames {
		_, _ = fmt.Fprintf(&dockerfile, "ENV %s=%s\n", name, shellQuote(containerEnvironment[name]))
	}
	dockerfile.WriteString("RUN mkdir -p /tmp/dev-container-feature && \\\n")
	_, _ = fmt.Fprintf(&dockerfile, "    printf '%%s\\n' '_CONTAINER_USER=%s' '_REMOTE_USER=%s' > /tmp/dev-container-feature/devcontainer-features.builtin.env && \\\n", containerUser, remoteUser)
	_, _ = fmt.Fprintf(&dockerfile, "    echo \"_CONTAINER_USER_HOME=$(getent passwd %s 2>/dev/null | cut -d: -f6)\" >> /tmp/dev-container-feature/devcontainer-features.builtin.env && \\\n", shellQuote(containerUser))
	_, _ = fmt.Fprintf(&dockerfile, "    echo \"_REMOTE_USER_HOME=$(getent passwd %s 2>/dev/null | cut -d: -f6)\" >> /tmp/dev-container-feature/devcontainer-features.builtin.env\n", shellQuote(remoteUser))
	for index, feature := range features {
		target := filepath.Join(root, "feature", strconv.Itoa(index))
		if err := copyDirectory(feature.directory, target); err != nil {
			return err
		}
		var featureEnvironment strings.Builder
		keys := make([]string, 0, len(feature.options))
		for key := range feature.options {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			_, _ = fmt.Fprintf(&featureEnvironment, "%s=%s\n", key, shellQuote(feature.options[key]))
		}
		if err := os.WriteFile(filepath.Join(target, "devcontainer-features.env"), []byte(featureEnvironment.String()), 0o600); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(&dockerfile, "COPY feature/%d /tmp/dev-container-feature/%d\n", index, index)
		_, _ = fmt.Fprintf(&dockerfile, "RUN cd /tmp/dev-container-feature/%d && chmod +x ./install.sh && set -a && . ../devcontainer-features.builtin.env && . ./devcontainer-features.env && set +a && ./install.sh && rm -rf /tmp/dev-container-feature/%d\n", index, index)
	}
	if containerUser != "" {
		_, _ = fmt.Fprintf(&dockerfile, "USER %s\n", containerUser)
	}
	return os.WriteFile(filepath.Join(root, "Dockerfile.features"), []byte(dockerfile.String()), 0o600)
}

func copyDirectory(source, target string) error {
	return filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, relative)
		if info.IsDir() {
			return os.MkdirAll(destination, info.Mode().Perm())
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return err
			}
			if relative, err := filepath.Rel(source, resolved); err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return fmt.Errorf("local Feature link %s leaves the Feature root", relative)
			}
			if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
				return err
			}
			return os.Symlink(link, destination)
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			_ = input.Close()
			return err
		}
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			_ = input.Close()
			return err
		}
		_, copyErr := io.Copy(output, input)
		return errors.Join(copyErr, input.Close(), output.Close())
	})
}

func featureConfigurationFromMetadata(metadata featureMetadata) devcontainer.Configuration {
	return devcontainer.Configuration{
		ContainerEnv: metadata.ContainerEnv, Mounts: metadata.Mounts,
		Init: metadata.Init, Privileged: metadata.Privileged, CapAdd: metadata.CapAdd, SecurityOpt: metadata.SecurityOpt,
		OnCreateCommand: metadata.OnCreateCommand, UpdateContentCommand: metadata.UpdateContentCommand,
		PostCreateCommand: metadata.PostCreateCommand, PostStartCommand: metadata.PostStartCommand,
		PostAttachCommand: metadata.PostAttachCommand, Customizations: metadata.Customizations,
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
