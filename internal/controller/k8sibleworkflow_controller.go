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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	EventReasonJobStarted       = "JobStarted"
	EventReasonJobSucceeded     = "JobSucceeded"
	EventReasonJobFailed        = "JobFailed"
	EventReasonReconcileTrigger = "ReconcileTriggeredApply"
	EventReasonScheduledRun     = "ScheduledRun"
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
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
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

	l.Info("K8sibleWorkflow",
		"name", workflow.Name,
		"repo", workflow.Spec.Source.Repository,
		"ref", workflow.Spec.Source.Reference,
		"applyPath", workflow.Spec.Apply.Path,
		"applySchedule", workflow.Spec.Apply.Schedule,
		"pendingPlaybooks", workflow.Status.PendingPlaybooks,
	)

	// Get git token from secret if configured
	gitToken, err := r.getGitToken(ctx, workflow)
	if err != nil {
		l.Error(err, "failed to get git token from secret")
		return ctrl.Result{}, err
	}

	// Build list of playbooks to check
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

	// Build a map of playbooks for easy lookup
	playbookMap := make(map[string]Playbook)
	for _, p := range playbooks {
		playbookMap[p.Type] = p
	}

	// Track if status needs updating
	statusUpdated := false

	// Check for completed jobs and process results
	completedJob, succeeded, err := r.checkCompletedJobs(ctx, workflow)
	if err != nil {
		l.Error(err, "failed to check completed jobs")
		return ctrl.Result{}, err
	}

	if completedJob != nil {
		statusUpdated = true
		if err := r.handleJobCompletion(ctx, workflow, completedJob, succeeded, playbookMap); err != nil {
			l.Error(err, "failed to handle job completion")
			return ctrl.Result{}, err
		}
	}

	// Process all playbooks - check commits and track pending jobs
	for _, playbook := range playbooks {
		commitInfo, err := r.GitClient.GetLatestCommit(ctx, playbook.Source, gitToken)
		if err != nil {
			l.Error(err, "failed to get latest commit",
				"type", playbook.Type,
				"path", playbook.Source.Path)
			return ctrl.Result{}, err
		}
		l.Info("Got latest commit",
			"type", playbook.Type,
			"path", playbook.Source.Path,
			"sha", commitInfo.SHA,
			"date", commitInfo.Date)

		// Reconcile the commit tracking ConfigMap
		configMapName, updated, err := r.reconcileConfigMap(ctx, workflow, commitInfo, playbook)
		if err != nil {
			l.Error(err, "failed to reconcile commit ConfigMap")
			return ctrl.Result{}, err
		}
		l.Info("Reconciled commit tracking ConfigMap",
			"configmap", configMapName,
			"updated", updated)

		// If commit changed, add to pending playbooks for immediate execution
		if updated && !contains(workflow.Status.PendingPlaybooks, playbook.Type) {
			workflow.Status.PendingPlaybooks = append(workflow.Status.PendingPlaybooks, playbook.Type)
			statusUpdated = true
			l.Info("Added playbook to pending queue (new commit detected)",
				"type", playbook.Type,
				"commit", commitInfo.SHA,
				"pendingPlaybooks", workflow.Status.PendingPlaybooks)
		}

		// Check if scheduled run is due
		if playbook.Schedule != "" {
			shouldRun, err := r.isScheduledRunDue(workflow, playbook)
			if err != nil {
				l.Error(err, "failed to check schedule",
					"type", playbook.Type,
					"schedule", playbook.Schedule)
				// Don't fail the reconcile, just skip scheduling
			} else if shouldRun && !contains(workflow.Status.PendingPlaybooks, playbook.Type) {
				workflow.Status.PendingPlaybooks = append(workflow.Status.PendingPlaybooks, playbook.Type)
				statusUpdated = true

				// Update last scheduled time
				now := metav1.Now()
				if playbook.Type == "apply" {
					if workflow.Status.ApplyScheduleStatus == nil {
						workflow.Status.ApplyScheduleStatus = &k8siblev1alpha1.ScheduleStatus{}
					}
					workflow.Status.ApplyScheduleStatus.LastScheduledTime = &now
				} else if playbook.Type == "reconcile" {
					if workflow.Status.ReconcileScheduleStatus == nil {
						workflow.Status.ReconcileScheduleStatus = &k8siblev1alpha1.ScheduleStatus{}
					}
					workflow.Status.ReconcileScheduleStatus.LastScheduledTime = &now
				}

				r.Recorder.Eventf(workflow, corev1.EventTypeNormal, EventReasonScheduledRun,
					"Scheduled run triggered for %s playbook", playbook.Type)

				l.Info("Added playbook to pending queue (scheduled run)",
					"type", playbook.Type,
					"schedule", playbook.Schedule,
					"pendingPlaybooks", workflow.Status.PendingPlaybooks)
			}
		}
	}

	// Check if any job is currently running for this workflow
	running, err := r.isAnyJobRunning(ctx, workflow)
	if err != nil {
		l.Error(err, "failed to check for running jobs")
		return ctrl.Result{}, err
	}

	if running {
		l.Info("A job is still running, will requeue to process pending jobs later",
			"pendingPlaybooks", workflow.Status.PendingPlaybooks)

		if statusUpdated {
			if err := r.Status().Update(ctx, workflow); err != nil {
				l.Error(err, "failed to update workflow status")
				return ctrl.Result{}, err
			}
		}

		return ctrl.Result{RequeueAfter: time.Second * 30}, nil
	}

	// No job running, process the pending playbook
	if len(workflow.Status.PendingPlaybooks) > 0 {
		sortPendingPlaybooks(workflow.Status.PendingPlaybooks)

		pendingType := workflow.Status.PendingPlaybooks[0]
		playbook, ok := playbookMap[pendingType]
		if !ok {
			l.Error(nil, "pending playbook type not found in playbook map",
				"type", pendingType)
			workflow.Status.PendingPlaybooks = workflow.Status.PendingPlaybooks[1:]
			if err := r.Status().Update(ctx, workflow); err != nil {
				l.Error(err, "failed to update workflow status")
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}

		if err := r.reconcileJob(ctx, workflow, playbook); err != nil {
			l.Error(err, "failed to reconcile Job")
			return ctrl.Result{}, err
		}

		r.Recorder.Eventf(workflow, corev1.EventTypeNormal, EventReasonJobStarted,
			"Started %s job for playbook %s (maxRetries: %d)", playbook.Type, playbook.Source.Path, playbook.MaxRetries)

		l.Info("Successfully created ansible-pull Job",
			"type", playbook.Type,
			"repo", playbook.Source.Repository,
			"path", playbook.Source.Path,
			"maxRetries", playbook.MaxRetries)

		workflow.Status.PendingPlaybooks = workflow.Status.PendingPlaybooks[1:]
		statusUpdated = true

		l.Info("Removed playbook from pending queue",
			"type", playbook.Type,
			"remainingPending", workflow.Status.PendingPlaybooks)
	}

	if statusUpdated {
		if err := r.Status().Update(ctx, workflow); err != nil {
			l.Error(err, "failed to update workflow status")
			return ctrl.Result{}, err
		}
	}

	// Calculate next requeue time based on schedules
	requeueAfter := r.calculateRequeueAfter(workflow, playbooks)

	if len(workflow.Status.PendingPlaybooks) > 0 {
		l.Info("More playbooks pending, will requeue to check job status",
			"pendingPlaybooks", workflow.Status.PendingPlaybooks)
		return ctrl.Result{RequeueAfter: time.Second * 30}, nil
	}

	l.Info("Reconcile complete, requeuing", "requeueAfter", requeueAfter)
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
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

// calculateRequeueAfter determines the optimal requeue duration based on schedules
func (r *K8sibleWorkflowReconciler) calculateRequeueAfter(workflow *k8siblev1alpha1.K8sibleWorkflow, playbooks []Playbook) time.Duration {
	// Default requeue interval for commit checking
	minRequeue := time.Minute * 3

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

func (r *K8sibleWorkflowReconciler) checkCompletedJobs(ctx context.Context, workflow *k8siblev1alpha1.K8sibleWorkflow) (*batchv1.Job, bool, error) {
	l := logf.FromContext(ctx)

	jobList := &batchv1.JobList{}
	if err := r.List(ctx, jobList,
		client.InNamespace(workflow.Namespace),
		client.MatchingLabels{
			"app.kubernetes.io/k8sible-workflow": workflow.Name,
		}); err != nil {
		return nil, false, err
	}

	for _, job := range jobList.Items {
		// Skip active jobs
		if job.Status.Active > 0 {
			continue
		}

		// Check if we've already processed this job
		if job.Annotations != nil && job.Annotations["k8sible.io/processed"] == "true" {
			continue
		}

		// Check job conditions
		for _, condition := range job.Status.Conditions {
			if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
				l.Info("Found completed job", "job", job.Name)
				return &job, true, nil
			}

			if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
				l.Info("Found failed job", "job", job.Name, "reason", condition.Reason, "message", condition.Message)
				return &job, false, nil
			}
		}
	}

	return nil, false, nil
}

func (r *K8sibleWorkflowReconciler) handleJobCompletion(ctx context.Context, workflow *k8siblev1alpha1.K8sibleWorkflow, job *batchv1.Job, succeeded bool, playbookMap map[string]Playbook) error {
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

	if succeeded {
		l.Info("Job succeeded", "job", job.Name, "type", playbookType)

		// Update last successful run
		now := metav1.Now()
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

	// Job failed (after all retries via BackoffLimit)
	var failureMessage string
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed {
			failureMessage = condition.Message
			break
		}
	}

	l.Info("Job failed after all retries",
		"job", job.Name,
		"type", playbookType,
		"message", failureMessage)

	// Update last failed run
	now := metav1.Now()
	workflow.Status.LastFailedRun = &k8siblev1alpha1.PlaybookRunStatus{
		Type:      playbookType,
		StartTime: job.Status.StartTime,
		EndTime:   &now,
		Succeeded: false,
		Message:   failureMessage,
	}

	r.Recorder.Eventf(workflow, corev1.EventTypeWarning, EventReasonJobFailed,
		"Job %s failed after all retries: %s", job.Name, failureMessage)

	if playbookType == "reconcile" {
		// Reconcile job failed after max retries - trigger apply
		r.Recorder.Eventf(workflow, corev1.EventTypeWarning, EventReasonReconcileTrigger,
			"Reconcile job failed, triggering apply job")

		// Queue apply followed by reconcile
		workflow.Status.PendingPlaybooks = []string{}

		if _, ok := playbookMap["apply"]; ok {
			workflow.Status.PendingPlaybooks = append(workflow.Status.PendingPlaybooks, "apply")
		}
		if _, ok := playbookMap["reconcile"]; ok {
			workflow.Status.PendingPlaybooks = append(workflow.Status.PendingPlaybooks, "reconcile")
		}

		l.Info("Queued apply and reconcile after reconcile failure",
			"pendingPlaybooks", workflow.Status.PendingPlaybooks)
	}

	return nil
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

func (r *K8sibleWorkflowReconciler) isAnyJobRunning(ctx context.Context, workflow *k8siblev1alpha1.K8sibleWorkflow) (bool, error) {
	jobList := &batchv1.JobList{}
	if err := r.List(ctx, jobList,
		client.InNamespace(workflow.Namespace),
		client.MatchingLabels{
			"app.kubernetes.io/k8sible-workflow": workflow.Name,
		}); err != nil {
		return false, err
	}

	for _, job := range jobList.Items {
		// Check if job is active or still has retries pending
		if job.Status.Active > 0 {
			logf.FromContext(ctx).Info("Found running job",
				"job", job.Name,
				"active", job.Status.Active)
			return true, nil
		}

		// Also check if job hasn't reached a terminal condition yet
		hasTerminalCondition := false
		for _, condition := range job.Status.Conditions {
			if (condition.Type == batchv1.JobComplete || condition.Type == batchv1.JobFailed) &&
				condition.Status == corev1.ConditionTrue {
				hasTerminalCondition = true
				break
			}
		}

		// If job exists but hasn't completed or failed, it might be retrying
		if !hasTerminalCondition && job.Annotations["k8sible.io/processed"] != "true" {
			logf.FromContext(ctx).Info("Found job pending retry",
				"job", job.Name,
				"failed", job.Status.Failed)
			return true, nil
		}
	}

	return false, nil
}

func (r *K8sibleWorkflowReconciler) reconcileConfigMap(ctx context.Context, workflow *k8siblev1alpha1.K8sibleWorkflow, commitInfo *git.CommitInfo, playbook Playbook) (string, bool, error) {
	l := logf.FromContext(ctx)
	configMapName := workflow.Name + "-" + playbook.Type + "-commit"

	existingConfigMap := &corev1.ConfigMap{}
	err := r.Get(ctx, client.ObjectKey{Name: configMapName, Namespace: workflow.Namespace}, existingConfigMap)

	if err == nil {
		existingCommit := existingConfigMap.Data["commit"]
		if existingCommit == commitInfo.SHA {
			l.Info("Commit unchanged, skipping update",
				"configmap", configMapName,
				"commit", commitInfo.SHA)
			return configMapName, false, nil
		}

		l.Info("New commit detected, updating",
			"configmap", configMapName,
			"oldCommit", existingCommit,
			"newCommit", commitInfo.SHA)

		existingConfigMap.Data = map[string]string{
			"commit":  commitInfo.SHA,
			"date":    commitInfo.Date.Format(time.RFC3339),
			"message": commitInfo.Message,
			"path":    playbook.Source.Path,
		}

		if err := r.Update(ctx, existingConfigMap); err != nil {
			return "", false, err
		}

		return configMapName, true, nil
	}

	if !apierrors.IsNotFound(err) {
		return "", false, err
	}

	l.Info("Creating new commit tracking ConfigMap",
		"configmap", configMapName,
		"commit", commitInfo.SHA)

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName,
			Namespace: workflow.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by":   "k8sible",
				"app.kubernetes.io/name":         workflow.Name,
				"app.kubernetes.io/k8sible-type": playbook.Type,
			},
		},
		Data: map[string]string{
			"commit":  commitInfo.SHA,
			"date":    commitInfo.Date.Format(time.RFC3339),
			"message": commitInfo.Message,
			"path":    playbook.Source.Path,
		},
	}

	if err := controllerutil.SetControllerReference(workflow, configMap, r.Scheme); err != nil {
		return "", false, err
	}

	if err := r.Create(ctx, configMap); err != nil {
		return "", false, err
	}

	return configMapName, true, nil
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

func (r *K8sibleWorkflowReconciler) reconcileJob(ctx context.Context, workflow *k8siblev1alpha1.K8sibleWorkflow, playbook Playbook) error {
	l := logf.FromContext(ctx)

	jobName := fmt.Sprintf("ansible-%s-%s-%d", workflow.Name, playbook.Type, time.Now().Unix())
	ansiblePullArgs := buildAnsiblePullArgs(playbook.Source)

	l.Info("Creating new ansible-pull Job", "job", jobName, "type", playbook.Type, "backoffLimit", playbook.MaxRetries)

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

	if err := r.Create(ctx, job); err != nil {
		return err
	}

	l.Info("Job created", "job", jobName, "args", ansiblePullArgs, "backoffLimit", playbook.MaxRetries)
	return nil
}

func (r *K8sibleWorkflowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&k8siblev1alpha1.K8sibleWorkflow{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&batchv1.Job{}).
		Named("k8sibleworkflow").
		Complete(r)
}
