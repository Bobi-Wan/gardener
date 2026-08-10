// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package botanist

import (
	"context"

	"github.com/gardener/gardener/imagevector"
	"github.com/gardener/gardener/pkg/component"
	"github.com/gardener/gardener/pkg/component/observability/monitoring/dataplaneprometheus"
	"github.com/gardener/gardener/pkg/features"
	imagevectorutils "github.com/gardener/gardener/pkg/utils/imagevector"
)

// DefaultDataplanePrometheus returns a deployer for the dataplane Prometheus component.
func (b *Botanist) DefaultDataplanePrometheus() (component.DeployWaiter, error) {
	image, err := imagevector.Containers().FindImage(
		imagevector.ContainerImageNamePrometheus,
		imagevectorutils.RuntimeVersion(b.ShootVersion()),
		imagevectorutils.TargetVersion(b.ShootVersion()),
	)
	if err != nil {
		return nil, err
	}

	config := dataplaneprometheus.Config{
		Image:      image.String(),
		Replicas:   1,
		VPAEnabled: b.Shoot.WantsVerticalPodAutoscaler,
	}

	return dataplaneprometheus.New(
		b.SeedClientSet.Client(),
		b.Shoot.ControlPlaneNamespace,
		config,
	), nil
}

// ReconcileDataplanePrometheus deploys or destroys the dataplane Prometheus component
// depending on whether the feature gate is enabled and shoot monitoring is enabled.
func (b *Botanist) ReconcileDataplanePrometheus(ctx context.Context) error {
	if !features.DefaultFeatureGate.Enabled(features.DataplanePrometheus) ||
		!b.IsShootMonitoringEnabled() {
		return b.Shoot.Components.SystemComponents.DataplanePrometheus.Destroy(ctx)
	}

	return b.Shoot.Components.SystemComponents.DataplanePrometheus.Deploy(ctx)
}
