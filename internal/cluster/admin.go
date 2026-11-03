// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const pendingSiteLabel = "codespace.gitea.dev/pending-site"

type adminResource struct {
	Name            string    `json:"name"`
	Namespace       string    `json:"namespace,omitempty"`
	UID             types.UID `json:"uid"`
	ResourceVersion string    `json:"resourceVersion"`
	Generation      int64     `json:"generation"`
	Deleting        bool      `json:"deleting"`
	Spec            any       `json:"spec"`
	Status          any       `json:"status,omitempty"`
}

type adminWriteIdentity struct {
	Name            string    `json:"name"`
	UID             types.UID `json:"uid"`
	ResourceVersion string    `json:"resourceVersion"`
}

type adminSiteWrite struct {
	adminWriteIdentity
	Spec          adminSiteSpec `json:"spec"`
	ManagerSecret string        `json:"managerSecret,omitempty"`
}

type adminTemplateWrite struct {
	adminWriteIdentity
	Spec         api.EnvironmentTemplateSpec `json:"spec"`
	Verification string                      `json:"verification,omitempty"`
}

type adminComponentWrite struct {
	adminWriteIdentity
	Spec        api.CodespaceComponentSpec `json:"spec"`
	S3AccessKey string                     `json:"s3AccessKey,omitempty"`
	S3SecretKey string                     `json:"s3SecretKey,omitempty"`
}

type adminRuntimeRecovery struct {
	UID             types.UID `json:"uid"`
	ResourceVersion string    `json:"resourceVersion"`
	PodUID          types.UID `json:"podUID"`
}

type adminSiteSpec struct {
	api.GiteaSiteSpec
	ManagerID string `json:"managerID"`
}

type adminComponentStatus struct {
	Available       bool              `json:"available"`
	ReadyReplicas   int32             `json:"readyReplicas"`
	DesiredReplicas int32             `json:"desiredReplicas"`
	LastHeartbeat   *metav1.MicroTime `json:"lastHeartbeat,omitempty"`
	CacheBytes      int64             `json:"cacheBytes,omitempty"`
	MirrorBytes     int64             `json:"mirrorBytes,omitempty"`
	CleanupResult   string            `json:"cleanupResult,omitempty"`
}

func (s *AdminServer) resources(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/admin/"), "/")
	if len(parts) == 3 && parts[0] == "runtimes" && r.Method == http.MethodDelete {
		s.deleteRuntime(w, r, parts[1], parts[2])
		return
	}
	if len(parts) == 4 && parts[0] == "runtimes" && parts[3] == "confirm-writer-stopped" {
		s.confirmRuntimeWriterStopped(w, r, parts[1], parts[2])
		return
	}
	if len(parts) == 3 && parts[0] == "components" && parts[2] == "rotate-ssh-host-key" {
		s.rotateGatewayHostKey(w, r, parts[1])
		return
	}
	if len(parts) > 2 || len(parts) == 0 {
		http.NotFound(w, r)
		return
	}
	kind, name := parts[0], ""
	if len(parts) == 2 {
		name = parts[1]
	}
	if kind != "sites" && kind != "environments" && kind != "runtimes" && kind != "components" {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet && name == "" {
		items, err := s.listResources(r.Context(), kind)
		if err != nil {
			adminError(w, err)
			return
		}
		adminJSON(w, http.StatusOK, items)
		return
	}
	if kind == "runtimes" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if (r.Method == http.MethodPost && name != "") || (r.Method != http.MethodPost && name == "") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodDelete {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var identity adminWriteIdentity
	var object client.Object
	var save func(context.Context, client.Object, bool) error
	switch kind {
	case "sites":
		var body adminSiteWrite
		if !adminDecode(w, r, &body) {
			return
		}
		identity = body.adminWriteIdentity
		object = &api.GiteaSite{}
		save = func(ctx context.Context, object client.Object, creating bool) error {
			return s.saveSite(ctx, object.(*api.GiteaSite), body, creating)
		}
	case "environments":
		var body adminTemplateWrite
		if !adminDecode(w, r, &body) {
			return
		}
		identity = body.adminWriteIdentity
		object = &api.EnvironmentTemplate{}
		save = func(ctx context.Context, object client.Object, creating bool) error {
			return s.saveTemplate(ctx, object.(*api.EnvironmentTemplate), body, creating)
		}
	case "components":
		var body adminComponentWrite
		if !adminDecode(w, r, &body) {
			return
		}
		identity = body.adminWriteIdentity
		object = &api.CodespaceComponent{}
		save = func(ctx context.Context, object client.Object, creating bool) error {
			return s.saveComponent(ctx, object.(*api.CodespaceComponent), body, creating)
		}
	}
	if r.Method == http.MethodPost {
		name = identity.Name
		if identity.UID != "" || identity.ResourceVersion != "" {
			adminJSON(w, http.StatusBadRequest, map[string]string{"error": "new resources must not specify an existing UID or revision"})
			return
		}
	}
	if len(validation.IsDNS1123Subdomain(name)) != 0 || identity.Name != name {
		adminJSON(w, http.StatusBadRequest, map[string]string{"error": "a valid, matching resource name is required"})
		return
	}
	object.SetName(name)
	if r.Method != http.MethodPost {
		if err := s.Client.Get(r.Context(), client.ObjectKeyFromObject(object), object); err != nil {
			adminError(w, err)
			return
		}
		if identity.UID == "" || identity.ResourceVersion == "" || object.GetUID() != identity.UID || object.GetResourceVersion() != identity.ResourceVersion || !object.GetDeletionTimestamp().IsZero() {
			adminJSON(w, http.StatusConflict, map[string]string{"error": "resource changed; reload before saving"})
			return
		}
	}
	if r.Method == http.MethodDelete {
		if err := s.checkDeletion(r.Context(), object); err != nil {
			adminJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		if err := s.Client.Delete(r.Context(), object, client.Preconditions{UID: &identity.UID, ResourceVersion: &identity.ResourceVersion}); err != nil {
			adminError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	err := save(r.Context(), object, r.Method == http.MethodPost)
	if err != nil {
		adminError(w, err)
		return
	}
	adminJSON(w, http.StatusOK, map[string]string{"name": name})
}

func (s *AdminServer) rotateGatewayHostKey(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost || len(validation.IsDNS1123Subdomain(name)) != 0 {
		http.NotFound(w, r)
		return
	}
	var body adminWriteIdentity
	if !adminDecode(w, r, &body) {
		return
	}
	var component api.CodespaceComponent
	if err := s.Client.Get(r.Context(), types.NamespacedName{Name: name}, &component); err != nil {
		adminError(w, err)
		return
	}
	if body.Name != name || body.UID == "" || body.ResourceVersion == "" || component.UID != body.UID || component.ResourceVersion != body.ResourceVersion || !component.DeletionTimestamp.IsZero() {
		adminJSON(w, http.StatusConflict, map[string]string{"error": "component changed; reload before rotating its SSH host key"})
		return
	}
	if component.Spec.Role != "gateway" {
		adminJSON(w, http.StatusBadRequest, map[string]string{"error": "only Gateway components have an SSH host key"})
		return
	}
	var secret corev1.Secret
	if err := s.Client.Get(r.Context(), types.NamespacedName{Namespace: s.Namespace, Name: component.Name + "-ssh-host-key"}, &secret); err != nil {
		adminError(w, err)
		return
	}
	if secret.Labels[ComponentUIDLabel] != string(component.UID) || !metav1.IsControlledBy(&secret, &component) || !secret.DeletionTimestamp.IsZero() {
		adminJSON(w, http.StatusConflict, map[string]string{"error": "Gateway SSH host key ownership changed"})
		return
	}
	generated, err := newGatewayHostKeySecret(secret.Name, secret.Namespace)
	if err != nil {
		adminError(w, err)
		return
	}
	secret.Data = generated.Data
	if err := s.Client.Update(r.Context(), &secret); err != nil {
		adminError(w, err)
		return
	}
	adminJSON(w, http.StatusOK, map[string]string{"name": name})
}

func (s *AdminServer) deleteRuntime(w http.ResponseWriter, r *http.Request, namespace, name string) {
	if len(validation.IsDNS1123Label(namespace)) != 0 || len(validation.IsDNS1123Subdomain(name)) != 0 {
		http.NotFound(w, r)
		return
	}
	var body adminRuntimeRecovery
	if !adminDecode(w, r, &body) {
		return
	}
	var runtime api.Codespace
	if err := s.Client.Get(r.Context(), types.NamespacedName{Namespace: namespace, Name: name}, &runtime); err != nil {
		adminError(w, err)
		return
	}
	if body.UID == "" || body.ResourceVersion == "" || runtime.UID != body.UID || runtime.ResourceVersion != body.ResourceVersion || runtime.Status.Pod.UID != body.PodUID {
		adminJSON(w, http.StatusConflict, map[string]string{"error": "runtime identity changed; reload before deleting"})
		return
	}
	var site api.GiteaSite
	if err := s.Client.Get(r.Context(), types.NamespacedName{Name: runtime.Spec.Site.Name}, &site); err != nil {
		adminError(w, err)
		return
	}
	expectedNamespace, err := api.SiteNamespace(site.Name, s.Namespace)
	if err != nil || site.UID != runtime.Spec.Site.UID || expectedNamespace != namespace || site.Spec.Enabled {
		adminJSON(w, http.StatusConflict, map[string]string{"error": "disable the owning site before cleaning up its runtime data"})
		return
	}
	runtime.Status.RecoveryAction = "delete"
	runtime.Status.Target = nil
	if err := s.Client.Status().Update(r.Context(), &runtime); err != nil {
		adminError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *AdminServer) confirmRuntimeWriterStopped(w http.ResponseWriter, r *http.Request, namespace, name string) {
	if r.Method != http.MethodPost || len(validation.IsDNS1123Label(namespace)) != 0 || len(validation.IsDNS1123Subdomain(name)) != 0 {
		http.NotFound(w, r)
		return
	}
	var body adminRuntimeRecovery
	if !adminDecode(w, r, &body) {
		return
	}
	var runtime api.Codespace
	if err := s.Client.Get(r.Context(), types.NamespacedName{Namespace: namespace, Name: name}, &runtime); err != nil {
		adminError(w, err)
		return
	}
	condition := meta.FindStatusCondition(runtime.Status.Conditions, "InfrastructureReady")
	if body.UID == "" || body.ResourceVersion == "" || body.PodUID == "" || runtime.UID != body.UID || runtime.ResourceVersion != body.ResourceVersion ||
		runtime.Status.Pod.UID != body.PodUID || !runtime.DeletionTimestamp.IsZero() || condition == nil || condition.Status != metav1.ConditionFalse ||
		(condition.Reason != "RecoveryRequired" && condition.Reason != "WriterUnconfirmed") {
		adminJSON(w, http.StatusConflict, map[string]string{"error": "runtime recovery state changed; reload before confirming"})
		return
	}
	var pod corev1.Pod
	err := s.Client.Get(r.Context(), types.NamespacedName{Namespace: namespace, Name: runtime.Status.Pod.Name}, &pod)
	if err == nil {
		if pod.UID != body.PodUID || !metav1.IsControlledBy(&pod, &runtime) || pod.DeletionTimestamp.IsZero() {
			adminJSON(w, http.StatusConflict, map[string]string{"error": "runtime Pod is still active or its identity changed"})
			return
		}
	} else if !apierrors.IsNotFound(err) {
		adminError(w, err)
		return
	}
	if runtime.Annotations == nil {
		runtime.Annotations = map[string]string{}
	}
	runtime.Annotations[WriterStoppedAnnotation] = string(body.PodUID)
	if err := s.Client.Update(r.Context(), &runtime); err != nil {
		adminError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *AdminServer) listResources(ctx context.Context, kind string) ([]adminResource, error) {
	items := []adminResource{}
	appendItem := func(object client.Object, spec, status any) {
		items = append(items, adminResource{Name: object.GetName(), Namespace: object.GetNamespace(), UID: object.GetUID(), ResourceVersion: object.GetResourceVersion(), Generation: object.GetGeneration(), Deleting: !object.GetDeletionTimestamp().IsZero(), Spec: spec, Status: status})
	}
	switch kind {
	case "sites":
		var list api.GiteaSiteList
		if err := s.Client.List(ctx, &list); err != nil {
			return nil, err
		}
		for _, item := range list.Items {
			appendItem(&item, adminSiteSpec{GiteaSiteSpec: item.Spec, ManagerID: strconv.FormatInt(item.Spec.ManagerID, 10)}, item.Status)
		}
	case "environments":
		var list api.EnvironmentTemplateList
		if err := s.Client.List(ctx, &list); err != nil {
			return nil, err
		}
		for _, item := range list.Items {
			appendItem(&item, item.Spec, item.Status)
		}
	case "runtimes":
		var list api.CodespaceList
		if err := s.Client.List(ctx, &list); err != nil {
			return nil, err
		}
		for _, item := range list.Items {
			// Operation payloads and raw metadata are not administration responses.
			appendItem(&item, struct {
				Site           api.ResourceReference `json:"site"`
				CodespaceID    string                `json:"codespaceID"`
				RuntimeUUID    string                `json:"runtimeUUID"`
				EnvironmentTag string                `json:"environmentTag"`
				Operation      string                `json:"operation"`
				Version        int64                 `json:"version"`
			}{item.Spec.Site, strconv.FormatInt(item.Spec.CodespaceID, 10), item.Spec.RuntimeUUID, item.Spec.EnvironmentTag, item.Spec.Operation.Type, item.Spec.Operation.Version}, struct {
				Bound      bool                 `json:"bound"`
				Pod        api.ObservedResource `json:"pod"`
				Volume     api.ObservedResource `json:"volume"`
				Ready      bool                 `json:"ready"`
				Conditions []metav1.Condition   `json:"conditions"`
			}{item.Status.Bound, item.Status.Pod, item.Status.Volume, item.Status.Target != nil && item.Status.Target.Ready, item.Status.Conditions})
		}
	case "components":
		var list api.CodespaceComponentList
		if err := s.Client.List(ctx, &list); err != nil {
			return nil, err
		}
		for _, item := range list.Items {
			status, err := s.componentStatus(ctx, &item)
			if err != nil {
				return nil, err
			}
			appendItem(&item, item.Spec, status)
		}
	}
	return items, nil
}

func (s *AdminServer) componentStatus(ctx context.Context, component *api.CodespaceComponent) (adminComponentStatus, error) {
	status := adminComponentStatus{DesiredReplicas: 1}
	var deployment appsv1.Deployment
	err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: component.Name}, &deployment)
	if err != nil && !apierrors.IsNotFound(err) {
		return status, err
	}
	if err == nil && metav1.IsControlledBy(&deployment, component) {
		status.DesiredReplicas = ptr.Deref(deployment.Spec.Replicas, int32(1))
		status.ReadyReplicas = deployment.Status.ReadyReplicas
		status.Available = status.DesiredReplicas > 0 && status.ReadyReplicas == status.DesiredReplicas
	}
	if component.Spec.Role != "cache" {
		return status, nil
	}
	deploymentAvailable := status.Available
	status.Available = false
	var lease coordinationv1.Lease
	err = s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: "cache-owner-" + component.Name}, &lease)
	if apierrors.IsNotFound(err) {
		return status, nil
	}
	if err != nil {
		return status, err
	}
	if lease.Labels[ComponentUIDLabel] != string(component.UID) || lease.Spec.RenewTime == nil {
		return status, nil
	}
	expires := lease.Spec.RenewTime.Add(time.Duration(ptr.Deref(lease.Spec.LeaseDurationSeconds, 0)) * time.Second)
	if time.Now().After(expires) {
		status.Available = false
		return status, nil
	}
	status.LastHeartbeat = lease.Spec.RenewTime
	status.Available = deploymentAvailable
	status.CacheBytes, _ = strconv.ParseInt(lease.Annotations["codespace.gitea.dev/cache-bytes"], 10, 64)
	status.MirrorBytes, _ = strconv.ParseInt(lease.Annotations["codespace.gitea.dev/mirror-bytes"], 10, 64)
	status.CleanupResult = lease.Annotations["codespace.gitea.dev/cleanup-result"]
	return status, nil
}

func (s *AdminServer) saveSite(ctx context.Context, site *api.GiteaSite, body adminSiteWrite, creating bool) error {
	previous := site.DeepCopy()
	managerID, err := strconv.ParseInt(body.Spec.ManagerID, 10, 64)
	if err != nil || managerID <= 0 {
		return apierrors.NewBadRequest("Manager ID must be a positive integer")
	}
	site.Spec = body.Spec.GiteaSiteSpec
	site.Spec.ManagerID = managerID
	if !creating && (site.Spec.ManagerID != previous.Spec.ManagerID || site.Spec.Credential != previous.Spec.Credential) {
		return apierrors.NewBadRequest("Manager identity and credential references are managed by the server")
	}
	if creating && (body.ManagerSecret == "" || site.Spec.Credential.Name != "" || site.Spec.Credential.UID != "") {
		return apierrors.NewBadRequest("a new site requires a Manager secret, not an existing credential reference")
	}
	if len(body.ManagerSecret) > 4096 || (body.ManagerSecret != "" && len(body.ManagerSecret) < 32) {
		return apierrors.NewBadRequest("invalid Manager secret length")
	}
	if creating {
		site.Spec.Credential = api.ResourceReference{Name: "pending", UID: "pending"}
	}
	if err := site.Validate(s.Namespace); err != nil {
		return apierrors.NewBadRequest(err.Error())
	}
	if err := s.checkSiteReferences(ctx, site); err != nil {
		return err
	}
	var credential *corev1.Secret
	if body.ManagerSecret != "" {
		credential = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{GenerateName: "site-credential-", Namespace: s.Namespace, Labels: map[string]string{pendingSiteLabel: site.Name}}, Data: map[string][]byte{"managerSecret": []byte(body.ManagerSecret)}}
		if !creating {
			credential.Labels = map[string]string{SiteUIDLabel: string(site.UID)}
			credential.OwnerReferences = []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "GiteaSite", Name: site.Name, UID: site.UID}}
		}
		if err := s.Client.Create(ctx, credential); err != nil {
			return err
		}
		site.Spec.Credential = api.ResourceReference{Name: credential.Name, UID: credential.UID}
	}
	if creating {
		err = s.Client.Create(ctx, site)
	} else {
		err = s.Client.Update(ctx, site)
	}
	// A timeout may have committed the reference. Reconciliation removes only
	// credentials that are positively known to be unreferenced.
	if err != nil {
		return err
	}
	if credential != nil {
		return bindSiteCredential(ctx, s.Client, s.Namespace, site)
	}
	return nil
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

func (s *AdminServer) saveTemplate(ctx context.Context, template *api.EnvironmentTemplate, body adminTemplateWrite, creating bool) error {
	template.Spec = body.Spec
	if err := template.Validate(); err != nil {
		return apierrors.NewBadRequest(err.Error())
	}
	if len(body.Verification) > 4096 {
		return apierrors.NewBadRequest("verification reference is too long")
	}
	var err error
	if creating {
		err = s.Client.Create(ctx, template)
	} else {
		err = s.Client.Update(ctx, template)
	}
	if err != nil {
		return err
	}
	if body.Verification != "" {
		template.Status.VerifiedGeneration, template.Status.Verification = template.Generation, body.Verification
		return s.Client.Status().Update(ctx, template)
	}
	return nil
}

func (s *AdminServer) saveComponent(ctx context.Context, component *api.CodespaceComponent, body adminComponentWrite, creating bool) error {
	previousRole := component.Spec.Role
	component.Spec = body.Spec
	component.Spec.DisplayName = strings.TrimSpace(component.Spec.DisplayName)
	if err := component.Validate(); err != nil {
		return apierrors.NewBadRequest(err.Error())
	}
	if !creating && previousRole != body.Spec.Role {
		return apierrors.NewBadRequest("component role is immutable")
	}
	if component.Spec.Role == "gateway" {
		if _, err := gatewayComponentConfig(component); err != nil {
			return apierrors.NewBadRequest(err.Error())
		}
		if component.Spec.Gateway.Parent.Name != "" {
			if err := validateGatewayParent(ctx, s.Client, component.Spec.Gateway.Parent); err != nil {
				return apierrors.NewBadRequest(err.Error())
			}
		}
	} else if _, err := cacheComponentConfig(component); err != nil {
		return apierrors.NewBadRequest(err.Error())
	}
	if (body.S3AccessKey == "") != (body.S3SecretKey == "") {
		return apierrors.NewBadRequest("S3 access key and secret key must be configured together")
	}
	var err error
	if creating {
		err = s.Client.Create(ctx, component)
	} else {
		err = s.Client.Update(ctx, component)
	}
	if err != nil || component.Spec.Role != "cache" || body.S3AccessKey == "" {
		return err
	}
	return s.saveCacheCredentials(ctx, component, body.S3AccessKey, body.S3SecretKey)
}

func (s *AdminServer) saveCacheCredentials(ctx context.Context, component *api.CodespaceComponent, accessKey, secretKey string) error {
	name := component.Name + "-cache"
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var secret corev1.Secret
		err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: name}, &secret)
		if apierrors.IsNotFound(err) {
			registryKey, tokenKey := make([]byte, 32), make([]byte, 32)
			if _, err := rand.Read(registryKey); err != nil {
				return err
			}
			if _, err := rand.Read(tokenKey); err != nil {
				return err
			}
			secret = corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace, Labels: map[string]string{ComponentUIDLabel: string(component.UID)}}, Data: map[string][]byte{"registryKey": registryKey, "tokenKey": tokenKey}}
			if err := controllerutil.SetControllerReference(component, &secret, s.Client.Scheme()); err != nil {
				return err
			}
			secret.Data["s3AccessKey"], secret.Data["s3SecretKey"] = []byte(accessKey), []byte(secretKey)
			if err := s.Client.Create(ctx, &secret); apierrors.IsAlreadyExists(err) {
				return apierrors.NewConflict(corev1.Resource("secrets"), name, err)
			} else {
				return err
			}
		}
		if err != nil {
			return err
		}
		if secret.Labels[ComponentUIDLabel] != string(component.UID) || !metav1.IsControlledBy(&secret, component) || !secret.DeletionTimestamp.IsZero() {
			return apierrors.NewConflict(corev1.Resource("secrets"), name, fmt.Errorf("cache Secret ownership changed"))
		}
		secret.Data["s3AccessKey"], secret.Data["s3SecretKey"] = []byte(accessKey), []byte(secretKey)
		return s.Client.Update(ctx, &secret)
	})
}

func newGatewayHostKeySecret(component, namespace string) (*corev1.Secret, error) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: component, Namespace: namespace},
		Data:       map[string][]byte{"hostKey": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})},
	}, nil
}

func (s *AdminServer) checkDeletion(ctx context.Context, object client.Object) error {
	switch current := object.(type) {
	case *api.EnvironmentTemplate:
		var sites api.GiteaSiteList
		if err := s.Client.List(ctx, &sites); err != nil {
			return fmt.Errorf("could not verify site references")
		}
		for _, site := range sites.Items {
			for _, ref := range site.Spec.Templates {
				if ref.UID == current.UID {
					return fmt.Errorf("template is selected by site %s", site.Name)
				}
			}
		}
	case *api.GiteaSite:
		if current.Spec.Enabled {
			return fmt.Errorf("disable the site before deleting it")
		}
		var runtimes api.CodespaceList
		namespace, err := api.SiteNamespace(current.Name, s.Namespace)
		if err != nil {
			return err
		}
		if err := s.Client.List(ctx, &runtimes, client.InNamespace(namespace)); err != nil {
			return fmt.Errorf("could not verify site runtimes")
		}
		if len(runtimes.Items) != 0 {
			return fmt.Errorf("delete the site's Codespaces in Gitea or clean up its runtime data here before deleting the site")
		}
	case *api.CodespaceComponent:
		role := current.Spec.Role
		if role != "gateway" && role != "cache" {
			return fmt.Errorf("resource is not a managed component")
		}
		var sites api.GiteaSiteList
		if err := s.Client.List(ctx, &sites); err != nil {
			return fmt.Errorf("could not verify site references")
		}
		for _, site := range sites.Items {
			if role == "gateway" && site.Spec.Gateway.UID == current.UID {
				return fmt.Errorf("gateway is selected by site %s", site.Name)
			}
			for _, reference := range site.Spec.Caches {
				if role == "cache" && reference.UID == current.UID {
					return fmt.Errorf("cache is selected by site %s", site.Name)
				}
			}
		}
		if role == "cache" {
			if current.Spec.Cache == nil {
				return fmt.Errorf("could not verify the cache configuration")
			}
			if current.Spec.Cache.Enabled {
				return fmt.Errorf("disable the cache before deleting it")
			}
			var lease coordinationv1.Lease
			err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: "cache-owner-" + current.Name}, &lease)
			if err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("could not verify the cache writer lease")
			}
			if err == nil && lease.Spec.RenewTime != nil && time.Now().Before(lease.Spec.RenewTime.Add(time.Duration(ptr.Deref(lease.Spec.LeaseDurationSeconds, 0))*time.Second)) {
				return fmt.Errorf("disable the cache and wait for its active writer lease to expire before deleting it")
			}
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
