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

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type adminComponentWrite struct {
	adminWriteIdentity
	Spec        api.CodespaceComponentSpec `json:"spec"`
	S3AccessKey string                     `json:"s3AccessKey,omitempty"`
	S3SecretKey string                     `json:"s3SecretKey,omitempty"`
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
		return status, nil
	}
	status.LastHeartbeat = lease.Spec.RenewTime
	status.Available = deploymentAvailable
	status.CacheBytes, _ = strconv.ParseInt(lease.Annotations[cacheBytesAnnotation], 10, 64)
	status.MirrorBytes, _ = strconv.ParseInt(lease.Annotations[cacheMirrorBytesAnnotation], 10, 64)
	status.CleanupResult = lease.Annotations[cacheCleanupResultAnnotation]
	return status, nil
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
	if err != nil || component.Spec.Role != "cache" {
		return err
	}
	return s.saveCacheCredentials(ctx, component, body)
}

func (s *AdminServer) saveCacheCredentials(ctx context.Context, component *api.CodespaceComponent, body adminComponentWrite) error {
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
			if body.S3AccessKey != "" {
				secret.Data["s3AccessKey"], secret.Data["s3SecretKey"] = []byte(body.S3AccessKey), []byte(body.S3SecretKey)
			}
			secret.Data = updateProxySecretData(secret.Data, component.Spec.Cache.Proxy)
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
		if body.S3AccessKey != "" {
			secret.Data["s3AccessKey"], secret.Data["s3SecretKey"] = []byte(body.S3AccessKey), []byte(body.S3SecretKey)
		}
		secret.Data = updateProxySecretData(secret.Data, component.Spec.Cache.Proxy)
		return s.Client.Update(ctx, &secret)
	})
}

func newGatewayHostKeySecret(name, namespace string) (*corev1.Secret, error) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       map[string][]byte{"hostKey": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})},
	}, nil
}
