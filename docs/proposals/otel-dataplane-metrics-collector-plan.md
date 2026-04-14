# Plan: OTEL Collector Agent for Data-Plane Metrics Filtering

## Problem Statement

**Current Architecture:**
```
┌─────────────────────────────────────────────────────────────────────────────┐
│ Shoot Data-Plane                                                            │
│  ┌──────────┐  ┌──────────┐  ┌─────────────┐                               │
│  │ kubelet  │  │ cadvisor │  │node-exporter│  ...                          │
│  │ /metrics │  │ /metrics │  │  /metrics   │                               │
│  └────┬─────┘  └────┬─────┘  └──────┬──────┘                               │
│       │             │               │                                       │
│       └─────────────┼───────────────┘                                       │
│                     │ ALL metrics                                           │
└─────────────────────┼───────────────────────────────────────────────────────┘
                      │ via kube-apiserver /proxy
                      ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│ Seed Control-Plane (shoot--project--name namespace)                         │
│  ┌──────────────────────────────────────────────────────────────────────┐  │
│  │ CP Prometheus                                                         │  │
│  │  - Scrapes via /api/v1/nodes/${node}/proxy/metrics                   │  │
│  │  - Receives ALL metrics over the network                             │  │
│  │  - Applies MetricRelabelConfigs (filtering) AFTER receiving          │  │
│  │  - Stores only filtered metrics in TSDB                              │  │
│  └──────────────────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────────────────┘
```

**Issues:**
1. Full metric set transferred over network (wasted bandwidth)
2. Filtering happens in control-plane after data transfer
3. Unnecessary network costs, especially with many shoots

---

## Goal

Deploy an OTEL collector agent in the shoot data-plane that:
1. Scrapes the same targets locally (kubelet, cadvisor, node-exporter, etc.)
2. Applies the same filtering rules **before** sending data
3. Exposes a single Prometheus endpoint with pre-filtered metrics
4. CP Prometheus scrapes only this single endpoint

**Target Architecture:**
```
┌─────────────────────────────────────────────────────────────────────────────┐
│ Shoot Data-Plane                                                            │
│                                                                             │
│  Node 1              Node 2              Node N                             │
│  ┌──────────┐        ┌──────────┐        ┌──────────┐                      │
│  │ kubelet  │        │ kubelet  │        │ kubelet  │                      │
│  │ /metrics │        │ /metrics │        │ /metrics │                      │
│  │ /cadvisor│        │ /cadvisor│        │ /cadvisor│                      │
│  └────┬─────┘        └────┬─────┘        └────┬─────┘                      │
│       │                   │                   │                             │
│       └───────────────────┼───────────────────┘                             │
│                           │ scrape all nodes                                │
│                           ▼                                                 │
│  ┌──────────────────────────────────────────────────────────────────────┐  │
│  │ OTEL Collector Agent (Deployment + Service)                           │  │
│  │  - prometheus receiver: scrapes ALL nodes' kubelet/cadvisor          │  │
│  │  - filter processor: applies metric filtering                         │  │
│  │  - prometheus exporter: exposes single /metrics endpoint              │  │
│  │  - Service: otel-collector-agent:9090                                 │  │
│  └────────────────────────────────┬─────────────────────────────────────┘  │
│                                   │ FILTERED metrics (single endpoint)      │
└───────────────────────────────────┼─────────────────────────────────────────┘
                                    │ via kube-apiserver /proxy to Service
                                    ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│ Seed Control-Plane                                                          │
│  ┌──────────────────────────────────────────────────────────────────────┐  │
│  │ CP Prometheus                                                         │  │
│  │  - Single scrape target: otel-collector-agent Service                 │  │
│  │  - Receives pre-filtered, aggregated metrics from all nodes           │  │
│  │  - Stores directly in TSDB                                            │  │
│  └──────────────────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## Current Scrape Targets (from scrapeconfigs.go)

| ScrapeConfig Name | Target | Metrics Path | Key Filtered Metrics |
|-------------------|--------|--------------|---------------------|
| `cadvisor` | kubelet via API proxy | `/api/v1/nodes/${node}/proxy/metrics/cadvisor` | `container_cpu_*`, `container_memory_*`, `container_fs_*`, `container_network_*` |
| `kube-kubelet` | kubelet via API proxy | `/api/v1/nodes/${node}/proxy/metrics` | `kubelet_running_pods`, `kubelet_volume_stats_*`, `kubelet_image_pull_*`, `process_*` |

These are the primary bandwidth consumers - full metric sets transferred, then filtered.

---

## Implementation Plan

### Phase 1: OTEL Collector Data-Plane Component

#### 1.1 New Component Location

```
pkg/component/observability/opentelemetry/collector/dataplane/
├── dataplane.go           # Main component (DaemonSet deployer)
├── dataplane_test.go      # Unit tests
├── config.go              # Collector configuration generation
├── config_test.go         # Config tests
└── constants/
    └── constants.go       # Ports, names, etc.
```

#### 1.2 OTEL Collector Configuration

The collector uses `kubernetes_sd_configs` to discover **all nodes** and scrapes each node's kubelet/cadvisor endpoints. This centralizes collection so CP Prometheus only needs to scrape a single endpoint.

```yaml
receivers:
  prometheus/kubelet:
    config:
      scrape_configs:
        - job_name: 'kubelet'
          scheme: https
          tls_config:
            ca_file: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
            insecure_skip_verify: true  # kubelet uses self-signed cert
          bearer_token_file: /var/run/secrets/kubernetes.io/serviceaccount/token
          kubernetes_sd_configs:
            - role: node
          relabel_configs:
            # Scrape kubelet on each discovered node
            - source_labels: [__meta_kubernetes_node_address_InternalIP]
              target_label: __address__
              replacement: ${1}:10250
            - source_labels: [__meta_kubernetes_node_name]
              target_label: instance
            - source_labels: [__meta_kubernetes_node_name]
              target_label: node

  prometheus/cadvisor:
    config:
      scrape_configs:
        - job_name: 'cadvisor'
          scheme: https
          metrics_path: /metrics/cadvisor
          tls_config:
            ca_file: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
            insecure_skip_verify: true
          bearer_token_file: /var/run/secrets/kubernetes.io/serviceaccount/token
          kubernetes_sd_configs:
            - role: node
          relabel_configs:
            - source_labels: [__meta_kubernetes_node_address_InternalIP]
              target_label: __address__
              replacement: ${1}:10250
            - source_labels: [__meta_kubernetes_node_name]
              target_label: instance
            - source_labels: [__meta_kubernetes_node_name]
              target_label: node

processors:
  # Filter to match current MetricRelabelConfigs
  filter/kubelet:
    metrics:
      include:
        match_type: regexp
        metric_names:
          - kubelet_running_pods
          - process_max_fds
          - process_open_fds
          - kubelet_volume_stats_available_bytes
          - kubelet_volume_stats_capacity_bytes
          - kubelet_volume_stats_used_bytes
          - kubelet_image_pull_duration_seconds_bucket
          - kubelet_image_pull_duration_seconds_sum
          - kubelet_image_pull_duration_seconds_count

  filter/cadvisor:
    metrics:
      include:
        match_type: regexp
        metric_names:
          - container_cpu_cfs_periods_total
          - container_cpu_cfs_throttled_seconds_total
          - container_cpu_cfs_throttled_periods_total
          - container_cpu_usage_seconds_total
          - container_fs_inodes_total
          - container_fs_limit_bytes
          - container_fs_usage_bytes
          - container_fs_reads_total
          - container_fs_writes_total
          - container_last_seen
          - container_memory_working_set_bytes
          - container_network_receive_bytes_total
          - container_network_transmit_bytes_total

  # Additional filtering: keep only kube-system namespace
  filter/namespace:
    metrics:
      include:
        match_type: expr
        expressions:
          - 'attributes["namespace"] == "" or attributes["namespace"] == "kube-system"'

  batch:
    timeout: 10s

exporters:
  prometheus:
    endpoint: "0.0.0.0:9090"
    namespace: ""
    send_timestamps: true
    resource_to_telemetry_conversion:
      enabled: true

service:
  pipelines:
    metrics/kubelet:
      receivers: [prometheus/kubelet]
      processors: [filter/kubelet, filter/namespace, batch]
      exporters: [prometheus]
    metrics/cadvisor:
      receivers: [prometheus/cadvisor]
      processors: [filter/cadvisor, filter/namespace, batch]
      exporters: [prometheus]
```

#### 1.3 Deployment

A **Deployment** (not DaemonSet) is used so that:
- CP Prometheus has a **single scrape target** instead of N targets (one per node)
- The collector centrally scrapes all nodes and aggregates metrics
- Simpler architecture with fewer moving parts

```go
// pkg/component/observability/opentelemetry/collector/dataplane/dataplane.go

type Values struct {
    Image              string
    Namespace          string  // kube-system
    Replicas           int32   // 1 for PoC, can scale for HA
    PriorityClassName  string
    // Metrics filter configuration (derived from scrapeconfigs.go)
    KubeletMetrics     []string
    CadvisorMetrics    []string
}

func (d *dataplaneCollector) deployment() *appsv1.Deployment {
    return &appsv1.Deployment{
        ObjectMeta: metav1.ObjectMeta{
            Name:      "otel-collector-agent",
            Namespace: d.values.Namespace,
            Labels:    getLabels(),
        },
        Spec: appsv1.DeploymentSpec{
            Replicas: ptr.To(d.values.Replicas),
            Selector: &metav1.LabelSelector{MatchLabels: getLabels()},
            Template: corev1.PodTemplateSpec{
                Spec: corev1.PodSpec{
                    ServiceAccountName: "otel-collector-agent",
                    Containers: []corev1.Container{{
                        Name:  "otel-collector",
                        Image: d.values.Image,
                        Args:  []string{"--config=/etc/otel/config.yaml"},
                        Ports: []corev1.ContainerPort{{
                            Name:          "metrics",
                            ContainerPort: 9090,
                        }},
                        VolumeMounts: []corev1.VolumeMount{{
                            Name:      "config",
                            MountPath: "/etc/otel",
                        }},
                    }},
                    Volumes: []corev1.Volume{{
                        Name: "config",
                        VolumeSource: corev1.VolumeSource{
                            ConfigMap: &corev1.ConfigMapVolumeSource{
                                LocalObjectReference: corev1.LocalObjectReference{
                                    Name: "otel-collector-agent-config",
                                },
                            },
                        },
                    }},
                },
            },
        },
    }
}

func (d *dataplaneCollector) service() *corev1.Service {
    return &corev1.Service{
        ObjectMeta: metav1.ObjectMeta{
            Name:      "otel-collector-agent",
            Namespace: d.values.Namespace,
            Labels:    getLabels(),
        },
        Spec: corev1.ServiceSpec{
            Selector: getLabels(),
            Ports: []corev1.ServicePort{{
                Name:       "metrics",
                Port:       9090,
                TargetPort: intstr.FromInt(9090),
            }},
        },
    }
}
```

#### 1.4 RBAC Requirements

Yes, RBAC is required for the collector to:
1. Discover nodes via `kubernetes_sd_configs`
2. Scrape kubelet metrics endpoints on each node

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: otel-collector-agent
  namespace: kube-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: otel-collector-agent
rules:
  # For prometheus receiver kubernetes_sd_config (node discovery)
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get", "list", "watch"]
  # For scraping kubelet /metrics and /metrics/cadvisor
  - apiGroups: [""]
    resources: ["nodes/metrics", "nodes/proxy"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: otel-collector-agent
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: otel-collector-agent
subjects:
  - kind: ServiceAccount
    name: otel-collector-agent
    namespace: kube-system
```

### Phase 2: Update CP Prometheus Scrape Configs

#### 2.1 New Scrape Config for OTEL Collector

Replace `cadvisor` and `kube-kubelet` scrape configs with a **single static target** pointing to the collector's Service:

```go
// In scrapeconfigs.go - add new config when feature enabled
&monitoringv1alpha1.ScrapeConfig{
    ObjectMeta: metav1.ObjectMeta{
        Name: "otel-collector-dataplane",
    },
    Spec: monitoringv1alpha1.ScrapeConfigSpec{
        HonorLabels: ptr.To(true),  // Preserve labels from collector (node, instance, etc.)
        Scheme:      ptr.To(monitoringv1.SchemeHTTPS),
        Authorization: &monitoringv1.SafeAuthorization{
            Credentials: &corev1.SecretKeySelector{
                LocalObjectReference: corev1.LocalObjectReference{Name: AccessSecretName},
                Key:                  resourcesv1alpha1.DataKeyToken,
            },
        },
        TLSConfig: &monitoringv1.SafeTLSConfig{
            CA: monitoringv1.SecretOrConfigMap{
                Secret: &corev1.SecretKeySelector{
                    LocalObjectReference: corev1.LocalObjectReference{Name: clusterCASecretName},
                    Key:                  secretsutils.DataKeyCertificateBundle,
                },
            },
        },
        // Single static target: the OTEL collector Service via kube-apiserver proxy
        StaticConfigs: []monitoringv1alpha1.StaticConfig{{
            Targets: []monitoringv1alpha1.Target{
                v1beta1constants.DeploymentNameKubeAPIServer + ":" + strconv.Itoa(kubeapiserverconstants.Port),
            },
        }},
        RelabelConfigs: []monitoringv1.RelabelConfig{
            {
                TargetLabel: "__metrics_path__",
                Replacement: ptr.To("/api/v1/namespaces/kube-system/services/otel-collector-agent:9090/proxy/metrics"),
            },
            {
                Action:      "replace",
                Replacement: ptr.To("otel-collector-dataplane"),
                TargetLabel: "job",
            },
        },
        // No MetricRelabelConfigs needed - already filtered at the collector!
    },
}
```

#### 2.2 Feature Flag

Add new feature gate:

```go
// pkg/features/features.go
OpenTelemetryDataPlaneMetrics featuregate.Feature = "OpenTelemetryDataPlaneMetrics"
```

### Phase 3: Botanist Integration

#### 3.1 Add Component to Shoot System Components

```go
// pkg/gardenlet/operation/botanist/botanist.go
type Shoot struct {
    Components struct {
        SystemComponents struct {
            // ... existing
            OtelDataPlaneCollector dataplane.Interface  // NEW
        }
    }
}
```

#### 3.2 Deploy Flow

```go
// pkg/gardenlet/operation/botanist/otel_dataplane.go
func (b *Botanist) DefaultOtelDataPlaneCollector() (dataplane.Interface, error) {
    if b.Shoot.IsWorkerless {
        return component.NoOp(), nil
    }

    image, err := imagevector.Containers().FindImage(imagevector.ContainerImageNameOpentelemetryCollector)
    if err != nil {
        return nil, err
    }

    return dataplane.New(
        b.SeedClientSet.Client(),
        b.Shoot.ControlPlaneNamespace,
        dataplane.Values{
            Image:             image.String(),
            KubeletMetrics:    getKubeletAllowedMetrics(),  // From scrapeconfigs.go
            CadvisorMetrics:   getCadvisorAllowedMetrics(), // From scrapeconfigs.go
        },
        b.SecretsManager,
    ), nil
}

func (b *Botanist) DeployOtelDataPlaneCollector(ctx context.Context) error {
    if !features.DefaultFeatureGate.Enabled(features.OpenTelemetryDataPlaneMetrics) {
        return b.Shoot.Components.SystemComponents.OtelDataPlaneCollector.Destroy(ctx)
    }
    return b.Shoot.Components.SystemComponents.OtelDataPlaneCollector.Deploy(ctx)
}
```

### Phase 4: Local Setup Configuration

#### 4.1 Enable Feature in Local Gardenlet

```yaml
# example/gardener-local/gardenlet/values.yaml
config:
  featureGates:
    OpenTelemetryCollector: true
    OpenTelemetryDataPlaneMetrics: true  # NEW
```

---

## Phase 5: PoC - Connectivity Resilience Testing

### 5.1 Test Scenario Setup

Create controlled connectivity disruption between CP Prometheus and data-plane OTEL collector:

```bash
# 1. Deploy shoot with OTEL data-plane collector
kubectl apply -f example/provider-local/shoot.yaml

# 2. Verify metrics flow
kubectl -n shoot--local--local port-forward svc/prometheus 9090:9090
# Query: up{job="otel-collector-dataplane"}

# 3. Simulate network partition using NetworkPolicy
kubectl apply -f - <<EOF
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: block-otel-collector
  namespace: shoot--local--local
spec:
  podSelector:
    matchLabels:
      app: prometheus
  egress:
    - to:
        - podSelector:
            matchLabels:
              app: NOT-otel-collector  # Block traffic
EOF

# 4. Observe metrics gaps
# 5. Remove NetworkPolicy and observe recovery
```

### 5.2 OTEL Collector Resilience Configuration

Test different configurations to minimize data loss:

```yaml
# Option A: Memory-based buffering (default prometheus exporter behavior)
exporters:
  prometheus:
    endpoint: "0.0.0.0:9090"

# Option B: Use prometheusremotewrite exporter for push-based with retry
exporters:
  prometheusremotewrite:
    endpoint: "http://prometheus:9090/api/v1/write"
    retry_on_failure:
      enabled: true
      initial_interval: 5s
      max_interval: 30s
      max_elapsed_time: 300s
    sending_queue:
      enabled: true
      num_consumers: 10
      queue_size: 5000
```

### 5.3 Metrics to Monitor During Test

| Metric | Purpose |
|--------|---------|
| `otelcol_exporter_send_failed_metric_points` | Failed exports |
| `otelcol_processor_dropped_metric_points` | Dropped during processing |
| `otelcol_receiver_refused_metric_points` | Refused at receiver |
| `prometheus_tsdb_head_samples_appended_total` | Samples stored in Prometheus |
| `up{job="otel-collector-dataplane"}` | Collector reachability |

### 5.4 Expected Findings

| Scenario | Expected Behavior |
|----------|-------------------|
| Brief network blip (<30s) | Prometheus retry handles it, minimal/no data loss |
| Extended outage (>scrape_interval) | Gaps in metrics, collector continues buffering locally |
| Recovery after outage | Prometheus resumes scraping, no historical backfill (pull model) |

**Recommendation from PoC:** If data loss during outages is unacceptable, consider:
1. Using `prometheusremotewrite` exporter (push model) with retry queue
2. Configuring aggressive retry on CP Prometheus side
3. Running a small local Prometheus on data-plane as a buffer (complex)

---

## File Changes Summary

| File | Change Type | Description |
|------|-------------|-------------|
| `pkg/component/observability/opentelemetry/collector/dataplane/` | New | Data-plane collector component |
| `pkg/features/features.go` | Modify | Add `OpenTelemetryDataPlaneMetrics` feature gate |
| `pkg/component/observability/monitoring/prometheus/shoot/scrapeconfigs.go` | Modify | Add OTEL collector scrape config, conditionally disable cadvisor/kubelet |
| `pkg/gardenlet/operation/botanist/botanist.go` | Modify | Add OtelDataPlaneCollector component |
| `pkg/gardenlet/operation/botanist/otel_dataplane.go` | New | Default factory and deploy method |
| `pkg/gardenlet/operation/shoot/shoot.go` | Modify | Initialize component |
| `example/gardener-local/gardenlet/values.yaml` | Modify | Enable feature gate |
| `imagevector/containers.yaml` | Verify | OTEL collector image present |

---

## RBAC Summary

**Yes, explicit RBAC is required** for the data-plane collector because it needs to:

1. **Node discovery**: `nodes` - get, list, watch (for `kubernetes_sd_configs`)
2. **Kubelet metrics access**: `nodes/metrics`, `nodes/proxy` - get (for scraping `/metrics` and `/metrics/cadvisor`)

---

## Next Steps

1. **Implement Phase 1** - Create data-plane collector component
2. **Implement Phase 2** - Update scrape configs with feature flag
3. **Test locally** - Verify bandwidth reduction and metric parity
4. **Run PoC tests** - Connectivity resilience scenarios
5. **Document findings** - Recommendations for production configuration
