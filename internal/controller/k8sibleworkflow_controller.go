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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	k8siblev1alpha1 "github.com/bensonphillipsiv/k8sible.git/api/v1alpha1"
	"github.com/bensonphillipsiv/k8sible.git/internal/git"
)

const (
	DefaultMaxRetries int32 = 3

	// Playbook types
	PlaybookTypeApply     = "apply"
	PlaybookTypeReconcile = "reconcile"

	// Annotation keys and values
	AnnotationProcessed      = "k8sible.io/processed"
	AnnotationProcessedValue = "true"

	// Event reasons
	EventReasonJobStarted       = "JobStarted"
	EventReasonJobSucceeded     = "JobSucceeded"
	EventReasonJobFailed        = "JobFailed"
	EventReasonReconcileTrigger = "ReconcileTriggeredApply"
	EventReasonScheduledRun     = "ScheduledRun"
	EventReasonNewCommit        = "NewCommit"
	EventReasonCooldownWaiting  = "CooldownWaiting"

	// Trigger reasons for pending playbooks
	TriggerReasonNewCommit    = "new_commit"
	TriggerReasonSchedule     = "schedule"
	TriggerReasonFailureRetry = "failure_retry"

	// Default working directory for git clone
	DefaultWorkDir = "/tmp/ansible"

	// Default ansible runner image
	DefaultAnsibleRunnerImage = "ghcr.io/bensonphillipsiv/ansible-runner:latest"
)

// K8sibleWorkflowReconciler reconciles a K8sibleWorkflow object
type K8sibleWorkflowReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	GitClient git.ClientInterface
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

	l.Info("K8sibleWorkflow",
		"name", workflow.Name,
		"repo", workflow.Spec.Source.Repository,
		"ref", workflow.Spec.Source.Reference,
		"pendingPlaybooks", workflow.Status.PendingPlaybooks,
	)

	// Track if status needs updating
	statusUpdated := false

	// PHASE 1: Process any completed jobs first
	if updated, err := r.processCompletedJobs(ctx, workflow); err != nil {
		l.Error(err, "failed to process completed jobs")
		return ctrl.Result{}, err
	} else if updated {
		// Save status immediately after processing completed jobs to ensure
		// LastSuccessfulRun/LastFailedRun are persisted before attempting retries
		if err := r.Status().Update(ctx, workflow); err != nil {
			l.Error(err, "failed to update workflow status after job completion")
			return ctrl.Result{}, err
		}
	}

	// PHASE 2: Check if ANY job is currently running - if so, just wait
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

	// PHASE 3: No job running - determine what should run next
	// This is the ONLY place we add to pending queue (except for failure handling)
	if updated, err := r.determineNextJob(ctx, workflow); err != nil {
		l.Error(err, "failed to determine next job")
		return ctrl.Result{}, err
	} else if updated {
		statusUpdated = true
	}

	// PHASE 4: Start the next pending job (if any)
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

// buildPlaybookList creates the list of playbooks from the workflow spec
func (r *K8sibleWorkflowReconciler) buildPlaybookList(workflow *k8siblev1alpha1.K8sibleWorkflow) []Playbook {
	playbooks := []Playbook{
		{
			Source: git.Source{
				Repository: workflow.Spec.Source.Repository,
				Reference:  workflow.Spec.Source.Reference,
				Path:       workflow.Spec.Apply.Path,
			},
			Type:       PlaybookTypeApply,
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
			Type:       PlaybookTypeReconcile,
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
	return Playbook{}, errPlaybookNotFound(playbookType)
}

// Helper functions
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

// buildAnsibleScript creates a shell script that clones the repo and runs ansible-playbook
func buildAnsibleScript(source git.Source, inventoryPath string) string {
	// Build the git clone command
	// If GIT_TOKEN env var is set, use it for authentication
	script := fmt.Sprintf(`set -e
		WORKDIR="%s"
		REPO="%s"
		PLAYBOOK="%s"

		# Clean up any previous runs
		rm -rf "${WORKDIR}"
		mkdir -p "${WORKDIR}"

		# Clone the repository
		if [ -n "${GIT_TOKEN:-}" ]; then
			# Insert token into URL for authentication
			REPO_WITH_AUTH=$(echo "${REPO}" | sed "s|https://|https://oauth2:${GIT_TOKEN}@|")
			git clone "${REPO_WITH_AUTH}" "${WORKDIR}/repo"
		else
			git clone "${REPO}" "${WORKDIR}/repo"
		fi

		cd "${WORKDIR}/repo"
		`, DefaultWorkDir, source.Repository, source.Path)

	// Add checkout command if reference is specified
	if source.Reference != "" {
		script += fmt.Sprintf(`
			# Checkout the specified reference
			git checkout %s
			`, source.Reference)
	}

	// Build the ansible-playbook command
	script += `ansible-playbook`

	if inventoryPath != "" {
		script += fmt.Sprintf(` -i "%s"`, inventoryPath)
	}

	script += ` "${PLAYBOOK}"`

	return script
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
	for i := range playbooks {
		if playbooks[i] == PlaybookTypeReconcile {
			for j := i + 1; j < len(playbooks); j++ {
				if playbooks[j] == PlaybookTypeApply {
					playbooks[i], playbooks[j] = playbooks[j], playbooks[i]
					return
				}
			}
		}
	}
}

// addToPending adds a playbook type to the pending list if not already present
// Returns true if the playbook was added
func addToPending(pending []string, playbookType string) []string {
	if !contains(pending, playbookType) {
		return append(pending, playbookType)
	}
	return pending
}

func errPlaybookNotFound(playbookType string) error {
	return &PlaybookNotFoundError{Type: playbookType}
}

// PlaybookNotFoundError is returned when a playbook type is not found
type PlaybookNotFoundError struct {
	Type string
}

func (e *PlaybookNotFoundError) Error() string {
	return "playbook type " + e.Type + " not found"
}

// SetupWithManager sets up the controller with the Manager.
func (r *K8sibleWorkflowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&k8siblev1alpha1.K8sibleWorkflow{}).
		Owns(&batchv1.Job{}).
		Named("k8sibleworkflow").
		Complete(r)
}
