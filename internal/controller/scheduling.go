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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	k8siblev1alpha1 "github.com/bensonphillipsiv/k8sible.git/api/v1alpha1"
)

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
			workflow.Status.LastTriggerReason = TriggerReasonNewCommit
			statusUpdated = true

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
			workflow.Status.LastTriggerReason = TriggerReasonSchedule
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

	// If the last apply failed, don't start a new job until the failureCycleCooldown period has passed
	// unless the trigger reason is a new commit or schedule
	if workflow.Spec.FailureCycleCooldown != nil &&
		workflow.Status.LastFailedRun != nil &&
		workflow.Status.LastTriggerReason == TriggerReasonFailureRetry {

		if workflow.Status.LastFailedRun.Type == "apply" && workflow.Status.LastFailedRun.EndTime != nil {
			cooldownEnd := workflow.Status.LastFailedRun.EndTime.Add(workflow.Spec.FailureCycleCooldown.Duration)
			if time.Now().Before(cooldownEnd) {
				l.Info("Apply job in cooldown, waiting",
					"cooldownEnd", cooldownEnd,
					"timeRemaining", time.Until(cooldownEnd))
				r.Recorder.Eventf(workflow, corev1.EventTypeNormal, EventReasonCooldownWaiting,
					"Apply job in cooldown, next retry at %s", cooldownEnd.Format(time.RFC3339))
				return false, nil
			}
		}

		// If the last failed job is a reconcile job and the last successful job is an apply job
		// that occurred at a later date than the reconcile job, don't retrigger a reconcile job
		// until after the cooldown
		if workflow.Status.LastFailedRun.Type == "reconcile" &&
			workflow.Status.LastFailedRun.EndTime != nil &&
			workflow.Status.LastSuccessfulRun != nil &&
			workflow.Status.LastSuccessfulRun.Type == "apply" &&
			workflow.Status.LastSuccessfulRun.EndTime != nil &&
			workflow.Status.LastSuccessfulRun.EndTime.After(workflow.Status.LastFailedRun.EndTime.Time) &&
			playbookType == "reconcile" {
			cooldownEnd := workflow.Status.LastSuccessfulRun.EndTime.Add(workflow.Spec.FailureCycleCooldown.Duration)
			if time.Now().Before(cooldownEnd) {
				l.Info("Reconcile job in cooldown after apply succeeded, waiting",
					"cooldownEnd", cooldownEnd,
					"timeRemaining", time.Until(cooldownEnd))
				r.Recorder.Eventf(workflow, corev1.EventTypeNormal, EventReasonCooldownWaiting,
					"Reconcile job in cooldown after apply succeeded, next retry at %s", cooldownEnd.Format(time.RFC3339))
				return false, nil
			}
		}
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

	// If there are pending jobs, requeue sooner
	if len(workflow.Status.PendingPlaybooks) > 0 {
		return time.Second * 30
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
