// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	configpkg "gitea.dev/codespace/internal/config"
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
)

const pendingSiteLabel = "codespace.gitea.dev/pending-site"

const pendingComponentLabel = "codespace.gitea.dev/pending-component"

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

type adminWrite struct {
	Name            string          `json:"name"`
	UID             types.UID       `json:"uid"`
	ResourceVersion string          `json:"resourceVersion"`
	Spec            json.RawMessage `json:"spec"`
	ManagerSecret   string          `json:"managerSecret,omitempty"`
	Verification    string          `json:"verification,omitempty"`
	S3AccessKey     string          `json:"s3AccessKey,omitempty"`
	S3SecretKey     string          `json:"s3SecretKey,omitempty"`
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

type adminComponentSpec struct {
	Role        string                   `json:"role"`
	DisplayName string                   `json:"displayName"`
	Gateway     *configpkg.GatewayConfig `json:"gateway,omitempty"`
	Cache       *configpkg.CacheConfig   `json:"cache,omitempty"`
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
	var body adminWrite
	if !adminDecode(w, r, &body) {
		return
	}
	if r.Method == http.MethodPost {
		name = body.Name
		if body.UID != "" || body.ResourceVersion != "" {
			adminJSON(w, http.StatusBadRequest, map[string]string{"error": "new resources must not specify an existing UID or revision"})
			return
		}
	}
	if len(validation.IsDNS1123Subdomain(name)) != 0 || body.Name != name {
		adminJSON(w, http.StatusBadRequest, map[string]string{"error": "a valid, matching resource name is required"})
		return
	}
	var object client.Object
	switch kind {
	case "sites":
		object = &api.GiteaSite{}
	case "environments":
		object = &api.EnvironmentTemplate{}
	default:
		object = &corev1.ConfigMap{}
		object.SetNamespace(s.Namespace)
	}
	object.SetName(name)
	if r.Method != http.MethodPost {
		if err := s.Client.Get(r.Context(), client.ObjectKeyFromObject(object), object); err != nil {
			adminError(w, err)
			return
		}
		if body.UID == "" || body.ResourceVersion == "" || object.GetUID() != body.UID || object.GetResourceVersion() != body.ResourceVersion || !object.GetDeletionTimestamp().IsZero() {
			adminJSON(w, http.StatusConflict, map[string]string{"error": "resource changed; reload before saving"})
			return
		}
	}
	if r.Method == http.MethodDelete {
		if err := s.checkDeletion(r.Context(), object); err != nil {
			adminJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		if err := s.Client.Delete(r.Context(), object, client.Preconditions{UID: &body.UID, ResourceVersion: &body.ResourceVersion}); err != nil {
			adminError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var err error
	switch current := object.(type) {
	case *api.GiteaSite:
		err = s.saveSite(r.Context(), current, body, r.Method == http.MethodPost)
	case *api.EnvironmentTemplate:
		err = s.saveTemplate(r.Context(), current, body, r.Method == http.MethodPost)
	case *corev1.ConfigMap:
		err = s.saveComponent(r.Context(), current, body, r.Method == http.MethodPost)
	}
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
	var body adminWrite
	if !adminDecode(w, r, &body) {
		return
	}
	var component corev1.ConfigMap
	if err := s.Client.Get(r.Context(), types.NamespacedName{Namespace: s.Namespace, Name: name}, &component); err != nil {
		adminError(w, err)
		return
	}
	if body.Name != name || body.UID == "" || body.ResourceVersion == "" || component.UID != body.UID || component.ResourceVersion != body.ResourceVersion || !component.DeletionTimestamp.IsZero() {
		adminJSON(w, http.StatusConflict, map[string]string{"error": "component changed; reload before rotating its SSH host key"})
		return
	}
	if component.Labels[ComponentLabel] != "gateway" {
		adminJSON(w, http.StatusBadRequest, map[string]string{"error": "only Gateway components have an SSH host key"})
		return
	}
	oldSecretName, oldSecretUID := component.Data["sshHostKeySecret"], types.UID(component.Data["sshHostKeySecretUID"])
	secret, err := newGatewayHostKeySecret(component.Name, component.Namespace)
	if err != nil {
		adminError(w, err)
		return
	}
	if err := s.Client.Create(r.Context(), secret); err != nil {
		adminError(w, err)
		return
	}
	component.Data["sshHostKeySecret"], component.Data["sshHostKeySecretUID"] = secret.Name, string(secret.UID)
	if err := s.Client.Update(r.Context(), &component); err != nil {
		adminError(w, err)
		return
	}
	if err := bindComponentCredential(r.Context(), s.Client, &component); err != nil {
		adminError(w, err)
		return
	}
	if oldSecretName != "" && oldSecretUID != "" {
		oldSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: oldSecretName, Namespace: component.Namespace}}
		if err := s.Client.Delete(r.Context(), oldSecret, client.Preconditions{UID: &oldSecretUID}); err != nil && !apierrors.IsNotFound(err) {
			adminError(w, err)
			return
		}
	}
	adminJSON(w, http.StatusOK, map[string]string{"name": name})
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
		var list corev1.ConfigMapList
		if err := s.Client.List(ctx, &list, client.InNamespace(s.Namespace)); err != nil {
			return nil, err
		}
		for _, item := range list.Items {
			role := item.Labels[ComponentLabel]
			if role != "gateway" && role != "cache" {
				continue
			}
			spec, err := componentSpecFromConfigMap(&item)
			if err != nil {
				return nil, err
			}
			status, err := s.componentStatus(ctx, &item)
			if err != nil {
				return nil, err
			}
			appendItem(&item, spec, status)
		}
	}
	return items, nil
}

func (s *AdminServer) componentStatus(ctx context.Context, component *corev1.ConfigMap) (adminComponentStatus, error) {
	status := adminComponentStatus{DesiredReplicas: 1}
	var deployment appsv1.Deployment
	err := s.Client.Get(ctx, types.NamespacedName{Namespace: component.Namespace, Name: component.Name}, &deployment)
	if err != nil && !apierrors.IsNotFound(err) {
		return status, err
	}
	if err == nil && metav1.IsControlledBy(&deployment, component) {
		status.DesiredReplicas = ptr.Deref(deployment.Spec.Replicas, int32(1))
		status.ReadyReplicas = deployment.Status.ReadyReplicas
		status.Available = status.DesiredReplicas > 0 && status.ReadyReplicas == status.DesiredReplicas
	}
	if component.Labels[ComponentLabel] != "cache" {
		return status, nil
	}
	deploymentAvailable := status.Available
	status.Available = false
	var lease coordinationv1.Lease
	err = s.Client.Get(ctx, types.NamespacedName{Namespace: component.Namespace, Name: "cache-owner-" + component.Name}, &lease)
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

func (s *AdminServer) saveSite(ctx context.Context, site *api.GiteaSite, body adminWrite, creating bool) error {
	previous := site.DeepCopy()
	var spec adminSiteSpec
	if err := decodeAdminSpec(body.Spec, &spec); err != nil {
		return err
	}
	managerID, err := strconv.ParseInt(spec.ManagerID, 10, 64)
	if err != nil || managerID <= 0 {
		return apierrors.NewBadRequest("Manager ID must be a positive integer")
	}
	site.Spec = spec.GiteaSiteSpec
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

func decodeAdminSpec(raw json.RawMessage, target any) error {
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return apierrors.NewBadRequest("resource specification must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return apierrors.NewBadRequest("invalid resource specification")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return apierrors.NewBadRequest("resource specification must contain one JSON object")
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
		var component corev1.ConfigMap
		if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: ref.Name}, &component); err != nil {
			return err
		}
		if component.UID != ref.UID || !component.DeletionTimestamp.IsZero() || component.Labels[ComponentLabel] != role {
			return apierrors.NewBadRequest("component reference is stale or has the wrong role")
		}
	}
	return nil
}

func (s *AdminServer) saveTemplate(ctx context.Context, template *api.EnvironmentTemplate, body adminWrite, creating bool) error {
	var spec api.EnvironmentTemplateSpec
	if err := decodeAdminSpec(body.Spec, &spec); err != nil {
		return err
	}
	template.Spec = spec
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

func componentSpecFromConfigMap(component *corev1.ConfigMap) (adminComponentSpec, error) {
	role := component.Labels[ComponentLabel]
	spec := adminComponentSpec{Role: role, DisplayName: component.Data["displayName"]}
	switch role {
	case "gateway":
		var gateway configpkg.GatewayConfig
		if err := json.Unmarshal([]byte(component.Data["config"]), &gateway); err != nil {
			return adminComponentSpec{}, fmt.Errorf("gateway %s has invalid configuration", component.Name)
		}
		spec.Gateway = &gateway
	case "cache":
		var cache configpkg.CacheConfig
		if err := json.Unmarshal([]byte(component.Data["config"]), &cache); err != nil {
			return adminComponentSpec{}, fmt.Errorf("cache %s has invalid configuration", component.Name)
		}
		cache.Storage.S3.AccessKey = ""
		cache.Storage.S3.SecretKey = ""
		if err := cache.Validate(); err != nil {
			return adminComponentSpec{}, fmt.Errorf("cache %s has invalid configuration: %w", component.Name, err)
		}
		spec.Cache = &cache
	default:
		return adminComponentSpec{}, fmt.Errorf("component %s has an invalid role", component.Name)
	}
	return spec, nil
}

func (s *AdminServer) saveComponent(ctx context.Context, component *corev1.ConfigMap, body adminWrite, creating bool) error {
	var spec adminComponentSpec
	if err := decodeAdminSpec(body.Spec, &spec); err != nil {
		return err
	}
	if spec.Role != "gateway" && spec.Role != "cache" || strings.TrimSpace(spec.DisplayName) == "" || len(spec.DisplayName) > 100 {
		return apierrors.NewBadRequest("component requires a role and display name")
	}
	if !creating && component.Labels[ComponentLabel] != spec.Role {
		return apierrors.NewBadRequest("component role is immutable")
	}
	data := map[string]string{"displayName": strings.TrimSpace(spec.DisplayName)}
	var secret *corev1.Secret
	switch spec.Role {
	case "gateway":
		if spec.Gateway == nil || spec.Cache != nil {
			return apierrors.NewBadRequest("Gateway configuration is required")
		}
		if err := spec.Gateway.Validate(); err != nil {
			return apierrors.NewBadRequest(err.Error())
		}
		encoded, err := json.Marshal(spec.Gateway)
		if err != nil {
			return err
		}
		data["config"] = string(encoded)
		data["url"], data["sshAddress"] = spec.Gateway.HTTP.PublicURL, spec.Gateway.SSH.PublicAddr
		data["httpListen"], data["sshListen"] = spec.Gateway.HTTP.Listen, spec.Gateway.SSH.Listen
		data["sessionTTLMilliseconds"] = strconv.FormatInt(time.Duration(spec.Gateway.Sessions.TTL).Milliseconds(), 10)
		data["sessionIdleTimeoutMilliseconds"] = strconv.FormatInt(time.Duration(spec.Gateway.Sessions.IdleTimeout).Milliseconds(), 10)
		data["revalidateIntervalMilliseconds"] = strconv.FormatInt(time.Duration(spec.Gateway.Sessions.RevalidateInterval).Milliseconds(), 10)
		data["maxSessionsPerCodespace"] = strconv.Itoa(spec.Gateway.Sessions.MaxPerCodespace)
		data["maxSessionsPerUser"] = strconv.Itoa(spec.Gateway.Sessions.MaxPerUser)
		data["maxInflight"] = strconv.Itoa(spec.Gateway.Limits.MaxInflightTotal)
		data["maxInflightPerSession"] = strconv.Itoa(spec.Gateway.Limits.MaxInflightPerSession)
		data["maxChannelsPerSSHConnection"] = strconv.Itoa(spec.Gateway.SSH.MaxChannelsPerConnection)
		if creating {
			secret, err = newGatewayHostKeySecret(component.Name, s.Namespace)
			if err != nil {
				return err
			}
		}
	case "cache":
		if spec.Cache == nil || spec.Gateway != nil {
			return apierrors.NewBadRequest("Cache configuration is required")
		}
		revision := int64(1)
		if !creating {
			var current configpkg.CacheConfig
			if err := json.Unmarshal([]byte(component.Data["config"]), &current); err != nil || current.Revision < 1 || current.Revision == math.MaxInt64 {
				return apierrors.NewBadRequest("stored Cache configuration revision is invalid")
			}
			revision = current.Revision + 1
		}
		spec.Cache.ID, spec.Cache.Name, spec.Cache.Revision = component.Name, data["displayName"], revision
		if err := spec.Cache.Validate(); err != nil {
			return apierrors.NewBadRequest(err.Error())
		}
		accessKey, secretKey := spec.Cache.Storage.S3.AccessKey, spec.Cache.Storage.S3.SecretKey
		if body.S3AccessKey != "" || body.S3SecretKey != "" {
			accessKey, secretKey = body.S3AccessKey, body.S3SecretKey
		}
		if (accessKey == "") != (secretKey == "") {
			return apierrors.NewBadRequest("S3 access key and secret key must be configured together")
		}
		spec.Cache.Storage.S3.AccessKey, spec.Cache.Storage.S3.SecretKey = "", ""
		encoded, err := json.Marshal(spec.Cache)
		if err != nil {
			return err
		}
		data["config"], data["url"] = string(encoded), spec.Cache.PublicURL
		if creating {
			registryKey, tokenKey := make([]byte, 32), make([]byte, 32)
			if _, err := rand.Read(registryKey); err != nil {
				return err
			}
			if _, err := rand.Read(tokenKey); err != nil {
				return err
			}
			secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{GenerateName: component.Name + "-cache-", Namespace: s.Namespace, Labels: map[string]string{pendingComponentLabel: component.Name}}, Data: map[string][]byte{"registryKey": registryKey, "tokenKey": tokenKey, "s3AccessKey": []byte(accessKey), "s3SecretKey": []byte(secretKey)}}
		} else if body.S3AccessKey != "" || body.S3SecretKey != "" {
			var current corev1.Secret
			if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: component.Data["secretName"]}, &current); err != nil {
				return err
			}
			if string(current.UID) != component.Data["secretUID"] || current.Labels[ComponentUIDLabel] != string(component.UID) {
				return apierrors.NewBadRequest("Cache Secret identity changed")
			}
			secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{GenerateName: component.Name + "-cache-", Namespace: s.Namespace, Labels: map[string]string{pendingComponentLabel: component.Name}}, Data: map[string][]byte{
				"registryKey": append([]byte(nil), current.Data["registryKey"]...), "tokenKey": append([]byte(nil), current.Data["tokenKey"]...),
				"s3AccessKey": []byte(accessKey), "s3SecretKey": []byte(secretKey),
			}}
		}
	}
	if secret != nil {
		if err := s.Client.Create(ctx, secret); err != nil {
			return err
		}
		if spec.Role == "gateway" {
			data["sshHostKeySecret"], data["sshHostKeySecretUID"] = secret.Name, string(secret.UID)
		} else {
			data["secretName"], data["secretUID"] = secret.Name, string(secret.UID)
		}
	}
	if creating {
		component.Namespace = s.Namespace
		component.Labels = map[string]string{ComponentLabel: spec.Role}
		component.Data = data
		if err := s.Client.Create(ctx, component); err != nil {
			return err
		}
	} else {
		for key := range component.Data {
			if key != "sshHostKeySecret" && key != "sshHostKeySecretUID" && key != "secretName" && key != "secretUID" {
				delete(component.Data, key)
			}
		}
		for key, value := range data {
			component.Data[key] = value
		}
		if err := s.Client.Update(ctx, component); err != nil {
			return err
		}
	}
	if secret == nil {
		return nil
	}
	return bindComponentCredential(ctx, s.Client, component)
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
		ObjectMeta: metav1.ObjectMeta{GenerateName: component + "-ssh-", Namespace: namespace, Labels: map[string]string{pendingComponentLabel: component}},
		Immutable:  ptr.To(true),
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
			return fmt.Errorf("delete the site's Codespaces in Gitea first")
		}
	case *corev1.ConfigMap:
		role := current.Labels[ComponentLabel]
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

func bindComponentCredential(ctx context.Context, reader client.Client, component *corev1.ConfigMap) error {
	secretName, secretUID := component.Data["secretName"], component.Data["secretUID"]
	if component.Labels[ComponentLabel] == "gateway" {
		secretName, secretUID = component.Data["sshHostKeySecret"], component.Data["sshHostKeySecretUID"]
	}
	if secretName == "" || secretUID == "" {
		return fmt.Errorf("component credential reference is missing")
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var credential corev1.Secret
		if err := reader.Get(ctx, types.NamespacedName{Namespace: component.Namespace, Name: secretName}, &credential); err != nil {
			return err
		}
		if string(credential.UID) != secretUID || !credential.DeletionTimestamp.IsZero() {
			return fmt.Errorf("component credential identity changed")
		}
		if credential.Labels[ComponentUIDLabel] == string(component.UID) {
			if len(credential.OwnerReferences) == 1 && credential.OwnerReferences[0].UID == component.UID {
				return nil
			}
			return fmt.Errorf("component credential ownership conflicts")
		}
		if credential.Labels[pendingComponentLabel] != component.Name || credential.Labels[ComponentUIDLabel] != "" || len(credential.OwnerReferences) != 0 {
			return fmt.Errorf("component credential ownership conflicts")
		}
		delete(credential.Labels, pendingComponentLabel)
		credential.Labels[ComponentUIDLabel] = string(component.UID)
		credential.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: component.Name, UID: component.UID}}
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
	var components corev1.ConfigMapList
	if err := s.Client.List(ctx, &components, client.InNamespace(s.Namespace)); err != nil {
		return err
	}
	for _, component := range components.Items {
		for _, field := range []string{"sshHostKeySecretUID", "secretUID"} {
			if uid := component.Data[field]; uid != "" {
				used[types.UID(uid)] = true
			}
		}
	}
	var secrets corev1.SecretList
	if err := s.Client.List(ctx, &secrets, client.InNamespace(s.Namespace)); err != nil {
		return err
	}
	for _, secret := range secrets.Items {
		isSiteCredential := secret.Labels[pendingSiteLabel] != "" || secret.Labels[SiteUIDLabel] != ""
		isComponentCredential := secret.Labels[pendingComponentLabel] != "" || secret.Labels[ComponentUIDLabel] != ""
		if !isSiteCredential && !isComponentCredential {
			continue
		}
		validPrefix := strings.HasPrefix(secret.Name, "site-credential-") || strings.Contains(secret.Name, "-ssh-") || strings.Contains(secret.Name, "-cache-")
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
			if err := s.Client.List(ctx, &components, client.InNamespace(s.Namespace)); err != nil {
				return err
			}
			for _, component := range components.Items {
				if types.UID(component.Data["sshHostKeySecretUID"]) == secret.UID || types.UID(component.Data["secretUID"]) == secret.UID {
					referenced = true
					break
				}
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
