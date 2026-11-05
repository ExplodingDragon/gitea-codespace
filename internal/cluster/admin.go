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
	"sort"
	"strconv"
	"strings"
	"time"
	"uuid"

	"connectrpc.com/connect/v2"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"gitea.dev/codespace/internal/controlplane"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
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

func newAdminResource(object client.Object, spec, status any) adminResource {
	return adminResource{
		Name:            object.GetName(),
		Namespace:       object.GetNamespace(),
		UID:             object.GetUID(),
		ResourceVersion: object.GetResourceVersion(),
		Generation:      object.GetGeneration(),
		Deleting:        !object.GetDeletionTimestamp().IsZero(),
		Spec:            spec,
		Status:          status,
	}
}

type adminWriteIdentity struct {
	Name            string    `json:"name"`
	UID             types.UID `json:"uid"`
	ResourceVersion string    `json:"resourceVersion"`
}

type adminSiteWrite struct {
	adminWriteIdentity
	Spec              adminSiteSpec `json:"spec"`
	RegistrationToken string        `json:"registrationToken,omitempty"`
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
		DisplayName: spec.DisplayName, URL: spec.URL, CABundle: spec.CABundle, Enabled: spec.Enabled, AcceptCreates: spec.AcceptCreates,
		StartupConcurrency: spec.StartupConcurrency, CleanupConcurrency: spec.CleanupConcurrency,
		Templates: spec.Templates, Gateway: spec.Gateway, Caches: spec.Caches, Quota: spec.Quota,
		Upstreams: spec.Upstreams,
	}
}

func (spec adminSiteSpec) resourceSpec() api.GiteaSiteSpec {
	return api.GiteaSiteSpec{
		DisplayName: spec.DisplayName, URL: spec.URL, CABundle: spec.CABundle, Enabled: spec.Enabled, AcceptCreates: spec.AcceptCreates,
		StartupConcurrency: spec.StartupConcurrency, CleanupConcurrency: spec.CleanupConcurrency,
		Templates: spec.Templates, Gateway: spec.Gateway, Caches: spec.Caches, Quota: spec.Quota,
		Upstreams: spec.Upstreams,
	}
}

type adminComponentStatus struct {
	Available                bool              `json:"available"`
	ReadyReplicas            int32             `json:"readyReplicas"`
	DesiredReplicas          int32             `json:"desiredReplicas"`
	HTTPExposureConfigured   bool              `json:"httpExposureConfigured,omitempty"`
	SSHExposureConfigured    bool              `json:"sshExposureConfigured,omitempty"`
	ServiceNamespace         string            `json:"serviceNamespace,omitempty"`
	ServiceName              string            `json:"serviceName,omitempty"`
	SSHLoadBalancerAddresses []string          `json:"sshLoadBalancerAddresses,omitempty"`
	LastHeartbeat            *metav1.MicroTime `json:"lastHeartbeat,omitempty"`
	CacheBytes               int64             `json:"cacheBytes,omitempty"`
	MirrorBytes              int64             `json:"mirrorBytes,omitempty"`
	CleanupResult            string            `json:"cleanupResult,omitempty"`
}

func (s *AdminServer) handleResources(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/admin/"), "/")
	if s.handleAdminAction(w, r, parts) {
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
			writeAdminError(w, r, err)
			return
		}
		writeAdminJSON(w, http.StatusOK, items)
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
	s.handleResourceWrite(w, r, kind, name)
}

func (s *AdminServer) handleAdminAction(w http.ResponseWriter, r *http.Request, parts []string) bool {
	if len(parts) == 1 && parts[0] == "cluster-options" && r.Method == http.MethodGet {
		options, err := s.clusterOptions(r.Context())
		if err != nil {
			writeAdminError(w, r, err)
			return true
		}
		writeAdminJSON(w, http.StatusOK, options)
		return true
	}
	if len(parts) == 3 && parts[0] == "runtimes" && r.Method == http.MethodDelete {
		s.deleteRuntime(w, r, parts[1], parts[2])
		return true
	}
	if len(parts) == 4 && parts[0] == "runtimes" && parts[3] == "confirm-writer-stopped" {
		s.confirmRuntimeWriterStopped(w, r, parts[1], parts[2])
		return true
	}
	if len(parts) == 3 && parts[0] == "components" && parts[2] == "rotate-ssh-host-key" {
		s.rotateGatewayHostKey(w, r, parts[1])
		return true
	}
	return false
}

func (s *AdminServer) handleResourceWrite(w http.ResponseWriter, r *http.Request, kind, name string) {
	var identity adminWriteIdentity
	var object client.Object
	var save func(context.Context, client.Object, bool) error
	var prefix string
	switch kind {
	case "sites":
		var body adminSiteWrite
		if !decodeAdminJSON(w, r, &body) {
			return
		}
		identity = body.adminWriteIdentity
		object = &api.GiteaSite{}
		save = func(ctx context.Context, object client.Object, creating bool) error {
			return s.saveSite(ctx, object.(*api.GiteaSite), body, creating)
		}
		prefix = "site-"
	case "environments":
		var body adminTemplateWrite
		if !decodeAdminJSON(w, r, &body) {
			return
		}
		identity = body.adminWriteIdentity
		object = &api.EnvironmentTemplate{}
		save = func(ctx context.Context, object client.Object, creating bool) error {
			return s.saveTemplate(ctx, object.(*api.EnvironmentTemplate), body, creating)
		}
		prefix = "environment-"
	case "components":
		var body adminComponentWrite
		if !decodeAdminJSON(w, r, &body) {
			return
		}
		identity = body.adminWriteIdentity
		object = &api.CodespaceComponent{}
		save = func(ctx context.Context, object client.Object, creating bool) error {
			return s.saveComponent(ctx, object.(*api.CodespaceComponent), body, creating)
		}
		prefix = "component-"
	}
	if r.Method == http.MethodPost {
		if identity.Name != "" {
			writeAdminProblem(w, r, http.StatusBadRequest, "invalid_request", "resource names are generated by the server")
			return
		}
		name = prefix + uuid.NewV4().String()
		identity.Name = name
		if identity.UID != "" || identity.ResourceVersion != "" {
			writeAdminProblem(w, r, http.StatusBadRequest, "invalid_request", "new resources must not specify an existing UID or revision")
			return
		}
	}
	if len(validation.IsDNS1123Subdomain(name)) != 0 || identity.Name != name {
		writeAdminProblem(w, r, http.StatusBadRequest, "invalid_request", "a valid, matching resource name is required")
		return
	}
	object.SetName(name)
	if r.Method != http.MethodPost {
		if err := s.Client.Get(r.Context(), client.ObjectKeyFromObject(object), object); err != nil {
			writeAdminError(w, r, err)
			return
		}
		if identity.UID == "" || identity.ResourceVersion == "" || object.GetUID() != identity.UID || object.GetResourceVersion() != identity.ResourceVersion || !object.GetDeletionTimestamp().IsZero() {
			writeAdminProblem(w, r, http.StatusConflict, "resource_conflict", "")
			return
		}
	}
	if r.Method == http.MethodDelete {
		if err := s.checkDeletion(r.Context(), object); err != nil {
			writeAdminProblem(w, r, http.StatusConflict, "resource_in_use", err.Error())
			return
		}
		if err := s.Client.Delete(r.Context(), object, client.Preconditions{UID: &identity.UID, ResourceVersion: &identity.ResourceVersion}); err != nil {
			writeAdminError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	err := save(r.Context(), object, r.Method == http.MethodPost)
	if err != nil {
		writeAdminError(w, r, err)
		return
	}
	var resource adminResource
	switch current := object.(type) {
	case *api.GiteaSite:
		resource = newAdminResource(current, newAdminSiteSpec(current.Spec), current.Status)
	case *api.EnvironmentTemplate:
		resource = newAdminResource(current, current.Spec, current.Status)
	case *api.CodespaceComponent:
		// Component status is a projection of Deployment and Lease state. The
		// controller has not observed this generation yet, so the write response
		// reports its initial state without adding fallible reads after the write.
		desired := int32(1)
		if current.Spec.Gateway != nil {
			desired = current.Spec.Gateway.Replicas
		}
		resource = newAdminResource(current, current.Spec, adminComponentStatus{DesiredReplicas: desired})
	}
	writeAdminJSON(w, http.StatusOK, resource)
}

func (s *AdminServer) rotateGatewayHostKey(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost || len(validation.IsDNS1123Subdomain(name)) != 0 {
		http.NotFound(w, r)
		return
	}
	var body adminWriteIdentity
	if !decodeAdminJSON(w, r, &body) {
		return
	}
	var component api.CodespaceComponent
	if err := s.Client.Get(r.Context(), types.NamespacedName{Name: name}, &component); err != nil {
		writeAdminError(w, r, err)
		return
	}
	if body.Name != name || body.UID == "" || body.ResourceVersion == "" || component.UID != body.UID || component.ResourceVersion != body.ResourceVersion || !component.DeletionTimestamp.IsZero() {
		writeAdminProblem(w, r, http.StatusConflict, "resource_conflict", "component changed; reload before rotating its SSH host key")
		return
	}
	if component.Spec.Role != "gateway" {
		writeAdminProblem(w, r, http.StatusBadRequest, "invalid_configuration", "only Gateway components have an SSH host key")
		return
	}
	var secret corev1.Secret
	if err := s.Client.Get(r.Context(), types.NamespacedName{Namespace: s.Namespace, Name: gatewayHostKeySecretName(component.Name)}, &secret); err != nil {
		writeAdminError(w, r, err)
		return
	}
	if secret.Labels[ComponentUIDLabel] != string(component.UID) || !metav1.IsControlledBy(&secret, &component) || !secret.DeletionTimestamp.IsZero() {
		writeAdminProblem(w, r, http.StatusConflict, "resource_conflict", "Gateway SSH host key ownership changed")
		return
	}
	generated, err := newGatewayHostKeySecret(secret.Name, secret.Namespace)
	if err != nil {
		writeAdminError(w, r, err)
		return
	}
	if secret.Data == nil {
		secret.Data = make(map[string][]byte)
	}
	secret.Data["hostKey"] = generated.Data["hostKey"]
	if err := s.Client.Update(r.Context(), &secret); err != nil {
		writeAdminError(w, r, err)
		return
	}
	writeAdminJSON(w, http.StatusOK, map[string]string{"name": name})
}

func (s *AdminServer) deleteRuntime(w http.ResponseWriter, r *http.Request, namespace, name string) {
	if len(validation.IsDNS1123Label(namespace)) != 0 || len(validation.IsDNS1123Subdomain(name)) != 0 {
		http.NotFound(w, r)
		return
	}
	var body adminRuntimeRecovery
	if !decodeAdminJSON(w, r, &body) {
		return
	}
	var runtime api.Codespace
	if err := s.Client.Get(r.Context(), types.NamespacedName{Namespace: namespace, Name: name}, &runtime); err != nil {
		writeAdminError(w, r, err)
		return
	}
	if body.UID == "" || body.ResourceVersion == "" || runtime.UID != body.UID || runtime.ResourceVersion != body.ResourceVersion || runtime.Status.Pod.UID != body.PodUID {
		writeAdminProblem(w, r, http.StatusConflict, "resource_conflict", "runtime identity changed; reload before deleting")
		return
	}
	var site api.GiteaSite
	if err := s.Client.Get(r.Context(), types.NamespacedName{Name: runtime.Spec.Site.Name}, &site); err != nil {
		writeAdminError(w, r, err)
		return
	}
	expectedNamespace, err := api.SiteNamespace(site.Name, s.Namespace)
	if err != nil || site.UID != runtime.Spec.Site.UID || expectedNamespace != namespace || site.Spec.Enabled {
		writeAdminProblem(w, r, http.StatusConflict, "resource_in_use", "disable the owning site before cleaning up its runtime data")
		return
	}
	operationVersion, podUID := runtime.Spec.Operation.Version, runtime.Status.Pod.UID
	if err := updateCodespaceStatus(r.Context(), s.Client, &runtime, func(current *api.Codespace) error {
		if current.Spec.Operation.Version != operationVersion || current.Status.Pod.UID != podUID {
			return errObservedResourceChanged
		}
		current.Status.RecoveryAction = "delete"
		current.Status.Target = nil
		return nil
	}); err != nil {
		writeAdminError(w, r, err)
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
	if !decodeAdminJSON(w, r, &body) {
		return
	}
	var runtime api.Codespace
	if err := s.Client.Get(r.Context(), types.NamespacedName{Namespace: namespace, Name: name}, &runtime); err != nil {
		writeAdminError(w, r, err)
		return
	}
	condition := meta.FindStatusCondition(runtime.Status.Conditions, "InfrastructureReady")
	if body.UID == "" || body.ResourceVersion == "" || body.PodUID == "" || runtime.UID != body.UID || runtime.ResourceVersion != body.ResourceVersion ||
		runtime.Status.Pod.UID != body.PodUID || !runtime.DeletionTimestamp.IsZero() || condition == nil || condition.Status != metav1.ConditionFalse ||
		(condition.Reason != "RecoveryRequired" && condition.Reason != "WriterUnconfirmed") {
		writeAdminProblem(w, r, http.StatusConflict, "resource_conflict", "runtime recovery state changed; reload before confirming")
		return
	}
	var pod corev1.Pod
	err := s.Client.Get(r.Context(), types.NamespacedName{Namespace: namespace, Name: runtime.Status.Pod.Name}, &pod)
	if err == nil {
		if pod.UID != body.PodUID || !metav1.IsControlledBy(&pod, &runtime) || pod.DeletionTimestamp.IsZero() {
			writeAdminProblem(w, r, http.StatusConflict, "resource_conflict", "runtime Pod is still active or its identity changed")
			return
		}
	} else if !apierrors.IsNotFound(err) {
		writeAdminError(w, r, err)
		return
	}
	if runtime.Annotations == nil {
		runtime.Annotations = map[string]string{}
	}
	runtime.Annotations[WriterStoppedAnnotation] = string(body.PodUID)
	if err := s.Client.Update(r.Context(), &runtime); err != nil {
		writeAdminError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *AdminServer) listResources(ctx context.Context, kind string) ([]adminResource, error) {
	items := []adminResource{}
	switch kind {
	case "sites":
		var list api.GiteaSiteList
		if err := s.Client.List(ctx, &list); err != nil {
			return nil, err
		}
		for _, item := range list.Items {
			items = append(items, newAdminResource(&item, newAdminSiteSpec(item.Spec), item.Status))
		}
	case "environments":
		var list api.EnvironmentTemplateList
		if err := s.Client.List(ctx, &list); err != nil {
			return nil, err
		}
		for _, item := range list.Items {
			items = append(items, newAdminResource(&item, item.Spec, item.Status))
		}
	case "runtimes":
		var list api.CodespaceList
		if err := s.Client.List(ctx, &list); err != nil {
			return nil, err
		}
		for _, item := range list.Items {
			// Operation payloads and raw metadata are not administration responses.
			items = append(items, newAdminResource(&item, struct {
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
			}{item.Status.Bound, item.Status.Pod, item.Status.Volume, item.Status.Target != nil && item.Status.Target.Ready, item.Status.Conditions}))
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
			items = append(items, newAdminResource(&item, item.Spec, status))
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
		if component.Spec.Role == "gateway" {
			status.Available = status.ReadyReplicas > 0
		} else {
			status.Available = status.ReadyReplicas == 1
		}
	}
	var service corev1.Service
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: component.Name}, &service); err != nil && !apierrors.IsNotFound(err) {
		return status, err
	} else if err == nil && metav1.IsControlledBy(&service, component) {
		status.ServiceNamespace, status.ServiceName = service.Namespace, service.Name
	}
	if component.Spec.Role != "cache" {
		status.HTTPExposureConfigured = meta.IsStatusConditionTrue(component.Status.Conditions, "HTTPExposureConfigured")
		status.SSHExposureConfigured = meta.IsStatusConditionTrue(component.Status.Conditions, "SSHExposureConfigured")
		var loadBalancer corev1.Service
		if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: component.Name + "-ssh"}, &loadBalancer); err != nil && !apierrors.IsNotFound(err) {
			return status, err
		} else if err == nil && metav1.IsControlledBy(&loadBalancer, component) {
			for _, ingress := range loadBalancer.Status.LoadBalancer.Ingress {
				if ingress.IP != "" {
					status.SSHLoadBalancerAddresses = append(status.SSHLoadBalancerAddresses, ingress.IP)
				} else if ingress.Hostname != "" {
					status.SSHLoadBalancerAddresses = append(status.SSHLoadBalancerAddresses, ingress.Hostname)
				}
			}
			sort.Strings(status.SSHLoadBalancerAddresses)
		}
		return status, nil
	}
	deploymentAvailable := status.Available
	status.Available = false
	var lease coordinationv1.Lease
	err = s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: cacheOwnerLeaseName(component.Name)}, &lease)
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
	status.CacheBytes, _ = strconv.ParseInt(lease.Annotations[cacheBytesAnnotation], 10, 64)
	status.MirrorBytes, _ = strconv.ParseInt(lease.Annotations[cacheMirrorBytesAnnotation], 10, 64)
	status.CleanupResult = lease.Annotations[cacheCleanupResultAnnotation]
	return status, nil
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
		remote, _, err := siteManagerClientForCheck(ctx, s.Client, s.Namespace, site, s.RegistrationHTTPClient)
		if err != nil {
			return err
		}
		checked, err := remote.CheckManager(ctx, &codespacev1.CheckManagerRequest{ProtocolVersion: controlplane.ProtocolVersion})
		if err != nil {
			return apierrors.NewBadRequest("Gitea Manager connection check failed: " + err.Error())
		}
		canonical, err := canonicalGiteaURL(checked.GiteaWebUrl)
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
	if _, err := canonicalGiteaURL(registration.GetGiteaWebUrl()); err != nil {
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
	if err := validateEnvironmentDependencies(ctx, s.Client, template.Spec.Runtime); err != nil {
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
		generation := template.Generation
		return updateTemplateStatus(ctx, s.Client, template, func(current *api.EnvironmentTemplate) error {
			if current.Generation != generation {
				return errObservedResourceChanged
			}
			current.Status.VerifiedGeneration, current.Status.Verification = generation, body.Verification
			return nil
		})
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
		gateway := component.Spec.Gateway
		if gateway.HTTPExposure.Mode == "ingress" {
			if err := gatewayIngressClassAvailable(ctx, s.Client, gateway.HTTPExposure.Ingress.ClassName); err != nil {
				return apierrors.NewBadRequest(err.Error())
			}
			switch tls := gateway.HTTPExposure.Ingress.TLS; tls.Source {
			case "secret":
				ready, err := gatewayTLSSecretReady(ctx, s.Client, s.Namespace, tls.SecretName)
				if err != nil {
					return err
				}
				if !ready {
					return apierrors.NewBadRequest(fmt.Sprintf("TLS secret %q is unavailable or incomplete", tls.SecretName))
				}
			case "certManager":
				if err := gatewayIssuerAvailable(ctx, s.Client, s.Namespace, tls.Issuer); err != nil {
					return apierrors.NewBadRequest(err.Error())
				}
			}
		}
	} else {
		if _, err := cacheComponentConfig(component); err != nil {
			return apierrors.NewBadRequest(err.Error())
		}
		if component.Spec.Cache.Storage.Driver == "filesystem" {
			storageClassName := component.Spec.Cache.Storage.StorageClassName
			if storageClassName != "" {
				var storage storagev1.StorageClass
				if err := s.Client.Get(ctx, types.NamespacedName{Name: storageClassName}, &storage); err != nil {
					return apierrors.NewBadRequest(fmt.Sprintf("storage class %q is unavailable: %v", storageClassName, err))
				}
			} else {
				var storages storagev1.StorageClassList
				if err := s.Client.List(ctx, &storages); err != nil {
					return err
				}
				hasDefault := false
				for _, storage := range storages.Items {
					if isDefaultStorageClass(storage.Annotations) {
						hasDefault = true
						break
					}
				}
				if !hasDefault {
					return apierrors.NewBadRequest("filesystem cache requires an available StorageClass or a cluster default StorageClass")
				}
			}
		}
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
			err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: cacheOwnerLeaseName(current.Name)}, &lease)
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
