// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"testing"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"gitea.dev/codespace/internal/runtimeplatform"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testQuota(site *api.GiteaSite, namespace string) *corev1.ResourceQuota {
	used := corev1.ResourceList{}
	for name := range site.Spec.Quota {
		used[name] = resource.MustParse("0")
	}
	return &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "codespace", Namespace: namespace, Labels: map[string]string{SiteUIDLabel: string(site.UID)}}, Spec: corev1.ResourceQuotaSpec{Hard: site.Spec.Quota.DeepCopy()}, Status: corev1.ResourceQuotaStatus{Hard: site.Spec.Quota.DeepCopy(), Used: used}}
}

func TestSiteCapacityIncludesNativeDevices(t *testing.T) {
	site := testSite("example", "site-uid")
	deviceQuota := corev1.ResourceName("requests.example.com/device")
	site.Spec.Quota[deviceQuota] = resource.MustParse("1")
	quota := testQuota(site, "codespace-example")
	quota.Status.Used[deviceQuota] = resource.MustParse("1")
	template := &api.EnvironmentTemplate{ObjectMeta: metav1.ObjectMeta{Name: "native", UID: "template-uid"}, Spec: api.EnvironmentTemplateSpec{Runtime: testEnvironment()}}
	template.Spec.Runtime.Isolation = api.IsolationNative
	template.Spec.Runtime.RuntimeClassName = ""
	template.Spec.Runtime.Native = &runtimeplatform.NativeConfiguration{DevicePlugins: []runtimeplatform.DevicePluginRequest{{
		ResourceName: "example.com/device", Count: 1,
		HostAccess: runtimeplatform.HostAccess{Devices: []runtimeplatform.DeviceMapping{{Source: "/dev/example", Target: "/dev/example", Permissions: "rwm"}}},
	}}}
	site.Spec.Templates = []api.ResourceReference{{Name: template.Name, UID: template.UID}}
	client := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(quota).WithObjects(quota, template).Build()
	coordinator := SiteCoordinator{Operations: Operations{Client: client, ManagementNamespace: "codespace-system"}}
	request := &codespacev1.FetchOperationsRequest{}
	require.NoError(t, coordinator.capacity(t.Context(), site, nil, []string{template.Name}, request))
	require.Empty(t, request.AcceptedCreateTags)
}

func TestSiteCapacity(t *testing.T) {
	site := testSite("example", "site-uid")
	quota := testQuota(site, "codespace-example")
	small := &api.EnvironmentTemplate{ObjectMeta: metav1.ObjectMeta{Name: "small", UID: "template-uid"}, Spec: api.EnvironmentTemplateSpec{Runtime: testEnvironment()}}
	site.Spec.Templates[0].Name = small.Name
	large := small.DeepCopy()
	large.Name, large.UID = "large", "large-uid"
	large.Spec.Runtime.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("8Gi")
	site.Spec.Templates = append(site.Spec.Templates, api.ResourceReference{Name: large.Name, UID: large.UID})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(quota).WithObjects(quota, small, large).Build()
	coordinator := SiteCoordinator{Operations: Operations{Client: c, ManagementNamespace: "codespace-system"}}
	request := &codespacev1.FetchOperationsRequest{}
	require.NoError(t, coordinator.capacity(t.Context(), site, nil, []string{"small", "large"}, request))
	require.Equal(t, []string{"small"}, request.AcceptedCreateTags)
	require.EqualValues(t, 1, request.StartupCapacityAvailable)

	rows := []api.Codespace{{Spec: api.CodespaceSpec{Operation: api.Operation{Type: "create", Version: 1}}}, {Spec: api.CodespaceSpec{Operation: api.Operation{Type: "stop", Version: 2}}}}
	request = &codespacev1.FetchOperationsRequest{}
	require.NoError(t, coordinator.capacity(t.Context(), site, rows, []string{"small"}, request))
	require.Zero(t, request.StartupCapacityAvailable)
	require.EqualValues(t, 15, request.CleanupCapacityAvailable)

	rows[0].Status.SettledOperationVersion = 1
	quota.Status.Used[corev1.ResourceRequestsStorage] = quota.Spec.Hard[corev1.ResourceRequestsStorage].DeepCopy()
	require.NoError(t, c.Status().Update(t.Context(), quota))
	request = &codespacev1.FetchOperationsRequest{}
	require.NoError(t, coordinator.capacity(t.Context(), site, rows, []string{"small"}, request))
	require.Equal(t, []codespacev1.AcceptedOperationType{codespacev1.AcceptedOperationType_ACCEPTED_OPERATION_TYPE_RESUME}, request.AcceptedOperationTypes)
	require.EqualValues(t, 1, request.StartupCapacityAvailable)

	quota.Status.Hard = nil
	require.NoError(t, c.Status().Update(t.Context(), quota))
	request = &codespacev1.FetchOperationsRequest{}
	require.Error(t, coordinator.capacity(t.Context(), site, rows, []string{"small"}, request))
	require.Zero(t, request.StartupCapacityAvailable)
	require.EqualValues(t, 15, request.CleanupCapacityAvailable)

	site.Spec.Quota = nil
	require.NoError(t, c.Delete(t.Context(), quota))
	request = &codespacev1.FetchOperationsRequest{}
	require.NoError(t, coordinator.capacity(t.Context(), site, nil, []string{"small", "large"}, request))
	require.Equal(t, []string{"small", "large"}, request.AcceptedCreateTags)
	require.EqualValues(t, 1, request.StartupCapacityAvailable)
}
