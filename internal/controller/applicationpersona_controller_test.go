/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

var _ = Describe("ApplicationPersona Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		applicationpersona := &dorguv1.ApplicationPersona{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind ApplicationPersona")
			err := k8sClient.Get(ctx, typeNamespacedName, applicationpersona)
			if err != nil && errors.IsNotFound(err) {
				resource := &dorguv1.ApplicationPersona{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: dorguv1.ApplicationPersonaSpec{
						Name: resourceName,
						Type: "api",
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &dorguv1.ApplicationPersona{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance ApplicationPersona")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &ApplicationPersonaReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("When reconciling with a matching deployment", func() {
		const resourceName = "app-with-deployment"
		const deploymentName = "app-with-deployment"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			By("creating a Deployment")
			replicas := int32(3)
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: "default",
					Labels: map[string]string{
						"app":                    deploymentName,
						"app.kubernetes.io/name": resourceName,
					},
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": deploymentName,
						},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app": deploymentName,
							},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "app",
									Image: "nginx:latest",
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("100m"),
											corev1.ResourceMemory: resource.MustParse("128Mi"),
										},
										Limits: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("500m"),
											corev1.ResourceMemory: resource.MustParse("512Mi"),
										},
									},
									LivenessProbe: &corev1.Probe{
										ProbeHandler: corev1.ProbeHandler{
											HTTPGet: &corev1.HTTPGetAction{
												Path: "/health",
												Port: intstr.FromInt(8080),
											},
										},
									},
								},
							},
						},
					},
				},
			}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: "default"}, &appsv1.Deployment{})
			if errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
			}

			By("creating the ApplicationPersona")
			minReplicas := int32(2)
			maxReplicas := int32(5)
			persona := &dorguv1.ApplicationPersona{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
				Spec: dorguv1.ApplicationPersonaSpec{
					Name: resourceName,
					Type: "api",
					Resources: &dorguv1.ResourceConstraints{
						Limits: &dorguv1.ResourceValues{
							CPU:    "1000m",
							Memory: "1Gi",
						},
					},
					Scaling: &dorguv1.ScalingSpec{
						MinReplicas: &minReplicas,
						MaxReplicas: &maxReplicas,
					},
					Health: &dorguv1.HealthSpec{
						LivenessPath: "/health",
					},
				},
			}
			err = k8sClient.Get(ctx, typeNamespacedName, &dorguv1.ApplicationPersona{})
			if errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, persona)).To(Succeed())
			}
		})

		AfterEach(func() {
			By("Cleanup the Deployment")
			deployment := &appsv1.Deployment{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: "default"}, deployment)
			if err == nil {
				Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
			}

			By("Cleanup the ApplicationPersona")
			persona := &dorguv1.ApplicationPersona{}
			err = k8sClient.Get(ctx, typeNamespacedName, persona)
			if err == nil {
				Expect(k8sClient.Delete(ctx, persona)).To(Succeed())
			}
		})

		It("should find matching deployment and validate successfully", func() {
			By("Reconciling the resource")
			controllerReconciler := &ApplicationPersonaReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking the status")
			persona := &dorguv1.ApplicationPersona{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, persona)).To(Succeed())
			Expect(persona.Status.Phase).NotTo(Equal("Pending"))
			Expect(persona.Status.Validation).NotTo(BeNil())
			Expect(persona.Status.Validation.Passed).To(BeTrue())
			Expect(persona.Status.Deployments).NotTo(BeNil())
			Expect(persona.Status.Deployments.Current).NotTo(BeEmpty())
		})
	})

	Context("When reconciling with validation errors", func() {
		const resourceName = "app-with-errors"
		const deploymentName = "app-with-errors"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			By("creating a Deployment with issues")
			replicas := int32(1)
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: "default",
					Labels: map[string]string{
						"app":                    deploymentName,
						"app.kubernetes.io/name": resourceName,
					},
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": deploymentName,
						},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app": deploymentName,
							},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "app",
									Image: "nginx:latest",
								},
							},
						},
					},
				},
			}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: "default"}, &appsv1.Deployment{})
			if errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
			}

			By("creating the ApplicationPersona with strict requirements")
			minReplicas := int32(3)
			persona := &dorguv1.ApplicationPersona{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
				Spec: dorguv1.ApplicationPersonaSpec{
					Name: resourceName,
					Type: "api",
					Scaling: &dorguv1.ScalingSpec{
						MinReplicas: &minReplicas,
					},
					Health: &dorguv1.HealthSpec{
						LivenessPath: "/health",
					},
				},
			}
			err = k8sClient.Get(ctx, typeNamespacedName, &dorguv1.ApplicationPersona{})
			if errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, persona)).To(Succeed())
			}
		})

		AfterEach(func() {
			By("Cleanup the Deployment")
			deployment := &appsv1.Deployment{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: "default"}, deployment)
			if err == nil {
				Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
			}

			By("Cleanup the ApplicationPersona")
			persona := &dorguv1.ApplicationPersona{}
			err = k8sClient.Get(ctx, typeNamespacedName, persona)
			if err == nil {
				Expect(k8sClient.Delete(ctx, persona)).To(Succeed())
			}
		})

		It("should detect validation errors", func() {
			By("Reconciling the resource")
			controllerReconciler := &ApplicationPersonaReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking the status has validation issues")
			persona := &dorguv1.ApplicationPersona{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, persona)).To(Succeed())
			Expect(persona.Status.Validation).NotTo(BeNil())
			Expect(persona.Status.Validation.Passed).To(BeFalse())
			Expect(len(persona.Status.Validation.Issues)).To(BeNumerically(">", 0))
		})

		It("should set Degraded phase when validation fails", func() {
			By("Reconciling the resource")
			controllerReconciler := &ApplicationPersonaReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking the phase is Degraded")
			persona := &dorguv1.ApplicationPersona{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, persona)).To(Succeed())
			Expect(persona.Status.Phase).To(Equal("Degraded"))
		})
	})

	Context("When reconciling a deleted resource", func() {
		const resourceName = "deleted-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		It("should handle not found gracefully", func() {
			By("Reconciling a non-existent resource")
			controllerReconciler := &ApplicationPersonaReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())
		})
	})

	Context("When reconciling without a matching deployment", func() {
		const resourceName = "app-no-deployment"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			By("creating the ApplicationPersona without a deployment")
			persona := &dorguv1.ApplicationPersona{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
				Spec: dorguv1.ApplicationPersonaSpec{
					Name: resourceName,
					Type: "api",
				},
			}
			err := k8sClient.Get(ctx, typeNamespacedName, &dorguv1.ApplicationPersona{})
			if errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, persona)).To(Succeed())
			}
		})

		AfterEach(func() {
			By("Cleanup the ApplicationPersona")
			persona := &dorguv1.ApplicationPersona{}
			err := k8sClient.Get(ctx, typeNamespacedName, persona)
			if err == nil {
				Expect(k8sClient.Delete(ctx, persona)).To(Succeed())
			}
		})

		It("should set Pending phase when no deployment found", func() {
			By("Reconciling the resource")
			controllerReconciler := &ApplicationPersonaReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking the phase is Pending")
			persona := &dorguv1.ApplicationPersona{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, persona)).To(Succeed())
			Expect(persona.Status.Phase).To(Equal("Pending"))
		})
	})

	Context("When reconciling with security policy violations", func() {
		const resourceName = "app-security-violation"
		const deploymentName = "app-security-violation"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			By("creating a Deployment without security context")
			replicas := int32(1)
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: "default",
					Labels: map[string]string{
						"app":                    deploymentName,
						"app.kubernetes.io/name": resourceName,
					},
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": deploymentName,
						},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app": deploymentName,
							},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "app",
									Image: "nginx:latest",
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU: resource.MustParse("100m"),
										},
									},
								},
							},
						},
					},
				},
			}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: "default"}, &appsv1.Deployment{})
			if errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
			}

			By("creating the ApplicationPersona with security requirements")
			runAsNonRoot := true
			persona := &dorguv1.ApplicationPersona{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
				Spec: dorguv1.ApplicationPersonaSpec{
					Name: resourceName,
					Type: "api",
					Policies: &dorguv1.PoliciesSpec{
						Security: &dorguv1.SecurityPolicy{
							RunAsNonRoot: &runAsNonRoot,
						},
					},
				},
			}
			err = k8sClient.Get(ctx, typeNamespacedName, &dorguv1.ApplicationPersona{})
			if errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, persona)).To(Succeed())
			}
		})

		AfterEach(func() {
			By("Cleanup the Deployment")
			deployment := &appsv1.Deployment{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: "default"}, deployment)
			if err == nil {
				Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
			}

			By("Cleanup the ApplicationPersona")
			persona := &dorguv1.ApplicationPersona{}
			err = k8sClient.Get(ctx, typeNamespacedName, persona)
			if err == nil {
				Expect(k8sClient.Delete(ctx, persona)).To(Succeed())
			}
		})

		It("should detect security policy violations", func() {
			By("Reconciling the resource")
			controllerReconciler := &ApplicationPersonaReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking the status has security issues")
			persona := &dorguv1.ApplicationPersona{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, persona)).To(Succeed())
			Expect(persona.Status.Validation).NotTo(BeNil())
			Expect(persona.Status.Validation.Passed).To(BeFalse())

			hasSecurityIssue := false
			for _, issue := range persona.Status.Validation.Issues {
				if issue.Severity == "error" && issue.Field == "spec.template.spec.securityContext.runAsNonRoot" {
					hasSecurityIssue = true
					break
				}
			}
			Expect(hasSecurityIssue).To(BeTrue())
		})
	})
})
