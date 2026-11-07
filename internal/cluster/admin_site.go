// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"gitea.dev/codespace/internal/controlplane"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const pendingSiteLabel = "codespace.gitea.dev/pending-site"

type adminSiteWrite struct {
	adminWriteIdentity
	Spec              adminSiteSpec `json:"spec"`
	RegistrationToken string        `json:"registrationToken,omitempty"`
}

type adminSiteSpec struct {
	DisplayName        string                   `json:"displayName"`
	URL                string                   `json:"url"`
	CABundle           string                   `json:"caBundle,omitempty"`
	Enabled            bool                     `json:"enabled"`
	AcceptCreates      bool                     `json:"acceptCreates"`
	StartupConcurrency int32                    `json:"startupConcurrency"`
	CleanupConcurrency int32                    `json:"cleanupConcurrency"`
	Templates          []api.ResourceReference  `json:"templates"`
	Gateway            api.ResourceReference    `json:"gateway"`
	Caches             []api.ResourceReference  `json:"caches,omitempty"`
	Quota              corev1.ResourceList      `json:"quota"`
	Upstreams          []api.NetworkDestination `json:"upstreams,omitempty"`
}

// adminSiteSpec exposes only fields controlled by administrators. Registration
// assigns the Manager identity and credential reference on the resource.
func newAdminSiteSpec(spec api.GiteaSiteSpec) adminSiteSpec {
	if spec.Quota == nil {
		spec.Quota = corev1.ResourceList{}
	}
	return adminSiteSpec{
		DisplayName:        spec.DisplayName,
		URL:                spec.URL,
		CABundle:           spec.CABundle,
		Enabled:            spec.Enabled,
		AcceptCreates:      spec.AcceptCreates,
		StartupConcurrency: spec.StartupConcurrency,
		CleanupConcurrency: spec.CleanupConcurrency,
		Templates:          spec.Templates,
		Gateway:            spec.Gateway,
		Caches:             spec.Caches,
		Quota:              spec.Quota,
		Upstreams:          spec.Upstreams,
	}
}

func (spec adminSiteSpec) resourceSpec() api.GiteaSiteSpec {
	return api.GiteaSiteSpec{
		DisplayName:        spec.DisplayName,
		URL:                spec.URL,
		CABundle:           spec.CABundle,
		Enabled:            spec.Enabled,
		AcceptCreates:      spec.AcceptCreates,
		StartupConcurrency: spec.StartupConcurrency,
		CleanupConcurrency: spec.CleanupConcurrency,
		Templates:          spec.Templates,
		Gateway:            spec.Gateway,
		Caches:             spec.Caches,
		Quota:              spec.Quota,
		Upstreams:          spec.Upstreams,
	}
}

func (s *AdminServer) saveSite(ctx context.Context, site *api.GiteaSite, body adminSiteWrite, creating bool) error {
	previous := site.DeepCopy()
	site.Spec = body.Spec.resourceSpec()
	if !creating {
		if body.RegistrationToken != "" {
			return apierrors.NewBadRequest("registration tokens are accepted only when creating a site")
		}
		site.Spec.ManagerID = previous.Spec.ManagerID
		site.Spec.Credential = previous.Spec.Credential
		if err := site.Validate(s.Namespace); err != nil {
			return apierrors.NewBadRequest(err.Error())
		}
		if err := s.checkSiteReferences(ctx, site); err != nil {
			return err
		}
		connection, err := newSiteManagerConnection(ctx, s.Client, s.Namespace, site, s.RegistrationHTTPClient)
		if err != nil {
			return err
		}
		defer connection.close()
		checked, err := connection.client.CheckManager(ctx, &codespacev1.CheckManagerRequest{ProtocolVersion: controlplane.ProtocolVersion})
		if err != nil {
			return apierrors.NewBadRequest("Gitea Manager connection check failed: " + err.Error())
		}
		canonical, err := controlplane.CanonicalGiteaURL(checked.GiteaWebUrl)
		if err != nil {
			return apierrors.NewBadRequest(err.Error())
		}
		if previous.Status.CanonicalURL != "" && previous.Status.CanonicalURL != canonical {
			return apierrors.NewBadRequest("Gitea Manager connection resolves to a different site identity")
		}
		return s.Client.Update(ctx, site)
	}
	if len(body.RegistrationToken) != 40 {
		return apierrors.NewBadRequest("a new site requires a valid registration token")
	}
	site.Spec.ManagerID = 1
	site.Spec.Credential = api.ResourceReference{Name: "pending", UID: "pending"}
	if err := site.Validate(s.Namespace); err != nil {
		return apierrors.NewBadRequest(err.Error())
	}
	if err := s.checkSiteReferences(ctx, site); err != nil {
		return err
	}
	httpClient, err := giteaHTTPClient(s.RegistrationHTTPClient, site.Spec.CABundle)
	if err != nil {
		return apierrors.NewBadRequest(err.Error())
	}
	registration, err := controlplane.NewManagerRegistrationClient(httpClient, strings.TrimRight(site.Spec.URL, "/")+"/api/codespace").RegisterManager(ctx, &codespacev1.RegisterManagerRequest{
		ProtocolVersion: controlplane.ProtocolVersion,
		Token:           body.RegistrationToken,
		Name:            site.Spec.DisplayName,
	})
	if err != nil {
		switch connect.CodeOf(err) {
		case connect.CodeInvalidArgument, connect.CodeUnauthenticated, connect.CodeFailedPrecondition:
			return apierrors.NewBadRequest("Gitea rejected the Manager registration: " + err.Error())
		case connect.CodeUnavailable, connect.CodeDeadlineExceeded:
			return apierrors.NewServiceUnavailable("Gitea Manager registration is unavailable")
		default:
			return fmt.Errorf("register Gitea Manager: %w", err)
		}
	}
	if registration.GetManagerId() <= 0 || len(registration.GetManagerSecret()) < 32 || len(registration.GetManagerSecret()) > 4096 || registration.GetManagerName() == "" {
		return fmt.Errorf("register Gitea Manager: incomplete response")
	}
	if _, err := controlplane.CanonicalGiteaURL(registration.GetGiteaWebUrl()); err != nil {
		return fmt.Errorf("register Gitea Manager: %w", err)
	}
	site.Spec.ManagerID = registration.GetManagerId()
	site.Spec.DisplayName = registration.GetManagerName()
	if err := site.Validate(s.Namespace); err != nil {
		return fmt.Errorf("register Gitea Manager: invalid registered identity: %w", err)
	}
	credential := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "site-credential-", Namespace: s.Namespace, Labels: map[string]string{pendingSiteLabel: site.Name}},
		Data:       map[string][]byte{"managerSecret": []byte(registration.GetManagerSecret())},
	}
	if err := s.Client.Create(ctx, credential); err != nil {
		return err
	}
	site.Spec.Credential = api.ResourceReference{Name: credential.Name, UID: credential.UID}
	if err := s.Client.Create(ctx, site); err != nil {
		// A timeout may have committed the reference. Reconciliation removes only
		// credentials that are positively known to be unreferenced.
		return err
	}
	return bindSiteCredential(ctx, s.Client, s.Namespace, site)
}

func (s *AdminServer) checkSiteReferences(ctx context.Context, site *api.GiteaSite) error {
	seen := make(map[types.UID]bool)
	for _, ref := range site.Spec.Templates {
		var template api.EnvironmentTemplate
		if err := s.Client.Get(ctx, types.NamespacedName{Name: ref.Name}, &template); err != nil {
			return err
		}
		if template.UID != ref.UID || !template.DeletionTimestamp.IsZero() || seen[ref.UID] {
			return apierrors.NewBadRequest("template reference is stale or duplicated")
		}
		seen[ref.UID] = true
		if err := validateProxyDestination(site.Spec.Upstreams, template.Spec.Runtime.Proxy); err != nil {
			return apierrors.NewBadRequest(fmt.Sprintf("template %s: %v", template.Name, err))
		}
	}
	refs := append([]api.ResourceReference{site.Spec.Gateway}, site.Spec.Caches...)
	for index, ref := range refs {
		role := "cache"
		if index == 0 {
			role = "gateway"
		}
		var component api.CodespaceComponent
		if err := s.Client.Get(ctx, types.NamespacedName{Name: ref.Name}, &component); err != nil {
			return err
		}
		if component.UID != ref.UID || !component.DeletionTimestamp.IsZero() || component.Spec.Role != role {
			return apierrors.NewBadRequest("component reference is stale or has the wrong role")
		}
	}
	return nil
}

func bindSiteCredential(ctx context.Context, reader client.Client, namespace string, site *api.GiteaSite) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var credential corev1.Secret
		if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: site.Spec.Credential.Name}, &credential); err != nil {
			return err
		}
		if credential.UID != site.Spec.Credential.UID || !credential.DeletionTimestamp.IsZero() {
			return fmt.Errorf("site credential identity changed")
		}
		if credential.Labels[SiteUIDLabel] == string(site.UID) {
			return nil
		}
		if credential.Labels[pendingSiteLabel] != site.Name || credential.Labels[SiteUIDLabel] != "" || len(credential.OwnerReferences) != 0 {
			return fmt.Errorf("site credential ownership conflicts")
		}
		delete(credential.Labels, pendingSiteLabel)
		credential.Labels[SiteUIDLabel] = string(site.UID)
		credential.OwnerReferences = []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "GiteaSite", Name: site.Name, UID: site.UID}}
		return reader.Update(ctx, &credential)
	})
}

func (s *AdminServer) pruneCredentials(ctx context.Context) error {
	var sites api.GiteaSiteList
	if err := s.Client.List(ctx, &sites); err != nil {
		return err
	}
	used := make(map[types.UID]bool, len(sites.Items))
	for _, site := range sites.Items {
		used[site.Spec.Credential.UID] = true
	}
	var secrets corev1.SecretList
	if err := s.Client.List(ctx, &secrets, client.InNamespace(s.Namespace)); err != nil {
		return err
	}
	for _, secret := range secrets.Items {
		isSiteCredential := secret.Labels[pendingSiteLabel] != "" || secret.Labels[SiteUIDLabel] != ""
		if !isSiteCredential {
			continue
		}
		validPrefix := strings.HasPrefix(secret.Name, "site-credential-")
		if !validPrefix || used[secret.UID] || time.Since(secret.CreationTimestamp.Time) < time.Hour {
			continue
		}
		// A fresh live list protects references committed during the cleanup pass.
		if err := s.Client.List(ctx, &sites); err != nil {
			return err
		}
		referenced := false
		for _, site := range sites.Items {
			if site.Spec.Credential.UID == secret.UID {
				referenced = true
				break
			}
		}
		if !referenced {
			if err := s.Client.Delete(ctx, &secret, client.Preconditions{UID: &secret.UID}); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}
