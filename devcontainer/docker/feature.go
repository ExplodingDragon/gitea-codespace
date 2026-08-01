// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/distribution/reference"
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
	reference string
	resolved  string
	digest    string
	directory string
	options   map[string]string
	metadata  featureMetadata
	lockable  bool
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

func (e *Engine) applyFeatures(ctx context.Context, baseImage string, resolved *devcontainer.ResolvedConfiguration, imageConfiguration, repositoryConfiguration devcontainer.Configuration) (string, map[string]string, error) {
	lockfile, lockfileExists, err := devcontainer.ReadLockfile(resolved.ConfigurationPath)
	if err != nil {
		return "", nil, devcontainer.InvalidConfiguration(err)
	}
	if resolved.FrozenLockfile && !resolved.Synthetic && !lockfileExists {
		return "", nil, devcontainer.InvalidConfiguration(fmt.Errorf("Dev Container lockfile does not exist"))
	}
	requestedFeatures := make(map[string]json.RawMessage, len(resolved.Features))
	for reference, options := range resolved.Features {
		requestedFeatures[reference] = options
	}
	buildContext, err := os.MkdirTemp("", "devcontainer-features-*")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(buildContext)
	features := map[string]*resolvedFeature{}
	var resolveFeature func(string, json.RawMessage) error
	resolveFeature = func(reference string, rawOptions json.RawMessage) error {
		if _, ok := features[reference]; ok {
			return nil
		}
		directory := filepath.Join(buildContext, strconv.Itoa(len(features)))
		feature, err := e.fetchFeature(ctx, reference, rawOptions, directory, lockfile.Features[reference], resolved.Cache, resolved.ConfigurationDir, resolved.AllowedPathRoot)
		if err != nil {
			return err
		}
		features[reference] = feature
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
			if err := resolveFeature(dependency, options); err != nil {
				return fmt.Errorf("resolve dependency of Feature %s: %w", reference, err)
			}
		}
		return nil
	}
	references := make([]string, 0, len(requestedFeatures))
	for reference := range requestedFeatures {
		references = append(references, reference)
	}
	sort.Strings(references)
	for _, reference := range references {
		if err := resolveFeature(reference, requestedFeatures[reference]); err != nil {
			return "", nil, err
		}
	}
	orderedFeatures, err := orderFeatures(features, resolved.OverrideFeatureInstallOrder)
	if err != nil {
		return "", nil, devcontainer.InvalidConfiguration(err)
	}
	for _, feature := range orderedFeatures {
		fmt.Fprintf(e.stdout, "Dev Container Feature %s resolved to %s\n", feature.reference, feature.digest)
	}
	imageConfiguration, err = devcontainer.ResolveLocalVariables(imageConfiguration, resolved.Workspace, resolved.LocalEnvironment, resolved.DevContainerID)
	if err != nil {
		return "", nil, devcontainer.InvalidConfiguration(err)
	}
	inspect, err := e.client.ImageInspect(ctx, baseImage)
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
		return "", nil, devcontainer.InvalidConfiguration(err)
	}
	for name, value := range imageConfiguration.ContainerEnv {
		containerEnvironment[name] = value
	}
	effectiveConfiguration := imageConfiguration
	for _, feature := range orderedFeatures {
		featureConfiguration, err := devcontainer.ResolveLocalVariables(featureConfigurationFromMetadata(feature.metadata), resolved.Workspace, resolved.LocalEnvironment, resolved.DevContainerID)
		if err != nil {
			return "", nil, devcontainer.InvalidConfiguration(err)
		}
		featureConfiguration, err = devcontainer.ResolveContainerVariables(featureConfiguration, resolved.WorkspaceFolder, containerEnvironment)
		if err != nil {
			return "", nil, devcontainer.InvalidConfiguration(err)
		}
		for name, value := range featureConfiguration.ContainerEnv {
			value = os.Expand(value, func(name string) string { return containerEnvironment[name] })
			featureConfiguration.ContainerEnv[name] = value
			containerEnvironment[name] = value
		}
		effectiveConfiguration = devcontainer.Merge(effectiveConfiguration, featureConfiguration)
		if strings.TrimSpace(feature.metadata.Entrypoint) != "" {
			if _, installOnly := resolved.InstallOnlyFeatures[feature.reference]; !installOnly {
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
		return "", nil, devcontainer.InvalidConfiguration(err)
	}
	resolved.Configuration = devcontainer.Merge(effectiveConfiguration, repositoryConfiguration)
	if err := resolved.Configuration.Finalize(); err != nil {
		return "", nil, devcontainer.InvalidConfiguration(err)
	}
	if err := checkHostRequirements(resolved.HostRequirements, resolved.Workspace); err != nil {
		return "", nil, devcontainer.InvalidConfiguration(err)
	}
	if !resolved.Synthetic {
		generated := devcontainer.Lockfile{Features: map[string]devcontainer.LockedFeature{}}
		for _, feature := range orderedFeatures {
			if _, excluded := resolved.InjectedFeatureReferences[feature.reference]; excluded || !feature.lockable {
				continue
			}
			dependencies := make([]string, 0, len(feature.metadata.DependsOn))
			for dependency := range feature.metadata.DependsOn {
				dependencies = append(dependencies, dependency)
			}
			sort.Strings(dependencies)
			generated.Features[feature.reference] = devcontainer.LockedFeature{
				Version: feature.metadata.Version, Resolved: feature.resolved, Integrity: feature.digest, DependsOn: dependencies,
			}
		}
		if err := devcontainer.WriteLockfile(resolved.ConfigurationPath, generated, resolved.FrozenLockfile); err != nil {
			return "", nil, devcontainer.InvalidConfiguration(err)
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
	if err := writeFeatureBuildContext(buildContext, baseImage, orderedFeatures, resolved.ContainerUser, resolved.RemoteUser, resolved.ContainerEnv); err != nil {
		return "", nil, err
	}
	imageName := "devcontainer-feature:" + resolved.DevContainerID
	if err := e.buildImage(ctx, buildContext, "Dockerfile.features", imageName, "", nil, nil, nil, resolved.Cache, "features"); err != nil {
		return "", nil, fmt.Errorf("build Dev Container Features: %w", err)
	}
	digests := make(map[string]string, len(orderedFeatures))
	for _, feature := range orderedFeatures {
		digests[feature.reference] = feature.digest
	}
	return imageName, digests, nil
}

func (e *Engine) fetchFeature(ctx context.Context, featureReference string, rawOptions json.RawMessage, directory string, locked devcontainer.LockedFeature, cache devcontainer.CacheOptions, configurationDir, allowedRoot string) (*resolvedFeature, error) {
	if strings.HasPrefix(featureReference, "./") || strings.HasPrefix(featureReference, "../") {
		path := filepath.Join(configurationDir, filepath.FromSlash(featureReference))
		path, err := resolvePathInsideRoot(allowedRoot, path)
		if err != nil {
			return nil, devcontainer.InvalidConfiguration(fmt.Errorf("resolve local Dev Container Feature %s: %w", featureReference, err))
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
			return nil, devcontainer.InvalidConfiguration(fmt.Errorf("parse Dev Container Feature URL: %w", err))
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return nil, fmt.Errorf("download Dev Container Feature %s: %w", featureReference, err)
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return nil, fmt.Errorf("download Dev Container Feature %s: %s", featureReference, response.Status)
		}
		hash := sha256.New()
		if err := extractFeatureLayer(io.TeeReader(response.Body, hash), directory); err != nil {
			return nil, fmt.Errorf("extract Dev Container Feature %s: %w", featureReference, err)
		}
		digest := fmt.Sprintf("sha256:%x", hash.Sum(nil))
		if locked.Integrity != "" && locked.Integrity != digest {
			return nil, devcontainer.InvalidConfiguration(fmt.Errorf("Dev Container Feature %s lockfile integrity does not match", featureReference))
		}
		return loadFeatureDirectory(featureReference, resolveReference, digest, rawOptions, directory, true)
	}
	resolveReference := featureReference
	if strings.TrimSpace(locked.Integrity) != "" {
		if strings.TrimSpace(locked.Resolved) == "" {
			return nil, devcontainer.InvalidConfiguration(fmt.Errorf("Dev Container Feature %s lockfile entry is incomplete", featureReference))
		}
		resolveReference = locked.Resolved
	}
	fetchReference, mirrored, err := mirroredReference(resolveReference, cache.Mirrors)
	if err != nil {
		return nil, fmt.Errorf("resolve OCI mirror for Dev Container Feature %s: %w", featureReference, err)
	}
	if mirrored {
		plainHTTP := false
		if named, parseErr := reference.ParseNormalizedNamed(resolveReference); parseErr == nil {
			if mirrorURL, parseErr := parseOCIRepositoryBase(cache.Mirrors[reference.Domain(named)], false); parseErr == nil {
				plainHTTP = mirrorURL.Scheme == "http"
			}
		}
		feature, err := fetchFeatureReference(ctx, featureReference, fetchReference, rawOptions, directory, locked, plainHTTP)
		if err == nil {
			fmt.Fprintf(e.stdout, "Dev Container Feature %s fetched through mirror %s\n", featureReference, fetchReference)
			return feature, nil
		}
		fmt.Fprintf(e.stderr, "Warning: OCI mirror for Dev Container Feature %s is unavailable, falling back to the original registry: %v\n", featureReference, err)
		if err := os.RemoveAll(directory); err != nil {
			return nil, err
		}
	}
	return fetchFeatureReference(ctx, featureReference, resolveReference, rawOptions, directory, locked, false)
}

func fetchFeatureReference(ctx context.Context, featureReference, resolveReference string, rawOptions json.RawMessage, directory string, locked devcontainer.LockedFeature, plainHTTP bool) (*resolvedFeature, error) {
	original, err := registry.ParseReference(featureReference)
	if err != nil {
		return nil, devcontainer.InvalidConfiguration(fmt.Errorf("parse Dev Container Feature reference %s: %w", featureReference, err))
	}
	parsed, err := registry.ParseReference(resolveReference)
	if err != nil {
		return nil, devcontainer.InvalidConfiguration(fmt.Errorf("parse Dev Container Feature %q: %w", featureReference, err))
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
	repository.Client = &auth.Client{Client: retry.DefaultClient, Cache: auth.NewCache(), Credential: credentials.Credential(credentialStore)}
	repository.PlainHTTP = plainHTTP
	descriptor, err := repository.Resolve(ctx, parsed.Reference)
	if err != nil {
		return nil, fmt.Errorf("resolve Dev Container Feature %s: %w", featureReference, err)
	}
	if locked.Integrity != "" && descriptor.Digest.String() != locked.Integrity {
		return nil, devcontainer.InvalidConfiguration(fmt.Errorf("Dev Container Feature %s lockfile integrity does not match", featureReference))
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
		return nil, devcontainer.InvalidConfiguration(fmt.Errorf("OCI artifact %s is not a Dev Container Feature", featureReference))
	}
	if len(manifest.Layers) != 1 {
		return nil, devcontainer.InvalidConfiguration(fmt.Errorf("Dev Container Feature %s must contain one layer", featureReference))
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
		return nil, devcontainer.InvalidConfiguration(fmt.Errorf("open Feature metadata: %w", err))
	}
	decoder := json.NewDecoder(metadataFile)
	decoder.DisallowUnknownFields()
	var metadata featureMetadata
	decodeErr := decoder.Decode(&metadata)
	if err := errors.Join(decodeErr, metadataFile.Close()); err != nil {
		return nil, devcontainer.InvalidConfiguration(fmt.Errorf("decode Feature metadata: %w", err))
	}
	if strings.TrimSpace(metadata.ID) == "" || strings.TrimSpace(metadata.Version) == "" {
		return nil, devcontainer.InvalidConfiguration(fmt.Errorf("Dev Container Feature %s identity is incomplete", reference))
	}
	if _, err := os.Stat(filepath.Join(directory, "install.sh")); err != nil {
		return nil, devcontainer.InvalidConfiguration(fmt.Errorf("Dev Container Feature %s install.sh is missing", reference))
	}
	options, err := resolveFeatureOptions(metadata.Options, rawOptions)
	if err != nil {
		return nil, devcontainer.InvalidConfiguration(fmt.Errorf("Dev Container Feature %s: %w", reference, err))
	}
	return &resolvedFeature{reference: reference, resolved: resolved, digest: digest, directory: directory, options: options, metadata: metadata, lockable: lockable}, nil
}

func extractFeatureLayer(reader io.Reader, directory string) error {
	buffered := bufio.NewReader(reader)
	var tarReader *tar.Reader
	if header, _ := buffered.Peek(2); len(header) == 2 && header[0] == 0x1f && header[1] == 0x8b {
		compressed, err := gzip.NewReader(buffered)
		if err != nil {
			return err
		}
		defer compressed.Close()
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
			return fmt.Errorf("Feature archive path %q is invalid", header.Name)
		}
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("Feature archive path %q is invalid", header.Name)
		}
		target := filepath.Join(directory, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)&0o777); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
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
				return fmt.Errorf("Feature archive link %q leaves the Feature root", header.Name)
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
				return fmt.Errorf("Feature archive link %q leaves the Feature root", header.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Link(filepath.Join(directory, link), target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("Feature archive entry %q has unsupported type", header.Name)
		}
	}
}

func resolveFeatureOptions(declared map[string]featureOption, raw json.RawMessage) (map[string]string, error) {
	provided := map[string]json.RawMessage{}
	if len(raw) > 0 && string(raw) != "true" && string(raw) != "null" {
		if err := json.Unmarshal(raw, &provided); err != nil {
			return nil, fmt.Errorf("options must be true or an object")
		}
	}
	for name := range provided {
		if _, ok := declared[name]; !ok {
			return nil, fmt.Errorf("option %q is not declared", name)
		}
	}
	result := make(map[string]string, len(declared))
	for name, option := range declared {
		environmentName := featureOptionEnvironmentName(name)
		if environmentName == "" {
			return nil, fmt.Errorf("option name %q is invalid", name)
		}
		value := provided[name]
		if len(value) == 0 {
			value = option.Default
		}
		if len(value) == 0 {
			continue
		}
		var scalar any
		if err := json.Unmarshal(value, &scalar); err != nil {
			return nil, fmt.Errorf("option %q is invalid", name)
		}
		switch option.Type {
		case "", "string":
			setting, ok := scalar.(string)
			if !ok {
				return nil, fmt.Errorf("option %q must be a string", name)
			}
			if len(option.Enum) > 0 {
				if !slices.Contains(option.Enum, setting) {
					return nil, fmt.Errorf("option %q value %q is not allowed", name, setting)
				}
			}
			result[environmentName] = setting
		case "boolean":
			setting, ok := scalar.(bool)
			if !ok {
				return nil, fmt.Errorf("option %q must be a boolean", name)
			}
			result[environmentName] = strconv.FormatBool(setting)
		default:
			return nil, fmt.Errorf("option %q has unsupported type %q", name, option.Type)
		}
	}
	return result, nil
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
	if value != "" && value[0] >= '0' && value[0] <= '9' {
		value = "_" + value
	}
	return value
}

func orderFeatures(features map[string]*resolvedFeature, override []string) ([]*resolvedFeature, error) {
	dependencies := make(map[string][]string, len(features))
	installsAfter := make(map[string][]string, len(features))
	for reference, feature := range features {
		for dependency := range feature.metadata.DependsOn {
			resolved, ok := findFeatureReference(features, dependency)
			if !ok {
				return nil, fmt.Errorf("Feature %s dependency %s is not resolved", reference, dependency)
			}
			dependencies[reference] = appendUnique(dependencies[reference], []string{resolved})
		}
		for _, dependency := range feature.metadata.InstallsAfter {
			if resolved, ok := findFeatureReference(features, dependency); ok {
				installsAfter[reference] = appendUnique(installsAfter[reference], []string{resolved})
			}
		}
	}
	priorities := map[string]int{}
	for index, requested := range override {
		reference, ok := findFeatureReference(features, requested)
		if !ok {
			return nil, fmt.Errorf("overrideFeatureInstallOrder: Feature %s is not resolved", requested)
		}
		priorities[reference] = len(override) - index
	}
	installed := map[string]bool{}
	ordered := make([]*resolvedFeature, 0, len(features))
	for len(ordered) < len(features) {
		var candidates []string
		for reference := range features {
			if installed[reference] {
				continue
			}
			ready := true
			for _, dependency := range dependencies[reference] {
				if !installed[dependency] {
					ready = false
					break
				}
			}
			if ready {
				candidates = append(candidates, reference)
			}
		}
		if len(candidates) == 0 {
			return nil, fmt.Errorf("Dev Container Feature dependency graph contains a cycle")
		}
		preferred := slices.DeleteFunc(slices.Clone(candidates), func(reference string) bool {
			return slices.ContainsFunc(installsAfter[reference], func(dependency string) bool {
				return !installed[dependency]
			})
		})
		if len(preferred) > 0 {
			candidates = preferred
		}
		maxPriority := 0
		for _, reference := range candidates {
			if priorities[reference] > maxPriority {
				maxPriority = priorities[reference]
			}
		}
		candidates = slices.DeleteFunc(candidates, func(reference string) bool { return priorities[reference] != maxPriority })
		sort.Strings(candidates)
		for _, reference := range candidates {
			installed[reference] = true
			ordered = append(ordered, features[reference])
		}
	}
	return ordered, nil
}

func findFeatureReference(features map[string]*resolvedFeature, requested string) (string, bool) {
	if _, ok := features[requested]; ok {
		return requested, true
	}
	requestedID, _ := featureReferenceID(requested)
	var match string
	for reference, feature := range features {
		candidateID, _ := featureReferenceID(reference)
		matched := requestedID != "" && strings.EqualFold(candidateID, requestedID) || strings.EqualFold(feature.metadata.ID, requested)
		if !matched {
			for _, legacyID := range feature.metadata.LegacyIDs {
				if strings.EqualFold(legacyID, requested) {
					matched = true
					break
				}
			}
		}
		if matched {
			if match != "" {
				return "", false
			}
			match = reference
		}
	}
	return match, match != ""
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
	fmt.Fprintf(&dockerfile, "FROM %s\nUSER root\n", baseImage)
	environmentNames := make([]string, 0, len(containerEnvironment))
	for name := range containerEnvironment {
		environmentNames = append(environmentNames, name)
	}
	sort.Strings(environmentNames)
	for _, name := range environmentNames {
		fmt.Fprintf(&dockerfile, "ENV %s=%s\n", name, shellQuote(containerEnvironment[name]))
	}
	dockerfile.WriteString("RUN mkdir -p /tmp/dev-container-feature && \\\n")
	fmt.Fprintf(&dockerfile, "    printf '%%s\\n' '_CONTAINER_USER=%s' '_REMOTE_USER=%s' > /tmp/dev-container-feature/devcontainer-features.builtin.env && \\\n", containerUser, remoteUser)
	fmt.Fprintf(&dockerfile, "    echo \"_CONTAINER_USER_HOME=$(getent passwd %s 2>/dev/null | cut -d: -f6)\" >> /tmp/dev-container-feature/devcontainer-features.builtin.env && \\\n", shellQuote(containerUser))
	fmt.Fprintf(&dockerfile, "    echo \"_REMOTE_USER_HOME=$(getent passwd %s 2>/dev/null | cut -d: -f6)\" >> /tmp/dev-container-feature/devcontainer-features.builtin.env\n", shellQuote(remoteUser))
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
			fmt.Fprintf(&featureEnvironment, "%s=%s\n", key, shellQuote(feature.options[key]))
		}
		if err := os.WriteFile(filepath.Join(target, "devcontainer-features.env"), []byte(featureEnvironment.String()), 0o600); err != nil {
			return err
		}
		fmt.Fprintf(&dockerfile, "COPY feature/%d /tmp/dev-container-feature/%d\n", index, index)
		fmt.Fprintf(&dockerfile, "RUN cd /tmp/dev-container-feature/%d && chmod +x ./install.sh && set -a && . ../devcontainer-features.builtin.env && . ./devcontainer-features.env && set +a && ./install.sh && rm -rf /tmp/dev-container-feature/%d\n", index, index)
	}
	if containerUser != "" {
		fmt.Fprintf(&dockerfile, "USER %s\n", containerUser)
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

func appendUnique(values ...[]string) []string {
	seen := map[string]struct{}{}
	var result []string
	for _, list := range values {
		for _, value := range list {
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	return result
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
