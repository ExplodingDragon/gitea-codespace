// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"testing"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func managedCacheVolume(component *api.CodespaceComponent, storageClass string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: component.Name, Namespace: "codespace-system", UID: "cache-volume-uid", Labels: map[string]string{ComponentUIDLabel: string(component.UID)},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(component, api.GroupVersion.WithKind("CodespaceComponent"))},
		},
		Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: ptr.To(storageClass), Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}},
	}
}

func TestAdminSaveComponentRejectsExistingCacheStorageClassChange(t *testing.T) {
	component := testCacheComponent("cache", "cache-uid")
	component.Spec.Cache.Storage.StorageClassName = "fast"
	claim := managedCacheVolume(component, "fast")
	kubeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		component, claim,
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "fast"}},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "slow"}},
	).Build()
	server := &AdminServer{Client: kubeClient, Namespace: claim.Namespace}
	updated := component.DeepCopy().Spec
	updated.Cache.Storage.StorageClassName = "slow"

	err := server.saveComponent(t.Context(), component, adminComponentWrite{Spec: updated}, false)
	require.EqualError(t, err, `cache storage class cannot be changed after PVC creation; existing class is "fast"`)
	var stored api.CodespaceComponent
	require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKeyFromObject(component), &stored))
	require.Equal(t, "fast", stored.Spec.Cache.Storage.StorageClassName)
}

func TestAdminSaveComponentRejectsUnownedExistingCacheVolume(t *testing.T) {
	component := testCacheComponent("cache", "cache-uid")
	claim := managedCacheVolume(component, "fast")
	claim.Labels[ComponentUIDLabel] = "other-component-uid"
	kubeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(component, claim).Build()
	server := &AdminServer{Client: kubeClient, Namespace: claim.Namespace}
	updated := component.DeepCopy().Spec
	updated.DisplayName = "Updated cache"

	err := server.saveComponent(t.Context(), component, adminComponentWrite{Spec: updated}, false)
	require.True(t, apierrors.IsConflict(err), err)
	require.ErrorContains(t, err, "cache volume ownership conflicts")
	var stored api.CodespaceComponent
	require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKeyFromObject(component), &stored))
	require.Equal(t, "Build cache", stored.Spec.DisplayName)
}

func TestAdminSaveComponentKeepsDefaultStorageClassOnExistingCache(t *testing.T) {
	component := testCacheComponent("cache", "cache-uid")
	claim := managedCacheVolume(component, "fast")
	kubeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		component, claim,
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "fast", Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}}},
	).Build()
	server := &AdminServer{Client: kubeClient, Namespace: claim.Namespace}
	updated := component.DeepCopy().Spec
	updated.DisplayName = "Updated cache"
	var storageClass storagev1.StorageClass
	require.NoError(t, kubeClient.Get(t.Context(), types.NamespacedName{Name: "fast"}, &storageClass))
	require.NoError(t, kubeClient.Delete(t.Context(), &storageClass))
	require.NoError(t, server.saveComponent(t.Context(), component, adminComponentWrite{Spec: updated}, false))

	var storedClaim corev1.PersistentVolumeClaim
	require.NoError(t, kubeClient.Get(t.Context(), types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}, &storedClaim))
	require.Equal(t, "fast", ptr.Deref(storedClaim.Spec.StorageClassName, ""))
	status, err := server.componentStatus(t.Context(), component)
	require.NoError(t, err)
	require.True(t, status.StorageClassLocked)
	require.Equal(t, "fast", status.StorageClassName)
}
