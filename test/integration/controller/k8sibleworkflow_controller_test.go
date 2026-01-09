/*
Copyright 2025 bensonphillipsiv.

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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	k8siblev1alpha1 "github.com/bensonphillipsiv/k8sible.git/api/v1alpha1"
)

const (
	timeout  = time.Second * 30
	interval = time.Millisecond * 250
)

// createTestWorkflow creates a K8sibleWorkflow for testing
func createTestWorkflow(name string) *k8siblev1alpha1.K8sibleWorkflow {
	const namespace = "default"
	return &k8siblev1alpha1.K8sibleWorkflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: k8siblev1alpha1.K8sibleWorkflowSpec{
			Source: k8siblev1alpha1.SourceSpec{
				Repository: "https://github.com/test/repo",
				Reference:  "main",
			},
			Apply: k8siblev1alpha1.PlaybookSpec{
				Path: "playbooks/apply.yaml",
			},
		},
	}
}

var _ = Describe("K8sibleWorkflow Controller", func() {
	Context("When creating a K8sibleWorkflow resource", func() {
		const (
			workflowName      = "test-workflow-init"
			workflowNamespace = "default"
		)

		var workflow *k8siblev1alpha1.K8sibleWorkflow

		BeforeEach(func() {
			// Reset mock git client to default state
			mockGitClient.SetCommit("abc123def456", "test commit", time.Now())
		})

		AfterEach(func() {
			// Clean up the workflow
			if workflow != nil {
				_ = k8sClient.Delete(ctx, workflow)
				// Wait for deletion
				Eventually(func() bool {
					err := k8sClient.Get(ctx, types.NamespacedName{
						Name:      workflowName,
						Namespace: workflowNamespace,
					}, &k8siblev1alpha1.K8sibleWorkflow{})
					return err != nil
				}, timeout, interval).Should(BeTrue())
			}
		})

		// Test: Workflow creation initializes status
		// Requirements: 1.1 - WHEN a valid K8sibleWorkflow resource is created,
		// THE K8sible_Controller SHALL initialize the status fields
		It("should initialize status fields when workflow is created", func() {
			By("Creating a new K8sibleWorkflow")
			workflow = createTestWorkflow(workflowName)
			Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

			By("Waiting for the controller to initialize status")
			workflowLookupKey := types.NamespacedName{Name: workflowName, Namespace: workflowNamespace}
			createdWorkflow := &k8siblev1alpha1.K8sibleWorkflow{}

			// The controller should detect the new commit and update ApplyCommit status
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, workflowLookupKey, createdWorkflow)).To(Succeed())
				g.Expect(createdWorkflow.Status.ApplyCommit).NotTo(BeNil())
				g.Expect(createdWorkflow.Status.ApplyCommit.SHA).To(Equal("abc123def456"))
			}, timeout, interval).Should(Succeed())
		})
	})

	Context("When a new commit is detected", func() {
		const (
			workflowName      = "test-workflow-commit"
			workflowNamespace = "default"
		)

		var workflow *k8siblev1alpha1.K8sibleWorkflow

		BeforeEach(func() {
			// Reset mock git client
			mockGitClient.SetCommit("initial123456", "initial commit", time.Now())
		})

		AfterEach(func() {
			// Clean up the workflow and any jobs
			if workflow != nil {
				// Delete jobs first
				jobList := &batchv1.JobList{}
				_ = k8sClient.List(ctx, jobList)
				for _, job := range jobList.Items {
					_ = k8sClient.Delete(ctx, &job)
				}

				_ = k8sClient.Delete(ctx, workflow)
				Eventually(func() bool {
					err := k8sClient.Get(ctx, types.NamespacedName{
						Name:      workflowName,
						Namespace: workflowNamespace,
					}, &k8siblev1alpha1.K8sibleWorkflow{})
					return err != nil
				}, timeout, interval).Should(BeTrue())
			}
		})

		// Test: New commit creates job
		// Requirements: 1.2 - WHEN a new commit is detected,
		// THE K8sible_Controller SHALL create an Ansible job
		It("should create a job when a new commit is detected", func() {
			By("Creating a K8sibleWorkflow")
			workflow = createTestWorkflow(workflowName)
			Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

			By("Waiting for the controller to detect the commit and create a job")
			// The controller should create a job for the initial commit
			Eventually(func(g Gomega) {
				jobList := &batchv1.JobList{}
				g.Expect(k8sClient.List(ctx, jobList)).To(Succeed())

				// Find jobs for our workflow
				var workflowJobs []batchv1.Job
				for _, job := range jobList.Items {
					if job.Labels["app.kubernetes.io/k8sible-workflow"] == workflowName {
						workflowJobs = append(workflowJobs, job)
					}
				}
				g.Expect(workflowJobs).ToNot(BeEmpty())
				g.Expect(workflowJobs[0].Labels["app.kubernetes.io/k8sible-type"]).To(Equal("apply"))
			}, timeout, interval).Should(Succeed())
		})
	})

	Context("When a job completes", func() {
		const (
			workflowName      = "test-workflow-complete"
			workflowNamespace = "default"
		)

		var workflow *k8siblev1alpha1.K8sibleWorkflow

		BeforeEach(func() {
			// Reset mock git client
			mockGitClient.SetCommit("complete123456", "complete commit", time.Now())
		})

		AfterEach(func() {
			// Clean up the workflow and any jobs
			if workflow != nil {
				jobList := &batchv1.JobList{}
				_ = k8sClient.List(ctx, jobList)
				for _, job := range jobList.Items {
					_ = k8sClient.Delete(ctx, &job)
				}

				_ = k8sClient.Delete(ctx, workflow)
				Eventually(func() bool {
					err := k8sClient.Get(ctx, types.NamespacedName{
						Name:      workflowName,
						Namespace: workflowNamespace,
					}, &k8siblev1alpha1.K8sibleWorkflow{})
					return err != nil
				}, timeout, interval).Should(BeTrue())
			}
		})

		// Test: Job completion updates status
		// Requirements: 2.1 - WHEN a job completes successfully,
		// THE K8sible_Controller SHALL update the lastSuccessfulRun status
		It("should update lastSuccessfulRun status when job completes successfully", func() {
			By("Creating a K8sibleWorkflow")
			workflow = createTestWorkflow(workflowName)
			Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

			By("Waiting for a job to be created")
			var createdJob *batchv1.Job
			Eventually(func(g Gomega) {
				jobList := &batchv1.JobList{}
				g.Expect(k8sClient.List(ctx, jobList)).To(Succeed())

				for i := range jobList.Items {
					if jobList.Items[i].Labels["app.kubernetes.io/k8sible-workflow"] == workflowName {
						createdJob = &jobList.Items[i]
						break
					}
				}
				g.Expect(createdJob).NotTo(BeNil())
			}, timeout, interval).Should(Succeed())

			By("Simulating job completion")
			// Update job status to indicate completion with all required fields
			now := metav1.Now()
			createdJob.Status.Succeeded = 1
			createdJob.Status.StartTime = &now
			createdJob.Status.CompletionTime = &now
			createdJob.Status.Conditions = []batchv1.JobCondition{
				{
					Type:               batchv1.JobSuccessCriteriaMet,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: now,
				},
				{
					Type:               batchv1.JobComplete,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: now,
				},
			}
			Expect(k8sClient.Status().Update(ctx, createdJob)).To(Succeed())

			By("Verifying the workflow status is updated")
			workflowLookupKey := types.NamespacedName{Name: workflowName, Namespace: workflowNamespace}
			Eventually(func(g Gomega) {
				updatedWorkflow := &k8siblev1alpha1.K8sibleWorkflow{}
				g.Expect(k8sClient.Get(ctx, workflowLookupKey, updatedWorkflow)).To(Succeed())
				g.Expect(updatedWorkflow.Status.LastSuccessfulRun).NotTo(BeNil())
				g.Expect(updatedWorkflow.Status.LastSuccessfulRun.Succeeded).To(BeTrue())
				g.Expect(updatedWorkflow.Status.LastSuccessfulRun.Type).To(Equal("apply"))
			}, timeout, interval).Should(Succeed())
		})

		// Test: Job failure updates status
		// Requirements: 2.2 - WHEN a job fails,
		// THE K8sible_Controller SHALL update the lastFailedRun status
		It("should update lastFailedRun status when job fails", func() {
			By("Creating a K8sibleWorkflow")
			workflow = createTestWorkflow(workflowName + "-fail")
			Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

			By("Waiting for a job to be created")
			var createdJob *batchv1.Job
			Eventually(func(g Gomega) {
				jobList := &batchv1.JobList{}
				g.Expect(k8sClient.List(ctx, jobList)).To(Succeed())

				for i := range jobList.Items {
					if jobList.Items[i].Labels["app.kubernetes.io/k8sible-workflow"] == workflowName+"-fail" {
						createdJob = &jobList.Items[i]
						break
					}
				}
				g.Expect(createdJob).NotTo(BeNil())
			}, timeout, interval).Should(Succeed())

			By("Simulating job failure")
			// Re-fetch the job to get the latest version
			jobKey := types.NamespacedName{Name: createdJob.Name, Namespace: createdJob.Namespace}
			Expect(k8sClient.Get(ctx, jobKey, createdJob)).To(Succeed())

			now := metav1.Now()
			createdJob.Status.Active = 0
			createdJob.Status.Failed = 1
			createdJob.Status.StartTime = &now
			createdJob.Status.Conditions = []batchv1.JobCondition{
				{
					Type:               batchv1.JobFailureTarget,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: now,
				},
				{
					Type:               batchv1.JobFailed,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: now,
					Message:            "Job failed due to test",
				},
			}
			Expect(k8sClient.Status().Update(ctx, createdJob)).To(Succeed())

			By("Verifying the workflow status is updated")
			workflowLookupKey := types.NamespacedName{Name: workflowName + "-fail", Namespace: workflowNamespace}
			Eventually(func(g Gomega) {
				updatedWorkflow := &k8siblev1alpha1.K8sibleWorkflow{}
				g.Expect(k8sClient.Get(ctx, workflowLookupKey, updatedWorkflow)).To(Succeed())
				g.Expect(updatedWorkflow.Status.LastFailedRun).NotTo(BeNil())
				g.Expect(updatedWorkflow.Status.LastFailedRun.Succeeded).To(BeFalse())
				g.Expect(updatedWorkflow.Status.LastFailedRun.Type).To(Equal("apply"))
			}, timeout, interval).Should(Succeed())
		})
	})
})
