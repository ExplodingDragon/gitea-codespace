// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"fmt"
	"net/netip"
	"net/url"
	"reflect"
	"strconv"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"gitea.dev/codespace/internal/devcontainerruntime"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

type adminEnvironmentWrite struct {
	adminWriteIdentity
	Spec api.EnvironmentTemplateSpec `json:"spec"`
}

func (s *AdminServer) saveEnvironment(ctx context.Context, template *api.EnvironmentTemplate, body adminEnvironmentWrite, creating bool) error {
	previousWebIDE := template.Spec.Runtime.DevContainer.WebIDE
	spec := body.Spec.DeepCopy()
	requestedWebIDE := spec.Runtime.DevContainer.WebIDE
	switch {
	case !requestedWebIDE.Enabled():
		spec.Runtime.DevContainer.WebIDE = devcontainerruntime.WebIDEConfiguration{}
	case !creating && reflect.DeepEqual(requestedWebIDE, previousWebIDE):
		// Unrelated template edits retain the already verified immutable snapshot.
	case s.WebIDEResolver == nil:
		return apierrors.NewServiceUnavailable("Web IDE image resolution is unavailable")
	default:
		resolved, err := s.WebIDEResolver.Resolve(ctx, requestedWebIDE.Image)
		if err != nil {
			return apierrors.NewBadRequest(fmt.Sprintf("resolve Web IDE image: %v", err))
		}
		configuration, err := resolved.configuration(requestedWebIDE.Options)
		if err != nil {
			return apierrors.NewBadRequest(err.Error())
		}
		spec.Runtime.DevContainer.WebIDE = configuration
	}
	template.Spec = *spec
	if err := template.Validate(); err != nil {
		return apierrors.NewBadRequest(err.Error())
	}
	if err := validateEnvironmentDependencies(ctx, s.Client, template.Spec.Runtime); err != nil {
		return apierrors.NewBadRequest(err.Error())
	}
	if !creating && template.Spec.Runtime.Proxy != nil {
		var sites api.GiteaSiteList
		if err := s.Client.List(ctx, &sites); err != nil {
			return err
		}
		for i := range sites.Items {
			for _, ref := range sites.Items[i].Spec.Templates {
				if ref.UID != template.UID {
					continue
				}
				if err := validateProxyDestination(sites.Items[i].Spec.Network.Egress, template.Spec.Runtime.Proxy); err != nil {
					return apierrors.NewBadRequest(fmt.Sprintf("site %s: %v", sites.Items[i].Name, err))
				}
			}
		}
	}
	if creating {
		return s.Client.Create(ctx, template)
	}
	return s.Client.Update(ctx, template)
}

func validateProxyDestination(egress api.SiteEgress, proxy *api.OutboundProxy) error {
	if proxy == nil {
		return nil
	}
	network, err := (api.SiteNetwork{Egress: egress}).Normalized()
	if err != nil {
		return err
	}
	egress = network.Egress
	for _, raw := range []string{proxy.HTTPProxy, proxy.HTTPSProxy, proxy.SOCKSProxy} {
		if raw == "" {
			continue
		}
		parsed, _ := url.Parse(raw)
		address, err := netip.ParseAddr(parsed.Hostname())
		if err != nil {
			continue
		}
		port, _ := strconv.ParseInt(parsed.Port(), 10, 32)
		if !siteAllowsEgress(egress, address, corev1.ProtocolTCP, int32(port)) {
			return fmt.Errorf("proxy %s:%d requires an allowed TCP egress destination", address, port)
		}
	}
	return nil
}
