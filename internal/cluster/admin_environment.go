// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

type adminEnvironmentWrite struct {
	adminWriteIdentity
	Spec api.EnvironmentTemplateSpec `json:"spec"`
}

func (s *AdminServer) saveEnvironment(ctx context.Context, template *api.EnvironmentTemplate, body adminEnvironmentWrite, creating bool) error {
	template.Spec = body.Spec
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
				if err := validateProxyDestination(sites.Items[i].Spec.Upstreams, template.Spec.Runtime.Proxy); err != nil {
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

func validateProxyDestination(upstreams []api.NetworkDestination, proxy *api.OutboundProxy) error {
	if proxy == nil {
		return nil
	}
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
		allowed := false
		for _, destination := range upstreams {
			prefix, err := netip.ParsePrefix(destination.CIDR)
			if err != nil || !prefix.Contains(address) {
				continue
			}
			for _, allowedPort := range destination.Ports {
				allowed = allowed || allowedPort == int32(port)
			}
		}
		if !allowed {
			return fmt.Errorf("proxy %s:%d requires a matching upstream CIDR and port", address, port)
		}
	}
	return nil
}
