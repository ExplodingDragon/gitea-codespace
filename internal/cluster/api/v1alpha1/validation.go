// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"uuid"

	"github.com/distribution/reference"
	dockerunits "github.com/docker/go-units"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

const MaxObjectBytes = 1024 * 1024

var tagPattern = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

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
		for name := range s.Spec.Quota {
			if !quotaResources[name] {
				return fmt.Errorf("site quota %s is not supported", name)
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
	if len(validation.IsDNS1123Subdomain(t.Name)) != 0 || !tagPattern.MatchString(t.Spec.Tag) || len(t.Spec.Description) > 255 {
		return fmt.Errorf("invalid environment name, tag or description")
	}
	if err := t.Spec.Runtime.Validate(); err != nil {
		return err
	}
	return ValidateObjectSize(t)
}

func (c *CodespaceComponent) Validate() error {
	if len(validation.IsDNS1123Label(c.Name)) != 0 || strings.TrimSpace(c.Spec.DisplayName) == "" || len(c.Spec.DisplayName) > 100 {
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
		parent := c.Spec.Gateway.Parent
		if parent.Name == "" {
			if parent.Namespace != "" || parent.HTTPSectionName != "" || parent.SSHSectionName != "" {
				return fmt.Errorf("gateway parent name is required when listener settings are configured")
			}
		} else {
			for _, name := range []string{parent.Name, parent.Namespace, parent.HTTPSectionName} {
				if len(validation.IsDNS1123Subdomain(name)) != 0 {
					return fmt.Errorf("gateway parent and HTTP listener names are required")
				}
			}
			if parent.SSHSectionName != "" && len(validation.IsDNS1123Subdomain(parent.SSHSectionName)) != 0 {
				return fmt.Errorf("gateway SSH listener name is invalid")
			}
		}
	case "cache":
		if c.Spec.Cache == nil || c.Spec.Gateway != nil {
			return fmt.Errorf("cache component requires only Cache settings")
		}
		switch c.Spec.Cache.Storage.Driver {
		case "filesystem":
			volumeSize, err := resource.ParseQuantity(c.Spec.Cache.Storage.VolumeSize)
			if err != nil || volumeSize.Sign() <= 0 {
				return fmt.Errorf("filesystem cache requires a positive volume size")
			}
			if c.Spec.Cache.Storage.StorageClassName != "" && len(validation.IsDNS1123Subdomain(c.Spec.Cache.Storage.StorageClassName)) != 0 {
				return fmt.Errorf("cache storage class name is invalid")
			}
			maxSize, maxErr := dockerunits.RAMInBytes(c.Spec.Cache.MaxSize)
			minFreeSpace, minErr := dockerunits.RAMInBytes(c.Spec.Cache.Storage.MinFreeSpace)
			if maxErr != nil || minErr != nil || maxSize <= 0 || minFreeSpace <= 0 {
				return fmt.Errorf("filesystem cache sizes must be positive byte quantities")
			}
			if maxSize > volumeSize.Value()-minFreeSpace {
				return fmt.Errorf("cache maximum size and minimum free space must fit within the volume")
			}
			if c.Spec.Cache.Storage.S3 != (CacheS3Storage{}) {
				return fmt.Errorf("filesystem cache cannot select S3 storage settings")
			}
		case "s3":
			if c.Spec.Cache.Storage.VolumeSize != "" || c.Spec.Cache.Storage.StorageClassName != "" || c.Spec.Cache.Storage.MinFreeSpace != "" {
				return fmt.Errorf("S3 cache cannot select filesystem storage settings")
			}
		default:
			return fmt.Errorf("cache storage driver must be filesystem or s3")
		}
	default:
		return fmt.Errorf("component role must be gateway or cache")
	}
	return ValidateObjectSize(c)
}

func (r EnvironmentConfiguration) Validate() error {
	if r.Isolation != IsolationKata && r.Isolation != IsolationSysbox {
		return fmt.Errorf("runtime isolation must be kata or sysbox")
	}
	if len(validation.IsDNS1123Subdomain(r.RuntimeClassName)) != 0 || len(validation.IsDNS1123Subdomain(r.StorageClassName)) != 0 {
		return fmt.Errorf("runtime and storage class names are required")
	}
	if r.AccessMode != corev1.ReadWriteOncePod && r.AccessMode != corev1.ReadWriteOnce {
		return fmt.Errorf("storage access mode must be ReadWriteOncePod or a verified ReadWriteOnce combination")
	}
	if len(r.Resources.Claims) != 0 {
		return fmt.Errorf("runtime dynamic resource claims are not supported")
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
	return r.DevContainer.Validate()
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
	if c.Spec.CodespaceID <= 0 || c.Spec.Operation.Version <= 0 || !tagPattern.MatchString(c.Spec.EnvironmentTag) {
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
