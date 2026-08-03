// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package dataplaneprometheus_test

import (
	"context"
	"fmt"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	"github.com/onsi/gomega/types"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	vpaautoscalingv1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	resourcesv1alpha1 "github.com/gardener/gardener/pkg/apis/resources/v1alpha1"
	"github.com/gardener/gardener/pkg/client/kubernetes"
	"github.com/gardener/gardener/pkg/component"
	. "github.com/gardener/gardener/pkg/component/observability/monitoring/dataplaneprometheus"
	"github.com/gardener/gardener/pkg/resourcemanager/controller/garbagecollector/references"
	"github.com/gardener/gardener/pkg/utils/retry"
	retryfake "github.com/gardener/gardener/pkg/utils/retry/fake"
	"github.com/gardener/gardener/pkg/utils/test"
	. "github.com/gardener/gardener/pkg/utils/test/matchers"
)

var _ = Describe("DataplanePrometheus", func() {
	var (
		ctx = context.Background()

		namespace = "some-namespace"
		image     = "some-prometheus-image:some-tag"

		c            client.Client
		comp         component.DeployWaiter
		consistOf    func(...client.Object) types.GomegaMatcher
		customConfig Config

		managedResource       *resourcesv1alpha1.ManagedResource
		managedResourceSecret *corev1.Secret

		fakeOps *retryfake.Ops
	)

	BeforeEach(func() {
		format.MaxDepth = 100000
		format.MaxLength = 100000
		format.TruncateThreshold = 100000
		c = fakeclient.NewClientBuilder().WithScheme(kubernetes.SeedScheme).Build()

		customConfig = Config{
			Image:    image,
			Replicas: 1,
		}

		comp = New(c, namespace, customConfig)
		consistOf = NewManagedResourceConsistOfObjectsMatcher(c)

		managedResource = &resourcesv1alpha1.ManagedResource{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "shoot-core-dataplane-prometheus",
				Namespace: namespace,
			},
		}

		managedResourceSecret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "managedresource-shoot-core-dataplane-prometheus",
				Namespace: namespace,
			},
		}

		fakeOps = &retryfake.Ops{MaxAttempts: 1}
		DeferCleanup(test.WithVars(
			&retry.Until, fakeOps.Until,
			&retry.UntilTimeout, fakeOps.UntilTimeout,
		))
	})

	Describe("#Deploy", func() {
		It("should successfully deploy all resources", func() {
			Expect(comp.Deploy(ctx)).To(Succeed())

			Expect(c.Get(ctx, client.ObjectKeyFromObject(managedResource), managedResource)).To(Succeed())

			// this is test setup, it's not the behaviour being tested, so it OK to panic
			if err := references.InjectAnnotations(managedResource); err != nil {
				panic(err)
			}

			managedResourceSecret.Name = managedResource.Spec.SecretRefs[0].Name
			Expect(c.Get(ctx, client.ObjectKeyFromObject(managedResourceSecret), managedResourceSecret)).To(Succeed())

			Expect(managedResource).To(consistOf(
				getServiceAccount(),
				getClusterRole(),
				getClusterRoleBinding(),
				getService(),
				getConfigMap(),
				getStatefulSet(image),
			))
		})

		It("should additionally deploy a VerticalPodAutoscaler when VPA is enabled", func() {
			comp = New(c, namespace, Config{Image: image, Replicas: 1, VPAEnabled: true})

			Expect(comp.Deploy(ctx)).To(Succeed())

			Expect(c.Get(ctx, client.ObjectKeyFromObject(managedResource), managedResource)).To(Succeed())

			// this is test setup, it's not the behaviour being tested, so it OK to panic
			if err := references.InjectAnnotations(managedResource); err != nil {
				panic(err)
			}

			managedResourceSecret.Name = managedResource.Spec.SecretRefs[0].Name
			Expect(c.Get(ctx, client.ObjectKeyFromObject(managedResourceSecret), managedResourceSecret)).To(Succeed())

			Expect(managedResource).To(consistOf(
				getServiceAccount(),
				getClusterRole(),
				getClusterRoleBinding(),
				getService(),
				getConfigMap(),
				getStatefulSet(image),
				getVPA(),
			))
		})
	})

	Describe("#Destroy", func() {
		It("should delete managed resources", func() {
			Expect(c.Create(ctx, managedResource)).To(Succeed())
			Expect(c.Create(ctx, managedResourceSecret)).To(Succeed())

			Expect(comp.Destroy(ctx)).To(Succeed())

			Expect(c.Get(ctx, client.ObjectKeyFromObject(managedResource), managedResource)).To(BeNotFoundError())
			Expect(c.Get(ctx, client.ObjectKeyFromObject(managedResourceSecret), managedResourceSecret)).To(BeNotFoundError())
		})
	})

	Describe("#Wait", func() {
		It("should fail when MR does not exist", func() {
			Expect(comp.Wait(ctx)).To(MatchError(ContainSubstring("not found")))
		})

		It("should succeed when MR is healthy", func() {
			Expect(c.Create(ctx, &resourcesv1alpha1.ManagedResource{
				ObjectMeta: metav1.ObjectMeta{
					Name:       managedResource.Name,
					Namespace:  namespace,
					Generation: 1,
				},
				Status: resourcesv1alpha1.ManagedResourceStatus{
					ObservedGeneration: 1,
					Conditions: []gardencorev1beta1.Condition{
						{Type: resourcesv1alpha1.ResourcesApplied, Status: gardencorev1beta1.ConditionTrue},
						{Type: resourcesv1alpha1.ResourcesHealthy, Status: gardencorev1beta1.ConditionTrue},
					},
				},
			})).To(Succeed())

			Expect(comp.Wait(ctx)).To(Succeed())
		})
	})

	Describe("#WaitCleanup", func() {
		It("should succeed when deleted", func() {
			Expect(comp.WaitCleanup(ctx)).To(Succeed())
		})
	})
})

func getServiceAccount() *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dataplane-prometheus",
			Namespace: "kube-system",
			Labels: map[string]string{
				"component": "dataplane-prometheus",
			},
		},
		AutomountServiceAccountToken: ptr.To(true),
	}
}

func getClusterRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: "dataplane-prometheus",
			Labels: map[string]string{
				"component": "dataplane-prometheus",
			},
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{
					"nodes",
					"services",
					"endpoints",
					"pods",
				},
				Verbs: []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{
					"nodes/metrics",
				},
				Verbs: []string{"get"},
			},
		},
	}
}

func getClusterRoleBinding() *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "dataplane-prometheus",
			Labels: map[string]string{
				"component": "dataplane-prometheus",
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "dataplane-prometheus",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "dataplane-prometheus",
				Namespace: "kube-system",
			},
		},
	}
}

func getService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dataplane-prometheus",
			Namespace: "kube-system",
			Labels: map[string]string{
				"component": "dataplane-prometheus",
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"component": "dataplane-prometheus",
			},
			Ports: []corev1.ServicePort{
				{
					Name:     "metrics",
					Port:     9090,
					Protocol: corev1.ProtocolTCP,
				},
			},
		},
	}
}

func getConfigMap() *corev1.ConfigMap {
	const configPath = "testdata/dataplane-prometheus-config.test.yaml"

	data, err := os.ReadFile(configPath)
	if err != nil {
		panic(fmt.Sprintf("failed to read config file %s: %v", configPath, err))
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dataplane-prometheus",
			Namespace: "kube-system",
			Labels: map[string]string{
				"component": "dataplane-prometheus",
			},
		},
		Data: map[string]string{
			"config.yaml": string(data),
		},
	}
}

func getStatefulSet(image string) *appsv1.StatefulSet {
	replicas := int32(1)

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dataplane-prometheus",
			Namespace: "kube-system",
			Labels: map[string]string{
				"component":           "dataplane-prometheus",
				"gardener.cloud/role": "observability",
				"origin":              "gardener",
			},
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: "dataplane-prometheus",
			Replicas:    &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"component": "dataplane-prometheus",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"component":                                  "dataplane-prometheus",
						"gardener.cloud/role":                        "observability",
						"networking.gardener.cloud/from-seed":        "allowed",
						"networking.gardener.cloud/to-apiserver":     "allowed",
						"networking.gardener.cloud/to-dns":           "allowed",
						"networking.gardener.cloud/to-kubelet":       "allowed",
						"networking.gardener.cloud/to-node-exporter": "allowed",
						"origin": "gardener",
					},
				},
				Spec: corev1.PodSpec{
					PriorityClassName:  "gardener-shoot-system-700",
					ServiceAccountName: "dataplane-prometheus",
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr.To(true),
						RunAsUser:    ptr.To(int64(65534)),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{
						{
							Name:  "dataplane-prometheus",
							Image: image,
							Args: []string{
								"--config.file=/etc/prometheus/config.yaml",
								"--storage.tsdb.path=/data",
								"--storage.tsdb.retention.time=2h",
								"--web.enable-lifecycle",
							},
							Ports: []corev1.ContainerPort{
								{Name: "metrics", ContainerPort: 9090, Protocol: corev1.ProtocolTCP},
							},
							ImagePullPolicy: corev1.PullIfNotPresent,
							SecurityContext: &corev1.SecurityContext{
								ReadOnlyRootFilesystem:   ptr.To(true),
								AllowPrivilegeEscalation: ptr.To(false),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "config",
									ReadOnly:  true,
									MountPath: "/etc/prometheus",
								},
								{
									Name:      "data",
									MountPath: "/data",
								},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("150m"),
									corev1.ResourceMemory: resource.MustParse("400M"),
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "dataplane-prometheus",
									},
								},
							},
						},
						{
							Name: "data",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{
									SizeLimit: ptr.To(resource.MustParse("2Gi")),
								},
							},
						},
					},
				},
			},
		},
	}
}

func getVPA() *vpaautoscalingv1.VerticalPodAutoscaler {
	return &vpaautoscalingv1.VerticalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dataplane-prometheus",
			Namespace: "kube-system",
			Labels: map[string]string{
				"component": "dataplane-prometheus",
			},
		},
		Spec: vpaautoscalingv1.VerticalPodAutoscalerSpec{
			TargetRef: &autoscalingv1.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "StatefulSet",
				Name:       "dataplane-prometheus",
			},
			UpdatePolicy: &vpaautoscalingv1.PodUpdatePolicy{
				UpdateMode: ptr.To(vpaautoscalingv1.UpdateModeInPlaceOrRecreate),
			},
			ResourcePolicy: &vpaautoscalingv1.PodResourcePolicy{
				ContainerPolicies: []vpaautoscalingv1.ContainerResourcePolicy{
					{
						ContainerName: "dataplane-prometheus",
						MinAllowed: corev1.ResourceList{
							corev1.ResourceMemory: resource.MustParse("100M"),
							corev1.ResourceCPU:    resource.MustParse("150m"),
						},
						ControlledValues: ptr.To(vpaautoscalingv1.ContainerControlledValuesRequestsOnly),
					},
					{
						ContainerName: vpaautoscalingv1.DefaultContainerResourcePolicy,
						Mode:          ptr.To(vpaautoscalingv1.ContainerScalingModeOff),
					},
				},
			},
		},
	}
}
