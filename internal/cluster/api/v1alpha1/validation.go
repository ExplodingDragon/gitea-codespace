// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"uuid"

	"gitea.dev/codespace/devcontainer"
	"gitea.dev/codespace/internal/certificates"
	configpkg "gitea.dev/codespace/internal/config"
	"gitea.dev/codespace/internal/runtimeplatform"
	"github.com/distribution/reference"
	dockerunits "github.com/docker/go-units"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

const MaxObjectBytes = 1024 * 1024

var environmentNamePattern = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)

var (
	capabilityPattern  = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	cdiKindPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*/[A-Za-z0-9_.-]+$`)
	environmentPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

var platformEnvironmentNames = func() map[string]struct{} {
	names := map[string]struct{}{
		"GITEA_CLI_NO_UPDATE_NOTIFIER": {}, "GITEA_INSTANCE_URL": {},
		"NODE_EXTRA_CA_CERTS": {}, "SSL_CERT_FILE": {},
	}
	for _, name := range devcontainer.ProxyVariableNames() {
		names[name] = struct{}{}
	}
	return names
}()

var platformMountTargets = []string{
	"/var/lib/gitea-codespace",
	"/usr/local/bin/gitea-codespace-endpoint",
	"/usr/local/libexec/gitea-codespace-runtime",
}

var platformRuntimePaths = []string{
	"/etc/cdi",
	"/run/codespace",
	"/run/identity",
	"/var/lib/codespace",
	"/var/run/cdi",
}

const (
	maxNoProxyEntries = 128
	maxProxyCABundle  = 256 * 1024
	maxProxyURL       = 2048
	maxNoProxyEntry   = 1024
)

func SiteNamespace(name, managementNamespace string) (string, error) {
	if len(name) > 53 || len(validation.IsDNS1123Label(name)) != 0 {
		return "", fmt.Errorf("site name must be a DNS label of at most 53 characters")
	}
	namespace := "codespace-" + name
	if namespace == managementNamespace {
		return "", fmt.Errorf("site namespace conflicts with the management namespace")
	}
	return namespace, nil
}

func (s *GiteaSite) Validate(managementNamespace string) error {
	if _, err := SiteNamespace(s.Name, managementNamespace); err != nil {
		return err
	}
	if strings.TrimSpace(s.Spec.DisplayName) == "" || len(s.Spec.DisplayName) > 255 || s.Spec.ManagerID <= 0 {
		return fmt.Errorf("site requires a display name and a positive Gitea Manager ID")
	}
	u, err := url.Parse(s.Spec.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("site URL must be an HTTP(S) URL without credentials, query or fragment")
	}
	if s.Spec.CABundle != "" {
		if u.Scheme != "https" {
			return fmt.Errorf("site CA bundle requires an HTTPS URL")
		}
		if _, err := certificates.ParseCABundle(s.Spec.CABundle); err != nil {
			return err
		}
	}
	refs := append([]ResourceReference{s.Spec.Credential, s.Spec.Gateway}, s.Spec.Templates...)
	refs = append(refs, s.Spec.Caches...)
	for _, ref := range refs {
		if len(validation.IsDNS1123Subdomain(ref.Name)) != 0 || ref.UID == "" {
			return fmt.Errorf("resource references require a valid name and UID")
		}
	}
	if len(s.Spec.Templates) == 0 || len(s.Spec.Templates) > 64 {
		return fmt.Errorf("site must select between 1 and 64 templates")
	}
	if s.Spec.StartupConcurrency < 1 || s.Spec.StartupConcurrency > 64 || s.Spec.CleanupConcurrency < 1 || s.Spec.CleanupConcurrency > 64 {
		return fmt.Errorf("startup and cleanup concurrency must be between 1 and 64")
	}
	if len(s.Spec.Quota) != 0 {
		quotaResources := map[corev1.ResourceName]bool{
			corev1.ResourcePods:                   true,
			corev1.ResourcePersistentVolumeClaims: true,
			corev1.ResourceRequestsCPU:            true,
			corev1.ResourceRequestsMemory:         true,
			corev1.ResourceRequestsStorage:        true,
		}
		for name := range quotaResources {
			if q := s.Spec.Quota[name]; q.Sign() <= 0 {
				return fmt.Errorf("site quota %s must be positive", name)
			}
		}
		for name, quantity := range s.Spec.Quota {
			if quotaResources[name] {
				continue
			}
			resourceName := strings.TrimPrefix(string(name), "requests.")
			value, exact := quantity.AsInt64()
			if resourceName == string(name) || !strings.Contains(resourceName, "/") || len(validation.IsQualifiedName(resourceName)) != 0 || !exact || value <= 0 {
				return fmt.Errorf("site device quota %s must be a positive integer extended resource request", name)
			}
		}
	}
	for _, target := range s.Spec.Upstreams {
		if _, err := netip.ParsePrefix(target.CIDR); err != nil || len(target.Ports) == 0 {
			return fmt.Errorf("upstream requires a CIDR and explicit ports")
		}
		for _, port := range target.Ports {
			if port < 1 || port > 65535 {
				return fmt.Errorf("upstream port must be between 1 and 65535")
			}
		}
	}
	return ValidateObjectSize(s)
}

func (t *EnvironmentTemplate) Validate() error {
	if !environmentNamePattern.MatchString(t.Name) || len(t.Spec.Description) > 255 {
		return fmt.Errorf("environment requires a lowercase DNS label name of at most 63 characters and a description of at most 255 characters")
	}
	if err := t.Spec.Runtime.Validate(); err != nil {
		return err
	}
	return ValidateObjectSize(t)
}

func (c *CodespaceComponent) Validate() error {
	if len(c.Name) > 50 || len(validation.IsDNS1123Label(c.Name)) != 0 || strings.TrimSpace(c.Spec.DisplayName) == "" || len(c.Spec.DisplayName) > 100 {
		return fmt.Errorf("component requires a valid name and display name")
	}
	if len(c.Spec.Resources.Claims) != 0 {
		return fmt.Errorf("component dynamic resource claims are not supported")
	}
	for _, list := range []corev1.ResourceList{c.Spec.Resources.Requests, c.Spec.Resources.Limits} {
		for name, quantity := range list {
			if quantity.Sign() < 0 {
				return fmt.Errorf("component resource %s must not be negative", name)
			}
		}
	}
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		request, limit := c.Spec.Resources.Requests[name], c.Spec.Resources.Limits[name]
		if request.Sign() <= 0 || limit.Sign() <= 0 || request.Cmp(limit) > 0 {
			return fmt.Errorf("component %s request and limit must be positive, with request no greater than limit", name)
		}
	}
	switch c.Spec.Role {
	case "gateway":
		if c.Spec.Gateway == nil || c.Spec.Cache != nil {
			return fmt.Errorf("gateway component requires only Gateway settings")
		}
		if err := validateGatewayComponent(c.Spec.Gateway); err != nil {
			return err
		}
	case "cache":
		if c.Spec.Cache == nil || c.Spec.Gateway != nil {
			return fmt.Errorf("cache component requires only Cache settings")
		}
		if err := validateCacheComponent(c.Spec.Cache); err != nil {
			return err
		}
	default:
		return fmt.Errorf("component role must be gateway or cache")
	}
	return ValidateObjectSize(c)
}

func validateGatewayComponent(gateway *GatewayComponentSpec) error {
	if gateway.Replicas < 1 || gateway.Replicas > 64 {
		return fmt.Errorf("gateway replicas must be between 1 and 64")
	}
	endpoint, err := url.Parse(gateway.EndpointBaseURL)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Path != "" && endpoint.Path != "/" {
		return fmt.Errorf("gateway endpoint base URL must be an HTTP(S) origin")
	}
	if len(gateway.TrustedProxyCIDRs) > 64 {
		return fmt.Errorf("gateway supports at most 64 trusted proxy CIDRs")
	}
	for _, prefix := range gateway.TrustedProxyCIDRs {
		if _, err := netip.ParsePrefix(prefix); err != nil {
			return fmt.Errorf("gateway trusted proxy CIDR %q is invalid", prefix)
		}
	}
	if err := validateGatewayHTTPExposure(gateway, endpoint); err != nil {
		return err
	}
	return validateGatewaySSHExposure(gateway)
}

func validateCacheComponent(cache *CacheComponentSpec) error {
	if err := validateOutboundProxy(cache.Proxy); err != nil {
		return err
	}
	switch cache.Storage.Driver {
	case "filesystem":
		volumeSize, err := resource.ParseQuantity(cache.Storage.VolumeSize)
		if err != nil || volumeSize.Sign() <= 0 {
			return fmt.Errorf("filesystem cache requires a positive volume size")
		}
		if cache.Storage.StorageClassName != "" && len(validation.IsDNS1123Subdomain(cache.Storage.StorageClassName)) != 0 {
			return fmt.Errorf("cache storage class name is invalid")
		}
		maxSize, maxErr := dockerunits.RAMInBytes(cache.MaxSize)
		minFreeSpace, minErr := dockerunits.RAMInBytes(cache.Storage.MinFreeSpace)
		if maxErr != nil || minErr != nil || maxSize <= 0 || minFreeSpace <= 0 {
			return fmt.Errorf("filesystem cache sizes must be positive byte quantities")
		}
		if maxSize > volumeSize.Value()-minFreeSpace {
			return fmt.Errorf("cache maximum size and minimum free space must fit within the volume")
		}
		if cache.Storage.S3 != (CacheS3Storage{}) {
			return fmt.Errorf("filesystem cache cannot select S3 storage settings")
		}
	case "s3":
		if cache.Storage.VolumeSize != "" || cache.Storage.StorageClassName != "" || cache.Storage.MinFreeSpace != "" {
			return fmt.Errorf("S3 cache cannot select filesystem storage settings")
		}
	default:
		return fmt.Errorf("cache storage driver must be filesystem or s3")
	}
	upstreams := make(map[string]configpkg.RuntimeCacheUpstreamConfig, len(cache.Upstreams))
	for host, upstream := range cache.Upstreams {
		upstreams[host] = configpkg.RuntimeCacheUpstreamConfig{Allow: upstream.Allow}
	}
	runtimeConfig := configpkg.CacheConfig{
		ID: "cache", Name: "cache", Revision: 1, Enabled: cache.Enabled, Listen: "127.0.0.1:8443",
		MaxSize: cache.MaxSize, MaxAge: configpkg.Duration(0), GCInterval: configpkg.Duration(0), Upstreams: upstreams,
		Storage: configpkg.CacheStorageConfig{
			Driver: cache.Storage.Driver, Path: "cache", MinFreeSpace: cache.Storage.MinFreeSpace,
			S3: configpkg.CacheS3Config{
				Endpoint: cache.Storage.S3.Endpoint, Region: cache.Storage.S3.Region, Bucket: cache.Storage.S3.Bucket,
				Prefix: cache.Storage.S3.Prefix, ForcePathStyle: cache.Storage.S3.ForcePathStyle,
			},
		},
	}
	maxAge, err := time.ParseDuration(cache.MaxAge)
	if err != nil {
		return fmt.Errorf("invalid cache maximum age: %w", err)
	}
	gcInterval, err := time.ParseDuration(cache.GCInterval)
	if err != nil {
		return fmt.Errorf("invalid cache cleanup interval: %w", err)
	}
	runtimeConfig.MaxAge, runtimeConfig.GCInterval = configpkg.Duration(maxAge), configpkg.Duration(gcInterval)
	if err := runtimeConfig.Validate(); err != nil {
		return fmt.Errorf("invalid cache configuration: %w", err)
	}
	return nil
}

func validateOutboundProxy(proxy *OutboundProxy) error {
	if proxy == nil {
		return nil
	}
	if proxy.HTTPProxy == "" && proxy.HTTPSProxy == "" && proxy.SOCKSProxy == "" {
		return fmt.Errorf("outbound proxy requires at least one proxy URL")
	}
	for name, value := range map[string]string{"HTTP": proxy.HTTPProxy, "HTTPS": proxy.HTTPSProxy} {
		if value == "" {
			continue
		}
		if err := validateProxyURL(value, "http", "https"); err != nil {
			return fmt.Errorf("%s proxy: %w", name, err)
		}
	}
	if proxy.SOCKSProxy != "" {
		if err := validateProxyURL(proxy.SOCKSProxy, "socks5", "socks5h"); err != nil {
			return fmt.Errorf("SOCKS proxy: %w", err)
		}
	}
	if len(proxy.NoProxy) > maxNoProxyEntries {
		return fmt.Errorf("outbound proxy supports at most %d bypass entries", maxNoProxyEntries)
	}
	seen := make(map[string]struct{}, len(proxy.NoProxy))
	for _, value := range proxy.NoProxy {
		if value == "" || len(value) > maxNoProxyEntry || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\r\n,") {
			return fmt.Errorf("proxy bypass entry %q is invalid", value)
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("proxy bypass entry %q is duplicated", value)
		}
		seen[key] = struct{}{}
	}
	if len(proxy.CABundle) > maxProxyCABundle {
		return fmt.Errorf("proxy CA bundle exceeds %d bytes", maxProxyCABundle)
	}
	if proxy.CABundle != "" {
		if _, err := certificates.ParseCABundle(proxy.CABundle); err != nil {
			return fmt.Errorf("proxy CA bundle: %w", err)
		}
	}
	return nil
}

func validateProxyURL(value string, schemes ...string) error {
	if len(value) > maxProxyURL || value != strings.TrimSpace(value) {
		return fmt.Errorf("URL contains surrounding whitespace")
	}
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || u.Port() == "" || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("URL must contain a host and port without a path, query, or fragment")
	}
	port, err := strconv.ParseUint(u.Port(), 10, 16)
	if err != nil || port == 0 {
		return fmt.Errorf("URL port must be between 1 and 65535")
	}
	for _, scheme := range schemes {
		if u.Scheme == scheme {
			return nil
		}
	}
	return fmt.Errorf("URL scheme must be %s", strings.Join(schemes, " or "))
}

func validateGatewayHTTPExposure(gateway *GatewayComponentSpec, endpoint *url.URL) error {
	switch gateway.HTTPExposure.Mode {
	case "external":
		if gateway.HTTPExposure.Ingress != nil {
			return fmt.Errorf("external HTTP exposure cannot include Ingress settings")
		}
	case "ingress":
		ingress := gateway.HTTPExposure.Ingress
		if ingress == nil || len(validation.IsDNS1123Subdomain(ingress.ClassName)) != 0 {
			return fmt.Errorf("managed HTTP exposure requires a valid IngressClass")
		}
		hostname := strings.ToLower(endpoint.Hostname())
		if net.ParseIP(hostname) != nil || len(validation.IsDNS1123Subdomain(hostname)) != 0 {
			return fmt.Errorf("managed Ingress requires a DNS endpoint hostname")
		}
		if endpoint.Port() != "" {
			return fmt.Errorf("managed Ingress uses the standard HTTP or HTTPS port")
		}
		switch ingress.TLS.Source {
		case "none":
			if endpoint.Scheme != "http" || ingress.TLS.SecretName != "" || ingress.TLS.Issuer != nil {
				return fmt.Errorf("plain HTTP Ingress requires an HTTP endpoint and no TLS reference")
			}
		case "secret":
			if endpoint.Scheme != "https" || len(validation.IsDNS1123Subdomain(ingress.TLS.SecretName)) != 0 || ingress.TLS.Issuer != nil {
				return fmt.Errorf("HTTPS Ingress TLS Secret selection is invalid")
			}
		case "certManager":
			issuer := ingress.TLS.Issuer
			if endpoint.Scheme != "https" || ingress.TLS.SecretName != "" || issuer == nil || issuer.Kind != "Issuer" && issuer.Kind != "ClusterIssuer" || len(validation.IsDNS1123Subdomain(issuer.Name)) != 0 {
				return fmt.Errorf("HTTPS Ingress cert-manager issuer selection is invalid")
			}
		default:
			return fmt.Errorf("ingress TLS source must be none, secret or certManager")
		}
	default:
		return fmt.Errorf("gateway HTTP exposure must be external or ingress")
	}
	return nil
}

func validateGatewaySSHExposure(gateway *GatewayComponentSpec) error {
	_, port, err := net.SplitHostPort(gateway.SSHExternalAddress)
	if err != nil {
		return fmt.Errorf("gateway SSH external address must use host:port")
	}
	switch gateway.SSHExposure.Mode {
	case "external":
	case "loadBalancer":
		if port != "22" {
			return fmt.Errorf("managed SSH LoadBalancer requires external port 22")
		}
	default:
		return fmt.Errorf("gateway SSH exposure must be external or loadBalancer")
	}
	return nil
}

func (r EnvironmentConfiguration) Validate() error {
	if r.Isolation != IsolationKata && r.Isolation != IsolationSysbox && r.Isolation != IsolationNative {
		return fmt.Errorf("runtime isolation must be kata, sysbox or native")
	}
	if len(validation.IsDNS1123Subdomain(r.StorageClassName)) != 0 {
		return fmt.Errorf("storage class name is required")
	}
	if r.Isolation == IsolationNative {
		if r.RuntimeClassName != "" {
			return fmt.Errorf("native isolation uses the cluster default runtime")
		}
	} else if len(validation.IsDNS1123Subdomain(r.RuntimeClassName)) != 0 {
		return fmt.Errorf("runtime class name is required for %s isolation", r.Isolation)
	}
	if r.AccessMode != corev1.ReadWriteOncePod && r.AccessMode != corev1.ReadWriteOnce {
		return fmt.Errorf("storage access mode must be ReadWriteOncePod or a verified ReadWriteOnce combination")
	}
	if len(r.Resources.Claims) != 0 {
		return fmt.Errorf("runtime dynamic resource claims are not supported")
	}
	for _, resources := range []corev1.ResourceList{r.Resources.Requests, r.Resources.Limits} {
		for name := range resources {
			if name != corev1.ResourceCPU && name != corev1.ResourceMemory {
				return fmt.Errorf("runtime compute resources may contain only CPU and memory")
			}
		}
	}
	if size := r.Storage[corev1.ResourceStorage]; size.Sign() <= 0 || len(r.Storage) != 1 {
		return fmt.Errorf("runtime storage must specify a positive storage quantity")
	}
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		if request := r.Resources.Requests[name]; request.Sign() <= 0 {
			return fmt.Errorf("runtime %s request must be positive", name)
		}
	}
	cpuLimit, memoryLimit := r.Resources.Limits[corev1.ResourceCPU], r.Resources.Limits[corev1.ResourceMemory]
	if (cpuLimit.Sign() > 0) != (memoryLimit.Sign() > 0) {
		return fmt.Errorf("runtime CPU and memory limits must be configured together")
	}
	if cpuLimit.Sign() > 0 {
		for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			request, limit := r.Resources.Requests[name], r.Resources.Limits[name]
			if request.Cmp(limit) > 0 {
				return fmt.Errorf("runtime %s request must not exceed its limit", name)
			}
		}
	} else if len(r.Resources.Limits) != 0 {
		return fmt.Errorf("runtime resource limits may contain only positive CPU and memory limits")
	}
	if r.GitSSHKeyType != "ed25519" && r.GitSSHKeyType != "rsa-4096" {
		return fmt.Errorf("git SSH key type must be ed25519 or rsa-4096")
	}
	if r.Isolation == IsolationNative {
		if r.Native == nil {
			return fmt.Errorf("native isolation requires native runtime settings")
		}
		if err := validateNativeConfiguration(r.Native); err != nil {
			return err
		}
	} else if r.Native != nil {
		return fmt.Errorf("native runtime settings require native isolation")
	}
	if err := validateOutboundProxy(r.Proxy); err != nil {
		return err
	}
	return r.DevContainer.Validate()
}

func validateNativeConfiguration(configuration *runtimeplatform.NativeConfiguration) error {
	for label, capabilities := range map[string][]corev1.Capability{
		"runtime":       configuration.RuntimeCapAdd,
		"Dev Container": configuration.DevContainerCapAdd,
	} {
		seen := make(map[corev1.Capability]struct{}, len(capabilities))
		for _, capability := range capabilities {
			if !capabilityPattern.MatchString(string(capability)) {
				return fmt.Errorf("native %s capability %q is invalid", label, capability)
			}
			if _, exists := seen[capability]; exists {
				return fmt.Errorf("native %s capability %q is duplicated", label, capability)
			}
			seen[capability] = struct{}{}
		}
	}
	resources := make(map[corev1.ResourceName]struct{}, len(configuration.DevicePlugins))
	targets := map[string]struct{}{}
	environments := map[string]corev1.ResourceName{}
	for _, device := range configuration.DevicePlugins {
		if len(validation.IsQualifiedName(string(device.ResourceName))) != 0 || !strings.Contains(string(device.ResourceName), "/") {
			return fmt.Errorf("native device resource %q must be a Kubernetes extended resource name", device.ResourceName)
		}
		if device.Count < 1 {
			return fmt.Errorf("native device resource %q count must be positive", device.ResourceName)
		}
		if _, exists := resources[device.ResourceName]; exists {
			return fmt.Errorf("native device resource %q is duplicated", device.ResourceName)
		}
		resources[device.ResourceName] = struct{}{}
		if device.CDI == nil && len(device.HostAccess.Devices) == 0 && len(device.HostAccess.Mounts) == 0 && len(device.HostAccess.ForwardEnvironment) == 0 {
			return fmt.Errorf("native device resource %q requires a propagation method", device.ResourceName)
		}
		if device.CDI != nil {
			if !cdiKindPattern.MatchString(device.CDI.Kind) || !environmentPattern.MatchString(device.CDI.DeviceIDEnvironment) {
				return fmt.Errorf("native device resource %q has invalid CDI settings", device.ResourceName)
			}
			if strings.HasPrefix(device.CDI.DeviceIDEnvironment, "CODESPACE_") {
				return fmt.Errorf("native device resource %q uses a platform-reserved CDI allocation environment %q", device.ResourceName, device.CDI.DeviceIDEnvironment)
			}
			if owner, exists := environments[device.CDI.DeviceIDEnvironment]; exists {
				return fmt.Errorf("native device environment %q is shared by resources %q and %q", device.CDI.DeviceIDEnvironment, owner, device.ResourceName)
			}
			environments[device.CDI.DeviceIDEnvironment] = device.ResourceName
		}
		if err := validateHostAccess(device.ResourceName, device.HostAccess, targets, environments); err != nil {
			return err
		}
	}
	return nil
}

func validateHostAccess(resourceName corev1.ResourceName, hostAccess runtimeplatform.HostAccess, targets map[string]struct{}, environments map[string]corev1.ResourceName) error {
	for _, device := range hostAccess.Devices {
		if !canonicalAbsolutePath(device.Source) || !canonicalAbsolutePath(device.Target) || device.Permissions == "" || strings.Trim(device.Permissions, "rwm") != "" {
			return fmt.Errorf("native device resource %q has an invalid device mapping", resourceName)
		}
		permissions := map[rune]struct{}{}
		for _, permission := range device.Permissions {
			permissions[permission] = struct{}{}
		}
		if len(device.Permissions) != len(permissions) {
			return fmt.Errorf("native device resource %q has duplicated device permissions", resourceName)
		}
		if _, exists := targets[device.Target]; exists {
			return fmt.Errorf("native device target %q is duplicated", device.Target)
		}
		if platformMountTarget(device.Target) {
			return fmt.Errorf("native device target %q is reserved for the Codespace runtime", device.Target)
		}
		targets[device.Target] = struct{}{}
	}
	for _, mount := range hostAccess.Mounts {
		if !canonicalAbsolutePath(mount.Source) || !canonicalAbsolutePath(mount.Target) {
			return fmt.Errorf("native device resource %q has an invalid mount mapping", resourceName)
		}
		if pathWithin(mount.Source, platformRuntimePaths) {
			return fmt.Errorf("native host mount source %q is reserved for the Codespace runtime", mount.Source)
		}
		if _, exists := targets[mount.Target]; exists {
			return fmt.Errorf("native device target %q is duplicated", mount.Target)
		}
		if platformMountTarget(mount.Target) {
			return fmt.Errorf("native device target %q is reserved for the Codespace runtime", mount.Target)
		}
		targets[mount.Target] = struct{}{}
	}
	for _, name := range hostAccess.ForwardEnvironment {
		if !environmentPattern.MatchString(name) {
			return fmt.Errorf("native device resource %q has invalid environment name %q", resourceName, name)
		}
		if _, reserved := platformEnvironmentNames[name]; reserved || strings.HasPrefix(name, "CODESPACE_") {
			return fmt.Errorf("native device environment %q is reserved for the Codespace runtime", name)
		}
		if owner, exists := environments[name]; exists {
			return fmt.Errorf("native device environment %q is shared by resources %q and %q", name, owner, resourceName)
		}
		environments[name] = resourceName
	}
	return nil
}

func platformMountTarget(target string) bool {
	return pathWithin(target, platformMountTargets)
}

func pathWithin(path string, reservedPaths []string) bool {
	for _, reserved := range reservedPaths {
		if path == reserved || strings.HasPrefix(path, reserved+"/") {
			return true
		}
	}
	return false
}

func canonicalAbsolutePath(value string) bool {
	return filepath.IsAbs(value) && filepath.Clean(value) == value && value != "/"
}

func (r RuntimeConfiguration) Validate() error {
	if err := r.EnvironmentConfiguration.Validate(); err != nil {
		return err
	}
	image, err := reference.ParseNormalizedNamed(r.Image)
	if err != nil {
		return fmt.Errorf("invalid platform image: %w", err)
	}
	if _, tagged := image.(reference.Tagged); !tagged {
		if _, digested := image.(reference.Digested); !digested {
			return fmt.Errorf("platform image must include an explicit tag or digest")
		}
	}
	return nil
}

func (c *Codespace) Validate(managementNamespace string) error {
	namespace, err := SiteNamespace(c.Spec.Site.Name, managementNamespace)
	if err != nil || c.Namespace != namespace || c.Spec.Site.UID == "" {
		return fmt.Errorf("codespace namespace does not match its site")
	}
	id, err := uuid.Parse(c.Spec.RuntimeUUID)
	if err != nil || id[6]>>4 != 4 || id.String() != c.Spec.RuntimeUUID {
		return fmt.Errorf("runtime UUID must be a canonical UUID v4")
	}
	if c.Spec.CodespaceID <= 0 || c.Spec.Operation.Version <= 0 || !environmentNamePattern.MatchString(c.Spec.EnvironmentTag) {
		return fmt.Errorf("codespace identity, operation version and environment tag are required")
	}
	switch c.Spec.Operation.Type {
	case "create", "resume":
		if err := c.Spec.Runtime.Validate(); err != nil {
			return err
		}
	case "stop", "delete", "abort_create", "abort_resume":
		// Cleanup is authorized by resource identities even when startup settings
		// are unusable; requiring them here would strand retained user data.
	default:
		return fmt.Errorf("invalid codespace operation")
	}
	return ValidateObjectSize(c)
}

// ValidateObjectSize also runs before status updates, where recovery records grow.
func ValidateObjectSize(object any) error {
	data, err := json.Marshal(object)
	if err != nil {
		return err
	}
	if len(data) > MaxObjectBytes {
		return fmt.Errorf("codespace resource exceeds the %d-byte limit", MaxObjectBytes)
	}
	return nil
}
