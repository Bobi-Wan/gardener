// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

// Package dataplaneprometheus contains the main reconcilation logic for
// the dataplane Prometheus component, responsible for scraping and filtering
// metrics targets in the shoot's data plane.
package dataplaneprometheus

import (
	"context"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	vpaautoscalingv1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	"github.com/gardener/gardener/pkg/client/kubernetes"
	"github.com/gardener/gardener/pkg/component"
	"github.com/gardener/gardener/pkg/utils"
	"github.com/gardener/gardener/pkg/utils/managedresources"
)

const (
	managedResourceName = "shoot-core-dataplane-prometheus"
	componentName       = "dataplane-prometheus"

	labelKeyComponent = "component"
	metricsPortName   = "metrics"
	portMetrics       = int32(9090)

	volumeNameConfig = "config"
	volumeNameData   = "data"
	mountPathConfig  = "/etc/prometheus"
	mountPathData    = "/data"

	targetNamespace               = metav1.NamespaceSystem
	TimeoutWaitForManagedResource = 2 * time.Minute
)

// Config is the dataplane Prometheus instance configuration.
type Config struct {
	// Image is the container image used for the dataplane Prometheus.
	Image string
	// Replicas is the number of replicas for the stateful set.
	Replicas int32
	// VPAEnabled marks whether VerticalPodAutoscaler functionality is enabled for the shoot.
	VPAEnabled bool
}

// component config wrapper, which is exposed as a DeployWaiter
type dataplanePrometheus struct {
	client    client.Client
	namespace string
	config    Config
}

var _ component.DeployWaiter = &dataplanePrometheus{}

// New creates an instance of DeployWaiter for the dataplane Prometheus component.
func New(
	client client.Client,
	namespace string,
	config Config,
) component.DeployWaiter {
	return &dataplanePrometheus{
		client:    client,
		namespace: namespace,
		config:    config,
	}
}

// creates the appropriate managed resource and deploys it in the shoot's
// control plane.
func (d *dataplanePrometheus) Deploy(ctx context.Context) error {
	data, err := d.computeResourcesData()
	if err != nil {
		return err
	}

	// keepObjects: false here so deleting the MR deletes components in the
	// data plane
	return managedresources.CreateForShoot(ctx, d.client, d.namespace, managedResourceName, managedresources.LabelValueGardener, false, data)
}

func (d *dataplanePrometheus) Destroy(ctx context.Context) error {
	return managedresources.DeleteForShoot(ctx, d.client, d.namespace, managedResourceName)
}

func (d *dataplanePrometheus) Wait(ctx context.Context) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, TimeoutWaitForManagedResource)
	defer cancel()

	return managedresources.WaitUntilHealthy(timeoutCtx, d.client, d.namespace, managedResourceName)
}

func (d *dataplanePrometheus) WaitCleanup(ctx context.Context) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, TimeoutWaitForManagedResource)
	defer cancel()

	return managedresources.WaitUntilDeleted(timeoutCtx, d.client, d.namespace, managedResourceName)
}

// the created resources are:
// - ClusterRole: lists permissions to scrape dataplane target /metrics endpoints
// - SA: assigned given the above permissions
// - ClusterRoleBinding: binds the two resources above
// - Service: exposes the Prometheus instance for scraping by the shoot Prometheus
// - ConfigMap: holds the Prometheus configuration
// - StatefulSet: deploys the Prometheus instance as a SS, for future extendability if
// we decide to use PVs for persistence
// - (optional) VPA:  scales the Prometheus instance vertically as the one in the control plane
func (d *dataplanePrometheus) computeResourcesData() (map[string][]byte, error) {
	var (
		registry = managedresources.NewRegistry(kubernetes.ShootScheme, kubernetes.ShootCodec, kubernetes.ShootSerializer)

		serviceAccount     = d.getServiceAccount()
		clusterRole        = d.getClusterRole()
		clusterRoleBinding = d.getClusterRoleBinding(clusterRole.Name, serviceAccount.Name, serviceAccount.Namespace)
		service            = d.getService()
		configMap          = d.getConfigMap()
		statefulSet        = d.getStatefulSet(configMap.Name, serviceAccount.Name)
	)

	objects := []client.Object{
		serviceAccount,
		clusterRole,
		clusterRoleBinding,
		service,
		configMap,
		statefulSet,
	}

	if d.config.VPAEnabled {
		objects = append(objects, d.getVPA(statefulSet.Name))
	}

	return registry.AddAllAndSerialize(objects...)
}

func (d *dataplanePrometheus) getServiceAccount() *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      componentName,
			Namespace: targetNamespace,
			Labels:    getLabels(),
		},
		AutomountServiceAccountToken: new(true),
	}
}

func (d *dataplanePrometheus) getClusterRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:   componentName,
			Labels: getLabels(),
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"nodes", "services", "endpoints", "pods"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"nodes/metrics"},
				Verbs:     []string{"get"},
			},
		},
	}
}

func (d *dataplanePrometheus) getClusterRoleBinding(clusterRoleName, serviceAccountName, serviceAccountNamespace string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:   componentName,
			Labels: getLabels(),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     clusterRoleName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      serviceAccountName,
				Namespace: serviceAccountNamespace,
			},
		},
	}
}

func (d *dataplanePrometheus) getConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      componentName,
			Namespace: targetNamespace,
			Labels:    getLabels(),
		},
		Data: map[string]string{
			"config.yaml": d.getPrometheusConfig(),
		},
	}
}

func (d *dataplanePrometheus) getService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      componentName,
			Namespace: targetNamespace,
			Labels:    getLabels(),
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:     metricsPortName,
					Port:     portMetrics,
					Protocol: corev1.ProtocolTCP,
				},
			},
			Selector: getLabels(),
		},
	}
}

func (d *dataplanePrometheus) getStatefulSet(configMapName, serviceAccountName string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      componentName,
			Namespace: targetNamespace,
			Labels: utils.MergeStringMaps(getLabels(), map[string]string{
				v1beta1constants.GardenRole:     v1beta1constants.GardenRoleObservability,
				managedresources.LabelKeyOrigin: managedresources.LabelValueGardener,
			}),
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: componentName,
			Replicas:    &d.config.Replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: getLabels(),
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: utils.MergeStringMaps(getLabels(), map[string]string{
						v1beta1constants.GardenRole:                            v1beta1constants.GardenRoleObservability,
						managedresources.LabelKeyOrigin:                        managedresources.LabelValueGardener,
						v1beta1constants.LabelNetworkPolicyShootFromSeed:       v1beta1constants.LabelNetworkPolicyAllowed,
						v1beta1constants.LabelNetworkPolicyShootToAPIServer:    v1beta1constants.LabelNetworkPolicyAllowed,
						v1beta1constants.LabelNetworkPolicyShootToKubelet:      v1beta1constants.LabelNetworkPolicyAllowed,
						v1beta1constants.LabelNetworkPolicyToDNS:               v1beta1constants.LabelNetworkPolicyAllowed,
						v1beta1constants.LabelNetworkPolicyShootToNodeExporter: v1beta1constants.LabelNetworkPolicyAllowed,
					}),
				},
				Spec: corev1.PodSpec{
					PriorityClassName:  v1beta1constants.PriorityClassNameShootSystem700,
					ServiceAccountName: serviceAccountName,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: new(true),
						RunAsUser:    new(int64(65534)),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{
						{
							Name:            componentName,
							Image:           d.config.Image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Args: []string{
								"--config.file=" + mountPathConfig + "/config.yaml",
								"--storage.tsdb.path=" + mountPathData,
								"--storage.tsdb.retention.time=2h",
								"--web.enable-lifecycle",
							},
							Ports: []corev1.ContainerPort{
								{
									Name:          metricsPortName,
									ContainerPort: portMetrics,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("150m"),
									corev1.ResourceMemory: resource.MustParse("400M"),
								},
							},
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: new(false),
								ReadOnlyRootFilesystem:   new(true),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      volumeNameConfig,
									MountPath: mountPathConfig,
									ReadOnly:  true,
								},
								{
									Name:      volumeNameData,
									MountPath: mountPathData,
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: volumeNameConfig,
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: configMapName,
									},
								},
							},
						},
						{
							Name: volumeNameData,
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{
									SizeLimit: new(resource.MustParse("2Gi")),
								},
							},
						},
					},
				},
			},
		},
	}
}

func (d *dataplanePrometheus) getPrometheusConfig() string {
	return prometheusConfig
}

func (d *dataplanePrometheus) getVPA(statefulSetName string) *vpaautoscalingv1.VerticalPodAutoscaler {
	return &vpaautoscalingv1.VerticalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      componentName,
			Namespace: targetNamespace,
			Labels:    getLabels(),
		},
		Spec: vpaautoscalingv1.VerticalPodAutoscalerSpec{
			TargetRef: &autoscalingv1.CrossVersionObjectReference{
				APIVersion: appsv1.SchemeGroupVersion.String(),
				Kind:       "StatefulSet",
				Name:       statefulSetName,
			},
			UpdatePolicy: &vpaautoscalingv1.PodUpdatePolicy{
				UpdateMode: new(vpaautoscalingv1.UpdateModeInPlaceOrRecreate),
			},
			ResourcePolicy: &vpaautoscalingv1.PodResourcePolicy{
				ContainerPolicies: []vpaautoscalingv1.ContainerResourcePolicy{
					{
						ContainerName: componentName,
						MinAllowed: corev1.ResourceList{
							corev1.ResourceMemory: resource.MustParse("100M"),
							corev1.ResourceCPU:    resource.MustParse("150m"),
						},
						ControlledValues: new(vpaautoscalingv1.ContainerControlledValuesRequestsOnly),
					},
					{
						ContainerName: vpaautoscalingv1.DefaultContainerResourcePolicy,
						Mode:          new(vpaautoscalingv1.ContainerScalingModeOff),
					},
				},
			},
		},
	}
}

func getLabels() map[string]string {
	return map[string]string{
		labelKeyComponent: componentName,
	}
}
