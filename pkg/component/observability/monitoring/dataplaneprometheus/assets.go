// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and Gardener contributors
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
