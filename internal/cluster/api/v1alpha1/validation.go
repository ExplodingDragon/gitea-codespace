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

	"github.com/distribution/reference"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
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
	for _, name := range []corev1.ResourceName{corev1.ResourcePods, corev1.ResourceRequestsCPU, corev1.ResourceRequestsMemory, corev1.ResourceLimitsCPU, corev1.ResourceLimitsMemory, corev1.ResourceRequestsStorage, corev1.ResourcePersistentVolumeClaims} {
		if q := s.Spec.Quota[name]; q.Sign() <= 0 {
			return fmt.Errorf("site quota %s must be positive", name)
		}
	}
	for name, q := range s.Spec.Quota {
		if q.Sign() < 0 {
			return fmt.Errorf("site quota %s must not be negative", name)
		}
	}
	if s.Spec.ContainerLimits.Type != corev1.LimitTypeContainer {
		return fmt.Errorf("site LimitRange must target containers")
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

func (r RuntimeConfiguration) Validate() error {
	if r.Isolation != "kata" && r.Isolation != "sysbox" {
		return fmt.Errorf("runtime isolation must be kata or sysbox")
	}
	if len(validation.IsDNS1123Subdomain(r.RuntimeClassName)) != 0 || len(validation.IsDNS1123Subdomain(r.StorageClassName)) != 0 {
		return fmt.Errorf("runtime and storage class names are required")
	}
	image, err := reference.ParseNormalizedNamed(r.Image)
	if err != nil {
		return fmt.Errorf("invalid runtime image: %w", err)
	}
	if _, ok := image.(reference.Digested); !ok {
		return fmt.Errorf("runtime image must be pinned by digest")
	}
	if r.AccessMode != corev1.ReadWriteOncePod && r.AccessMode != corev1.ReadWriteOnce {
		return fmt.Errorf("storage access mode must be ReadWriteOncePod or a verified ReadWriteOnce combination")
	}
	if r.VolumeMode != corev1.PersistentVolumeFilesystem && r.VolumeMode != corev1.PersistentVolumeBlock {
		return fmt.Errorf("storage volume mode must be Filesystem or Block")
	}
	if r.Isolation == "kata" && r.VolumeMode != corev1.PersistentVolumeBlock {
		return fmt.Errorf("kata isolation requires a raw Block volume so Docker data is mounted inside the guest")
	}
	if r.Isolation == "sysbox" && r.VolumeMode != corev1.PersistentVolumeFilesystem {
		return fmt.Errorf("sysbox isolation requires a Filesystem volume")
	}
	if len(r.Resources.Claims) != 0 {
		return fmt.Errorf("runtime dynamic resource claims are not supported")
	}
	if size := r.Storage[corev1.ResourceStorage]; size.Sign() <= 0 || len(r.Storage) != 1 {
		return fmt.Errorf("runtime storage must specify a positive storage quantity")
	}
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		request, limit := r.Resources.Requests[name], r.Resources.Limits[name]
		if request.Sign() <= 0 || limit.Sign() <= 0 || request.Cmp(limit) > 0 {
			return fmt.Errorf("runtime %s request and limit must be positive, with request no greater than limit", name)
		}
	}
	if strings.TrimSpace(r.CodeServerVersion) == "" {
		return fmt.Errorf("runtime requires a code-server version")
	}
	if r.GitSSHKeyType != "ed25519" && r.GitSSHKeyType != "rsa-4096" {
		return fmt.Errorf("git SSH key type must be ed25519 or rsa-4096")
	}
	return nil
}

func (c *Codespace) Validate(managementNamespace string) error {
	namespace, err := SiteNamespace(c.Spec.Site.Name, managementNamespace)
	if err != nil || c.Namespace != namespace || c.Spec.Site.UID == "" {
		return fmt.Errorf("codespace namespace does not match its site")
	}
	id, err := uuid.Parse(c.Spec.RuntimeUUID)
	if err != nil || id.Version() != 4 || id.String() != c.Spec.RuntimeUUID {
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
