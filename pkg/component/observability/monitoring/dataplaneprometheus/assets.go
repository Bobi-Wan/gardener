// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package dataplaneprometheus

import (
	_ "embed"
)

// prometheusConfig contains the complete Prometheus configuration for scraping
// kubelet, cadvisor, and node-exporter metrics in the shoot data plane.
//
//go:embed assets/dataplane-prometheus-config.yaml
var prometheusConfig string

// GetPrometheusConfig returns the embedded Prometheus configuration for scraping
// kubelet, cadvisor, and node-exporter metrics in the shoot data plane.
func GetPrometheusConfig() string {
	return prometheusConfig
}
