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
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	k8siblev1alpha1 "github.com/bensonphillipsiv/k8sible.git/api/v1alpha1"
)

// getRunningJob returns if a job is running and which type
func (r *K8sibleWorkflowReconciler) getRunningJob(ctx context.Context, workflow *k8siblev1alpha1.K8sibleWorkflow) (bool, string, error) {
	jobList := &batchv1.JobList{}
	if err := r.List(ctx, jobList,
		client.InNamespace(workflow.Namespace),
		client.MatchingLabels{
			"app.kubernetes.io/k8sible-workflow": workflow.Name,
		}); err != nil {
		return false, "", err
	}

	for _, job := range jobList.Items {
		// Active pods running
		if job.Status.Active > 0 {
			return true, job.Labels["app.kubernetes.io/k8sible-type"], nil
		}

		// Job exists but not yet terminal (might be starting or retrying)
		isTerminal := false
		for _, condition := range job.Status.Conditions {
			if (condition.Type == batchv1.JobComplete || condition.Type == batchv1.JobFailed) &&
				condition.Status == corev1.ConditionTrue {
				isTerminal = true
				break
			}
		}

		isProcessed := job.Annotations != nil && job.Annotations["k8sible.io/processed"] == "true"

		if !isTerminal && !isProcessed {
			return true, job.Labels["app.kubernetes.io/k8sible-type"], nil
		}
	}

	return false, "", nil
}

// processCompletedJobs handles all completed jobs
func (r *K8sibleWorkflowReconciler) processCompletedJobs(ctx context.Context, workflow *k8siblev1alpha1.K8sibleWorkflow) (bool, error) {
	l := logf.FromContext(ctx)
	statusUpdated := false

	jobList := &batchv1.JobList{}
	if err := r.List(ctx, jobList,
		client.InNamespace(workflow.Namespace),
		client.MatchingLabels{
			"app.kubernetes.io/k8sible-workflow": workflow.Name,
		}); err != nil {
		return false, err
	}

	for i := range jobList.Items {
		job := &jobList.Items[i]

		// Skip active jobs
		if job.Status.Active > 0 {
			continue
		}

		// Skip already processed jobs
		if job.Annotations != nil && job.Annotations["k8sible.io/processed"] == "true" {
			continue
		}

		// Check for terminal condition
		var succeeded bool
		var isTerminal bool

		for _, condition := range job.Status.Conditions {
			if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
				succeeded = true
				isTerminal = true
				break
			}
			if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
				succeeded = false
				isTerminal = true
				break
			}
		}

		if !isTerminal {
			continue
		}

		l.Info("Processing completed job", "job", job.Name, "succeeded", succeeded)

		if err := r.handleJobCompletion(ctx, workflow, job, succeeded); err != nil {
			return false, err
		}
		statusUpdated = true
	}

	return statusUpdated, nil
}

// createJob creates a new ansible-pull job
func (r *K8sibleWorkflowReconciler) createJob(ctx context.Context, workflow *k8siblev1alpha1.K8sibleWorkflow, playbook Playbook) error {
	l := logf.FromContext(ctx)

	jobName := fmt.Sprintf("ansible-%s-%s-%d", workflow.Name, playbook.Type, time.Now().Unix())
	ansiblePullArgs := buildAnsiblePullArgs(playbook.Source)

	l.Info("Creating job", "job", jobName, "type", playbook.Type, "backoffLimit", playbook.MaxRetries)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: workflow.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by":       "k8sible",
				"app.kubernetes.io/k8sible-workflow": workflow.Name,
				"app.kubernetes.io/k8sible-type":     playbook.Type,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(playbook.MaxRetries),
			TTLSecondsAfterFinished: ptr.To(int32(3600)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/managed-by":       "k8sible",
						"app.kubernetes.io/k8sible-workflow": workflow.Name,
						"app.kubernetes.io/k8sible-type":     playbook.Type,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "ansible",
							Image:   "quay.io/ansible/ansible-runner:latest",
							Command: []string{"ansible-pull"},
							Args:    ansiblePullArgs,
						},
					},
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(workflow, job, r.Scheme); err != nil {
		return err
	}

	return r.Create(ctx, job)
}

// handleJobCompletion processes a completed job
func (r *K8sibleWorkflowReconciler) handleJobCompletion(ctx context.Context, workflow *k8siblev1alpha1.K8sibleWorkflow, job *batchv1.Job, succeeded bool) error {
	l := logf.FromContext(ctx)

	playbookType := job.Labels["app.kubernetes.io/k8sible-type"]

	// Mark job as processed
	if job.Annotations == nil {
		job.Annotations = make(map[string]string)
	}
	job.Annotations["k8sible.io/processed"] = "true"
	if err := r.Update(ctx, job); err != nil {
		return fmt.Errorf("failed to mark job as processed: %w", err)
	}

	now := metav1.Now()

	if succeeded {
		l.Info("Job succeeded", "job", job.Name, "type", playbookType)

		workflow.Status.LastSuccessfulRun = &k8siblev1alpha1.PlaybookRunStatus{
			Type:      playbookType,
			StartTime: job.Status.StartTime,
			EndTime:   &now,
			Succeeded: true,
		}

		r.Recorder.Eventf(workflow, corev1.EventTypeNormal, EventReasonJobSucceeded,
			"Job %s completed successfully", job.Name)

		return nil
	}

	// Job failed
	var failureMessage string
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed {
			failureMessage = condition.Message
			break
		}
	}

	l.Info("Job failed", "job", job.Name, "type", playbookType, "message", failureMessage)

	workflow.Status.LastFailedRun = &k8siblev1alpha1.PlaybookRunStatus{
		Type:      playbookType,
		StartTime: job.Status.StartTime,
		EndTime:   &now,
		Succeeded: false,
		Message:   failureMessage,
	}

	r.Recorder.Eventf(workflow, corev1.EventTypeWarning, EventReasonJobFailed,
		"Job %s failed: %s", job.Name, failureMessage)

	// Handle failure based on playbook type
	if playbookType == "apply" {
		r.handleApplyFailure(ctx, workflow, failureMessage)
	} else if playbookType == "reconcile" {
		r.handleReconcileFailure(ctx, workflow, failureMessage)
	}

	return nil
}

// handleApplyFailure handles an apply job failure after max retries
func (r *K8sibleWorkflowReconciler) handleApplyFailure(ctx context.Context, workflow *k8siblev1alpha1.K8sibleWorkflow, failureMessage string) {
	l := logf.FromContext(ctx)

	l.Info("Apply failed, retrying immediately")
	r.Recorder.Eventf(workflow, corev1.EventTypeWarning, EventReasonJobFailed,
		"Apply job failed, retrying immediately")

	if !contains(workflow.Status.PendingPlaybooks, "apply") {
		workflow.Status.PendingPlaybooks = append(workflow.Status.PendingPlaybooks, "apply")
		workflow.Status.LastTriggerReason = TriggerReasonFailureRetry
	}
}

// handleReconcileFailure handles a reconcile job failure after max retries
func (r *K8sibleWorkflowReconciler) handleReconcileFailure(ctx context.Context, workflow *k8siblev1alpha1.K8sibleWorkflow, failureMessage string) {
	l := logf.FromContext(ctx)

	// Reconcile failure - trigger apply
	l.Info("Reconcile failed, triggering apply")
	r.queueApplyAndReconcile(ctx, workflow)
}

// queueApplyAndReconcile queues apply and reconcile after a reconcile failure
func (r *K8sibleWorkflowReconciler) queueApplyAndReconcile(ctx context.Context, workflow *k8siblev1alpha1.K8sibleWorkflow) {
	l := logf.FromContext(ctx)

	r.Recorder.Eventf(workflow, corev1.EventTypeWarning, EventReasonReconcileTrigger,
		"Reconcile failed, triggering apply")

	workflow.Status.PendingPlaybooks = []string{"apply"}
	workflow.Status.LastTriggerReason = TriggerReasonFailureRetry

	if workflow.Spec.Reconcile != nil {
		workflow.Status.PendingPlaybooks = append(workflow.Status.PendingPlaybooks, "reconcile")
	}

	l.Info("Queued apply and reconcile", "pending", workflow.Status.PendingPlaybooks)
}
