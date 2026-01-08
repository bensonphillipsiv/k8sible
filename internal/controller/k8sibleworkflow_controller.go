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

	"github.com/robfig/cron/v3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	k8siblev1alpha1 "github.com/bensonphillipsiv/k8sible.git/api/v1alpha1"
	"github.com/bensonphillipsiv/k8sible.git/internal/git"
)

const (
	DefaultMaxRetries int32 = 3

	// Event reasons
	EventReasonJobStarted         = "JobStarted"
	EventReasonJobSucceeded       = "JobSucceeded"
	EventReasonJobFailed          = "JobFailed"
	EventReasonReconcileTrigger   = "ReconcileTriggeredApply"
	EventReasonScheduledRun       = "ScheduledRun"
	EventReasonNewCommit          = "NewCommit"
	EventReasonCooldownStarted    = "CooldownStarted"
	EventReasonCooldownEnded      = "CooldownEnded"
	EventReasonCooldownBlocked    = "CooldownBlocked"
	EventReasonPermanentlyBlocked = "PermanentlyBlocked"
	EventReasonCycleDetected      = "CycleDetected"
)

// K8sibleWorkflowReconciler reconciles a K8sibleWorkflow object
type K8sibleWorkflowReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	GitClient *git.Client
	Recorder  record.EventRecorder
}

// Playbook represents a playbook source and its type
type Playbook struct {
	Source     git.Source
	Type       string // "apply" or "reconcile"
	Schedule   string // cron schedule (optional)
	MaxRetries int32
}

// +kubebuilder:rbac:groups=k8sible.core.k8sible.io,resources=k8sibleworkflows,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8sible.core.k8sible.io,resources=k8sibleworkflows/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=k8sible.core.k8sible.io,resources=k8sibleworkflows/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *K8sibleWorkflowReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := logf.FromContext(ctx)

	workflow := &k8siblev1alpha1.K8sibleWorkflow{}
	if err := r.Get(ctx, req.NamespacedName, workflow); err != nil {
		l.Error(err, "unable to fetch K8sibleWorkflow")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Initialize status
	r.initializeStatus(workflow)

	l.Info("K8sibleWorkflow",
		"name", workflow.Name,
		"repo", workflow.Spec.Source.Repository,
		"ref", workflow.Spec.Source.Reference,
		"pendingPlaybooks", workflow.Status.PendingPlaybooks,
		"inCooldown", workflow.Status.FailureCycleStatus.InCooldown,
	)

	// Track if status needs updating
	statusUpdated := false

	// PHASE 1: Check cooldown expiry
	if r.checkCooldownExpiry(ctx, workflow) {
		statusUpdated = true
		l.Info("Cooldown expired, queued apply for retry")
	}

	// PHASE 2: Process any completed jobs first
	if updated, err := r.processCompletedJobs(ctx, workflow); err != nil {
		l.Error(err, "failed to process completed jobs")
		return ctrl.Result{}, err
	} else if updated {
		statusUpdated = true
	}

	// PHASE 3: Check if ANY job is currently running - if so, just wait
	running, runningType, err := r.getRunningJob(ctx, workflow)
	if err != nil {
		l.Error(err, "failed to check for running jobs")
		return ctrl.Result{}, err
	}

	if running {
		l.Info("Job running, waiting", "type", runningType)
		if statusUpdated {
			if err := r.Status().Update(ctx, workflow); err != nil {
				l.Error(err, "failed to update workflow status")
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: time.Second * 30}, nil
	}

	// PHASE 4: No job running - determine what should run next
	if updated, err := r.determineNextJob(ctx, workflow); err != nil {
		l.Error(err, "failed to determine next job")
		return ctrl.Result{}, err
	} else if updated {
		statusUpdated = true
	}

	// PHASE 5: Start the next pending job (if any)
	if updated, err := r.startNextPendingJob(ctx, workflow); err != nil {
		l.Error(err, "failed to start next pending job")
		return ctrl.Result{}, err
	} else if updated {
		statusUpdated = true
	}

	// Save status
	if statusUpdated {
		if err := r.Status().Update(ctx, workflow); err != nil {
			l.Error(err, "failed to update workflow status")
			return ctrl.Result{}, err
		}
	}

	// Calculate requeue time
	playbooks := r.buildPlaybookList(workflow)
	requeueAfter := r.calculateRequeueAfter(workflow, playbooks)

	l.Info("Reconcile complete", "requeueAfter", requeueAfter, "pendingPlaybooks", workflow.Status.PendingPlaybooks)
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// initializeStatus ensures all status fields are initialized
func (r *K8sibleWorkflowReconciler) initializeStatus(workflow *k8siblev1alpha1.K8sibleWorkflow) {
	if workflow.Status.FailureCycleStatus == nil {
		workflow.Status.FailureCycleStatus = &k8siblev1alpha1.FailureCycleStatus{}
	}
}

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

// determineNextJob figures out what should run next based on commits, schedules, and state
func (r *K8sibleWorkflowReconciler) determineNextJob(ctx context.Context, workflow *k8siblev1alpha1.K8sibleWorkflow) (bool, error) {
	l := logf.FromContext(ctx)
	statusUpdated := false

	// If there's already something pending, don't add more
	if len(workflow.Status.PendingPlaybooks) > 0 {
		l.Info("Pending playbooks exist, not determining next job", "pending", workflow.Status.PendingPlaybooks)
		return false, nil
	}

	// Get git token
	gitToken, err := r.getGitToken(ctx, workflow)
	if err != nil {
		return false, err
	}

	// Build playbook list
	playbooks := r.buildPlaybookList(workflow)

	// Priority order for what to run next:
	// 1. New commits (apply first, then reconcile)
	// 2. Scheduled runs (apply first, then reconcile)

	// Check for new commits
	for _, playbook := range playbooks {
		commitInfo, err := r.GitClient.GetLatestCommit(ctx, playbook.Source, gitToken)
		if err != nil {
			l.Error(err, "failed to get latest commit", "type", playbook.Type)
			continue
		}

		updated, err := r.checkForNewCommit(ctx, workflow, commitInfo, playbook)
		if err != nil {
			return false, err
		}

		if updated {
			// New commit - queue this playbook
			workflow.Status.PendingPlaybooks = append(workflow.Status.PendingPlaybooks, playbook.Type)
			statusUpdated = true

			// Clear cooldown on new commit
			if workflow.Status.FailureCycleStatus.InCooldown {
				r.clearCooldown(ctx, workflow, "new commit detected")
			}
			workflow.Status.FailureCycleStatus.ReconcileTriggeredApply = false
			workflow.Status.FailureCycleStatus.ConsecutiveReconcileFailuresAfterApply = 0

			r.Recorder.Eventf(workflow, corev1.EventTypeNormal, EventReasonNewCommit,
				"New commit detected for %s playbook: %s", playbook.Type, commitInfo.SHA[:7])

			l.Info("Queued playbook for new commit", "type", playbook.Type, "sha", commitInfo.SHA)

			// If apply has new commit, also queue reconcile after it
			if playbook.Type == "apply" && workflow.Spec.Reconcile != nil {
				workflow.Status.PendingPlaybooks = append(workflow.Status.PendingPlaybooks, "reconcile")
				l.Info("Also queued reconcile after apply")
			}

			// Sort to ensure apply runs before reconcile
			sortPendingPlaybooks(workflow.Status.PendingPlaybooks)
			return statusUpdated, nil
		}
	}

	// Check scheduled runs (only if nothing pending from commits)
	for _, playbook := range playbooks {
		if playbook.Schedule == "" {
			continue
		}

		shouldRun, err := r.isScheduledRunDue(workflow, playbook)
		if err != nil {
			l.Error(err, "failed to check schedule", "type", playbook.Type)
			continue
		}

		if shouldRun {
			workflow.Status.PendingPlaybooks = append(workflow.Status.PendingPlaybooks, playbook.Type)
			statusUpdated = true

			// Update last scheduled time
			now := metav1.Now()
			if playbook.Type == "apply" {
				if workflow.Status.ApplyScheduleStatus == nil {
					workflow.Status.ApplyScheduleStatus = &k8siblev1alpha1.ScheduleStatus{}
				}
				workflow.Status.ApplyScheduleStatus.LastScheduledTime = &now

				// Apply schedule also triggers reconcile after
				if workflow.Spec.Reconcile != nil {
					workflow.Status.PendingPlaybooks = append(workflow.Status.PendingPlaybooks, "reconcile")
					l.Info("Also queued reconcile after scheduled apply")
				}
			} else if playbook.Type == "reconcile" {
				if workflow.Status.ReconcileScheduleStatus == nil {
					workflow.Status.ReconcileScheduleStatus = &k8siblev1alpha1.ScheduleStatus{}
				}
				workflow.Status.ReconcileScheduleStatus.LastScheduledTime = &now
			}

			r.Recorder.Eventf(workflow, corev1.EventTypeNormal, EventReasonScheduledRun,
				"Scheduled run triggered for %s playbook", playbook.Type)

			l.Info("Queued playbook for schedule", "type", playbook.Type)

			sortPendingPlaybooks(workflow.Status.PendingPlaybooks)
			return statusUpdated, nil
		}
	}

	return statusUpdated, nil
}

// startNextPendingJob creates a job for the next pending playbook
func (r *K8sibleWorkflowReconciler) startNextPendingJob(ctx context.Context, workflow *k8siblev1alpha1.K8sibleWorkflow) (bool, error) {
	l := logf.FromContext(ctx)

	if len(workflow.Status.PendingPlaybooks) == 0 {
		return false, nil
	}

	// Get the next playbook type
	playbookType := workflow.Status.PendingPlaybooks[0]

	// Build the playbook
	playbook, err := r.getPlaybook(workflow, playbookType)
	if err != nil {
		l.Error(err, "failed to get playbook", "type", playbookType)
		// Remove invalid playbook from queue
		workflow.Status.PendingPlaybooks = workflow.Status.PendingPlaybooks[1:]
		return true, nil
	}

	// Create the job
	if err := r.createJob(ctx, workflow, playbook); err != nil {
		return false, err
	}

	r.Recorder.Eventf(workflow, corev1.EventTypeNormal, EventReasonJobStarted,
		"Started %s job for playbook %s (maxRetries: %d)", playbook.Type, playbook.Source.Path, playbook.MaxRetries)

	// Remove from pending
	workflow.Status.PendingPlaybooks = workflow.Status.PendingPlaybooks[1:]

	l.Info("Started job", "type", playbookType, "remaining", workflow.Status.PendingPlaybooks)

	return true, nil
}

// buildPlaybookList creates the list of playbooks from the workflow spec
func (r *K8sibleWorkflowReconciler) buildPlaybookList(workflow *k8siblev1alpha1.K8sibleWorkflow) []Playbook {
	playbooks := []Playbook{
		{
			Source: git.Source{
				Repository: workflow.Spec.Source.Repository,
				Reference:  workflow.Spec.Source.Reference,
				Path:       workflow.Spec.Apply.Path,
			},
			Type:       "apply",
			Schedule:   workflow.Spec.Apply.Schedule,
			MaxRetries: getMaxRetries(workflow.Spec.Apply.MaxRetries),
		},
	}

	if workflow.Spec.Reconcile != nil {
		playbooks = append(playbooks, Playbook{
			Source: git.Source{
				Repository: workflow.Spec.Source.Repository,
				Reference:  workflow.Spec.Source.Reference,
				Path:       workflow.Spec.Reconcile.Path,
			},
			Type:       "reconcile",
			Schedule:   workflow.Spec.Reconcile.Schedule,
			MaxRetries: getMaxRetries(workflow.Spec.Reconcile.MaxRetries),
		})
	}

	return playbooks
}

// getPlaybook returns a playbook by type
func (r *K8sibleWorkflowReconciler) getPlaybook(workflow *k8siblev1alpha1.K8sibleWorkflow, playbookType string) (Playbook, error) {
	playbooks := r.buildPlaybookList(workflow)
	for _, p := range playbooks {
		if p.Type == playbookType {
			return p, nil
		}
	}
	return Playbook{}, fmt.Errorf("playbook type %s not found", playbookType)
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

		if playbookType == "apply" {
			// Apply succeeded - queue reconcile if configured and not already pending
			if workflow.Spec.Reconcile != nil && !contains(workflow.Status.PendingPlaybooks, "reconcile") {
				workflow.Status.PendingPlaybooks = append(workflow.Status.PendingPlaybooks, "reconcile")
				l.Info("Queued reconcile after successful apply")
			}
		} else if playbookType == "reconcile" {
			// Reconcile succeeded - clear cycle tracking
			workflow.Status.FailureCycleStatus.ReconcileTriggeredApply = false
			workflow.Status.FailureCycleStatus.ConsecutiveReconcileFailuresAfterApply = 0
			l.Info("Reconcile succeeded, cleared cycle tracking")
		}

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

	// Check cooldown
	if r.shouldBlockDueToCooldown(ctx, workflow) {
		l.Info("Not retrying due to active cooldown")
		return nil
	}

	// Handle failure
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

	// If failureCycleCooldown is not set, always retry immediately (can cause cycles)
	if workflow.Spec.FailureCycleCooldown == nil {
		l.Info("Apply failed, retrying immediately (no failureCycleCooldown set)")
		r.Recorder.Eventf(workflow, corev1.EventTypeWarning, EventReasonJobFailed,
			"Apply job failed, retrying immediately (no cooldown configured)")

		if !contains(workflow.Status.PendingPlaybooks, "apply") {
			workflow.Status.PendingPlaybooks = append(workflow.Status.PendingPlaybooks, "apply")
		}
		return
	}

	// If failureCycleCooldown is 0, never retry
	if workflow.Spec.FailureCycleCooldown.Duration == 0 {
		l.Info("Apply failed, blocking permanently (failureCycleCooldown=0)")
		r.startCooldown(ctx, workflow, "apply job failed after max retries")
		r.Recorder.Eventf(workflow, corev1.EventTypeWarning, EventReasonPermanentlyBlocked,
			"Apply job failed after max retries. Manual intervention required (failureCycleCooldown=0)")
		return
	}

	// Start cooldown
	l.Info("Apply failed, starting cooldown", "duration", workflow.Spec.FailureCycleCooldown.Duration)
	r.startCooldown(ctx, workflow, fmt.Sprintf("apply job failed: %s", failureMessage))

	r.Recorder.Eventf(workflow, corev1.EventTypeWarning, EventReasonJobFailed,
		"Apply job failed after max retries. Cooldown for %s before retry.",
		workflow.Spec.FailureCycleCooldown.Duration)
}

// handleReconcileFailure handles a reconcile job failure after max retries
func (r *K8sibleWorkflowReconciler) handleReconcileFailure(ctx context.Context, workflow *k8siblev1alpha1.K8sibleWorkflow, failureMessage string) {
	l := logf.FromContext(ctx)

	// Check if this is a cycle: reconcile-triggered apply succeeded but reconcile failed again
	if workflow.Status.FailureCycleStatus.ReconcileTriggeredApply {
		workflow.Status.FailureCycleStatus.ConsecutiveReconcileFailuresAfterApply++

		l.Info("Cycle detected: reconcile failed after reconcile-triggered apply",
			"consecutiveFailures", workflow.Status.FailureCycleStatus.ConsecutiveReconcileFailuresAfterApply)

		r.Recorder.Eventf(workflow, corev1.EventTypeWarning, EventReasonCycleDetected,
			"Cycle detected: reconcile failed after successful apply")

		// If failureCycleCooldown is not set, continue the cycle
		if workflow.Spec.FailureCycleCooldown == nil {
			l.Info("Cycle detected but no cooldown set, continuing cycle")
			r.queueApplyAndReconcile(ctx, workflow)
			return
		}

		// If failureCycleCooldown is 0, block permanently
		if workflow.Spec.FailureCycleCooldown.Duration == 0 {
			l.Info("Cycle detected, blocking permanently (failureCycleCooldown=0)")
			r.startCooldown(ctx, workflow, "reconcile-apply cycle detected")
			workflow.Status.FailureCycleStatus.ReconcileTriggeredApply = false
			return
		}

		// Start cooldown to break the cycle
		l.Info("Cycle detected, starting cooldown", "duration", workflow.Spec.FailureCycleCooldown.Duration)
		r.startCooldown(ctx, workflow, "reconcile-apply cycle detected")
		workflow.Status.FailureCycleStatus.ReconcileTriggeredApply = false
		return
	}

	// First reconcile failure (not a cycle yet) - trigger apply
	l.Info("Reconcile failed, triggering apply")
	r.queueApplyAndReconcile(ctx, workflow)
}

// queueApplyAndReconcile queues apply and reconcile after a reconcile failure
func (r *K8sibleWorkflowReconciler) queueApplyAndReconcile(ctx context.Context, workflow *k8siblev1alpha1.K8sibleWorkflow) {
	l := logf.FromContext(ctx)

	r.Recorder.Eventf(workflow, corev1.EventTypeWarning, EventReasonReconcileTrigger,
		"Reconcile failed, triggering apply")

	workflow.Status.FailureCycleStatus.ReconcileTriggeredApply = true
	workflow.Status.PendingPlaybooks = []string{"apply"}

	if workflow.Spec.Reconcile != nil {
		workflow.Status.PendingPlaybooks = append(workflow.Status.PendingPlaybooks, "reconcile")
	}

	l.Info("Queued apply and reconcile", "pending", workflow.Status.PendingPlaybooks)
}

// checkCooldownExpiry checks if cooldown has expired and clears it
// Returns true if cooldown was cleared (status needs update)
func (r *K8sibleWorkflowReconciler) checkCooldownExpiry(ctx context.Context, workflow *k8siblev1alpha1.K8sibleWorkflow) bool {
	if !workflow.Status.FailureCycleStatus.InCooldown {
		return false
	}

	if workflow.Spec.FailureCycleCooldown == nil {
		r.clearCooldown(ctx, workflow, "cooldown not configured")
		return true
	}

	if workflow.Status.FailureCycleStatus.CooldownStartTime == nil {
		r.clearCooldown(ctx, workflow, "no cooldown start time")
		return true
	}

	cooldownDuration := workflow.Spec.FailureCycleCooldown.Duration
	if cooldownDuration == 0 {
		// Cooldown is 0, meaning never retry - stay in cooldown permanently
		return false
	}

	elapsed := time.Since(workflow.Status.FailureCycleStatus.CooldownStartTime.Time)
	if elapsed >= cooldownDuration {
		r.clearCooldown(ctx, workflow, "cooldown period expired")

		// Queue the apply job to retry after cooldown expires
		if !contains(workflow.Status.PendingPlaybooks, "apply") {
			workflow.Status.PendingPlaybooks = append(workflow.Status.PendingPlaybooks, "apply")
		}

		return true
	}

	return false
}

// clearCooldown clears the cooldown state
func (r *K8sibleWorkflowReconciler) clearCooldown(ctx context.Context, workflow *k8siblev1alpha1.K8sibleWorkflow, reason string) {
	l := logf.FromContext(ctx)

	workflow.Status.FailureCycleStatus.InCooldown = false
	workflow.Status.FailureCycleStatus.CooldownStartTime = nil
	workflow.Status.FailureCycleStatus.CooldownReason = ""

	l.Info("Cooldown cleared", "reason", reason)
	r.Recorder.Eventf(workflow, corev1.EventTypeNormal, EventReasonCooldownEnded,
		"Cooldown period ended: %s", reason)
}

// startCooldown initiates the cooldown period
func (r *K8sibleWorkflowReconciler) startCooldown(ctx context.Context, workflow *k8siblev1alpha1.K8sibleWorkflow, reason string) {
	l := logf.FromContext(ctx)

	now := metav1.Now()
	workflow.Status.FailureCycleStatus.InCooldown = true
	workflow.Status.FailureCycleStatus.CooldownStartTime = &now
	workflow.Status.FailureCycleStatus.CooldownReason = reason

	if workflow.Spec.FailureCycleCooldown != nil && workflow.Spec.FailureCycleCooldown.Duration == 0 {
		l.Info("Cooldown started (permanent - manual intervention required)", "reason", reason)
		r.Recorder.Eventf(workflow, corev1.EventTypeWarning, EventReasonPermanentlyBlocked,
			"Workflow blocked permanently until manual intervention: %s", reason)
	} else if workflow.Spec.FailureCycleCooldown != nil {
		l.Info("Cooldown started", "reason", reason, "duration", workflow.Spec.FailureCycleCooldown.Duration)
		r.Recorder.Eventf(workflow, corev1.EventTypeWarning, EventReasonCooldownStarted,
			"Cooldown started for %s: %s", workflow.Spec.FailureCycleCooldown.Duration, reason)
	}
}

// shouldBlockDueToCooldown determines if a failure-triggered retry should be blocked
func (r *K8sibleWorkflowReconciler) shouldBlockDueToCooldown(ctx context.Context, workflow *k8siblev1alpha1.K8sibleWorkflow) bool {
	l := logf.FromContext(ctx)

	if !workflow.Status.FailureCycleStatus.InCooldown {
		return false
	}

	// Check if cooldown is permanent (duration = 0)
	if workflow.Spec.FailureCycleCooldown != nil && workflow.Spec.FailureCycleCooldown.Duration == 0 {
		l.Info("Blocking retry due to permanent cooldown (failureCycleCooldown=0)")
		r.Recorder.Eventf(workflow, corev1.EventTypeWarning, EventReasonCooldownBlocked,
			"Retry blocked: permanent cooldown active, manual intervention required")
		return true
	}

	l.Info("Blocking retry due to active cooldown",
		"cooldownReason", workflow.Status.FailureCycleStatus.CooldownReason,
		"cooldownStartTime", workflow.Status.FailureCycleStatus.CooldownStartTime)

	if workflow.Spec.FailureCycleCooldown != nil && workflow.Status.FailureCycleStatus.CooldownStartTime != nil {
		r.Recorder.Eventf(workflow, corev1.EventTypeWarning, EventReasonCooldownBlocked,
			"Retry blocked: cooldown active until %s",
			workflow.Status.FailureCycleStatus.CooldownStartTime.Add(workflow.Spec.FailureCycleCooldown.Duration))
	}

	return true
}

// isScheduledRunDue checks if a scheduled run should be triggered
func (r *K8sibleWorkflowReconciler) isScheduledRunDue(workflow *k8siblev1alpha1.K8sibleWorkflow, playbook Playbook) (bool, error) {
	if playbook.Schedule == "" {
		return false, nil
	}

	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	schedule, err := parser.Parse(playbook.Schedule)
	if err != nil {
		return false, fmt.Errorf("failed to parse cron schedule: %w", err)
	}

	now := time.Now()

	// Get last scheduled time
	var lastScheduled time.Time
	if playbook.Type == "apply" && workflow.Status.ApplyScheduleStatus != nil &&
		workflow.Status.ApplyScheduleStatus.LastScheduledTime != nil {
		lastScheduled = workflow.Status.ApplyScheduleStatus.LastScheduledTime.Time
	} else if playbook.Type == "reconcile" && workflow.Status.ReconcileScheduleStatus != nil &&
		workflow.Status.ReconcileScheduleStatus.LastScheduledTime != nil {
		lastScheduled = workflow.Status.ReconcileScheduleStatus.LastScheduledTime.Time
	} else {
		// If never scheduled, use workflow creation time as baseline
		lastScheduled = workflow.CreationTimestamp.Time
	}

	// Get next scheduled time after last run
	nextRun := schedule.Next(lastScheduled)

	// If next run time has passed, we should trigger
	return now.After(nextRun), nil
}

// calculateRequeueAfter determines the optimal requeue duration based on schedules and cooldown
func (r *K8sibleWorkflowReconciler) calculateRequeueAfter(workflow *k8siblev1alpha1.K8sibleWorkflow, playbooks []Playbook) time.Duration {
	// Default requeue interval for commit checking
	minRequeue := time.Minute * 3

	// If there are pending jobs, requeue sooner
	if len(workflow.Status.PendingPlaybooks) > 0 {
		return time.Second * 30
	}

	// If in cooldown, consider cooldown expiry time
	if workflow.Status.FailureCycleStatus.InCooldown &&
		workflow.Spec.FailureCycleCooldown != nil &&
		workflow.Spec.FailureCycleCooldown.Duration > 0 &&
		workflow.Status.FailureCycleStatus.CooldownStartTime != nil {

		cooldownEnd := workflow.Status.FailureCycleStatus.CooldownStartTime.Add(workflow.Spec.FailureCycleCooldown.Duration)
		timeUntilCooldownEnd := time.Until(cooldownEnd)

		if timeUntilCooldownEnd > 0 && timeUntilCooldownEnd < minRequeue {
			minRequeue = timeUntilCooldownEnd + time.Second*5 // Add buffer
		}
	}

	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	now := time.Now()

	for _, playbook := range playbooks {
		if playbook.Schedule == "" {
			continue
		}

		schedule, err := parser.Parse(playbook.Schedule)
		if err != nil {
			continue
		}

		// Get last scheduled time
		var lastScheduled time.Time
		if playbook.Type == "apply" && workflow.Status.ApplyScheduleStatus != nil &&
			workflow.Status.ApplyScheduleStatus.LastScheduledTime != nil {
			lastScheduled = workflow.Status.ApplyScheduleStatus.LastScheduledTime.Time
		} else if playbook.Type == "reconcile" && workflow.Status.ReconcileScheduleStatus != nil &&
			workflow.Status.ReconcileScheduleStatus.LastScheduledTime != nil {
			lastScheduled = workflow.Status.ReconcileScheduleStatus.LastScheduledTime.Time
		} else {
			lastScheduled = workflow.CreationTimestamp.Time
		}

		nextRun := schedule.Next(lastScheduled)
		timeUntilNext := nextRun.Sub(now)

		// If next run is in the past, requeue soon
		if timeUntilNext <= 0 {
			timeUntilNext = time.Second * 10
		}

		// Add a small buffer to ensure we're past the scheduled time
		timeUntilNext += time.Second * 5

		if timeUntilNext < minRequeue {
			minRequeue = timeUntilNext
		}
	}

	return minRequeue
}

// checkForNewCommit checks if there's a new commit and updates status
func (r *K8sibleWorkflowReconciler) checkForNewCommit(ctx context.Context, workflow *k8siblev1alpha1.K8sibleWorkflow, commitInfo *git.CommitInfo, playbook Playbook) (bool, error) {
	l := logf.FromContext(ctx)

	var currentCommit *k8siblev1alpha1.CommitStatus
	if playbook.Type == "apply" {
		currentCommit = workflow.Status.ApplyCommit
	} else if playbook.Type == "reconcile" {
		currentCommit = workflow.Status.ReconcileCommit
	}

	// Check if commit changed
	if currentCommit != nil && currentCommit.SHA == commitInfo.SHA {
		return false, nil
	}

	// New commit detected - update status
	l.Info("New commit detected",
		"type", playbook.Type,
		"oldSHA", getSHA(currentCommit),
		"newSHA", commitInfo.SHA)

	commitDate := metav1.NewTime(commitInfo.Date)
	newCommitStatus := &k8siblev1alpha1.CommitStatus{
		SHA:     commitInfo.SHA,
		Date:    &commitDate,
		Message: truncateMessage(commitInfo.Message, 100),
	}

	if playbook.Type == "apply" {
		workflow.Status.ApplyCommit = newCommitStatus
	} else if playbook.Type == "reconcile" {
		workflow.Status.ReconcileCommit = newCommitStatus
	}

	return true, nil
}

// getGitToken retrieves the git token from the referenced secret
func (r *K8sibleWorkflowReconciler) getGitToken(ctx context.Context, workflow *k8siblev1alpha1.K8sibleWorkflow) (string, error) {
	if workflow.Spec.Source.SecretRef == nil {
		return "", nil
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{
		Name:      workflow.Spec.Source.SecretRef.Name,
		Namespace: workflow.Namespace,
	}, secret); err != nil {
		return "", fmt.Errorf("failed to get secret %s: %w", workflow.Spec.Source.SecretRef.Name, err)
	}

	key := workflow.Spec.Source.SecretRef.Key
	if key == "" {
		key = "token"
	}

	token, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("key %s not found in secret %s", key, workflow.Spec.Source.SecretRef.Name)
	}

	return string(token), nil
}

// getMaxRetries returns the max retries value or the default
func getMaxRetries(maxRetries *int32) int32 {
	if maxRetries == nil {
		return DefaultMaxRetries
	}
	return *maxRetries
}

func getSHA(commit *k8siblev1alpha1.CommitStatus) string {
	if commit == nil {
		return "<none>"
	}
	return commit.SHA
}

func truncateMessage(msg string, maxLen int) string {
	if len(msg) <= maxLen {
		return msg
	}
	return msg[:maxLen-3] + "..."
}

func buildAnsiblePullArgs(source git.Source) []string {
	args := []string{
		"-U", source.Repository,
	}

	if source.Reference != "" {
		args = append(args, "-C", source.Reference)
	}

	args = append(args, source.Path)

	return args
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func sortPendingPlaybooks(playbooks []string) {
	for i := 0; i < len(playbooks); i++ {
		if playbooks[i] == "reconcile" {
			for j := i + 1; j < len(playbooks); j++ {
				if playbooks[j] == "apply" {
					playbooks[i], playbooks[j] = playbooks[j], playbooks[i]
					return
				}
			}
		}
	}
}

func (r *K8sibleWorkflowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&k8siblev1alpha1.K8sibleWorkflow{}).
		Owns(&batchv1.Job{}).
		Named("k8sibleworkflow").
		Complete(r)
}
