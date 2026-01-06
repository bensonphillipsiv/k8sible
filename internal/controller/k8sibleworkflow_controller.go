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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	k8siblev1alpha1 "github.com/bensonphillipsiv/k8sible.git/api/v1alpha1"
	"github.com/bensonphillipsiv/k8sible.git/internal/git"
)

// K8sibleWorkflowReconciler reconciles a K8sibleWorkflow object
type K8sibleWorkflowReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	GitClient *git.Client
}

// Playbook represents a playbook source and its type
type Playbook struct {
	Source   git.Source
	Type     string // "apply" or "reconcile"
	Schedule string // cron schedule (optional)
}

// +kubebuilder:rbac:groups=k8sible.core.k8sible.io,resources=k8sibleworkflows,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8sible.core.k8sible.io,resources=k8sibleworkflows/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=k8sible.core.k8sible.io,resources=k8sibleworkflows/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete

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
		"reconcilePath", workflow.Spec.Reconcile.Path,
		"reconcileSchedule", workflow.Spec.Reconcile.Schedule,
		"pendingPlaybooks", workflow.Status.PendingPlaybooks,
	)

	// Build list of playbooks to check
	playbooks := []Playbook{
		{
			Source: git.Source{
				Repository: workflow.Spec.Source.Repository,
				Reference:  workflow.Spec.Source.Reference,
				Path:       workflow.Spec.Apply.Path,
			},
			Type:     "apply",
			Schedule: workflow.Spec.Apply.Schedule,
		},
	}

	if workflow.Spec.Reconcile != nil {
		playbooks = append(playbooks, Playbook{
			Source: git.Source{
				Repository: workflow.Spec.Source.Repository,
				Reference:  workflow.Spec.Source.Reference,
				Path:       workflow.Spec.Reconcile.Path,
			},
			Type:     "reconcile",
			Schedule: workflow.Spec.Reconcile.Schedule,
		})
	}

	// Build a map of playbooks for easy lookup
	playbookMap := make(map[string]Playbook)
	for _, p := range playbooks {
		playbookMap[p.Type] = p
	}

	// Track if status needs updating
	statusUpdated := false

	// Process all playbooks - check commits and track pending jobs
	for _, playbook := range playbooks {
		commitInfo, err := r.GitClient.GetLatestCommit(ctx, playbook.Source)
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

		// Reconcile CronJob if schedule is defined
		if playbook.Schedule != "" {
			if err := r.reconcileCronJob(ctx, workflow, playbook); err != nil {
				l.Error(err, "failed to reconcile CronJob")
				return ctrl.Result{}, err
			}
			l.Info("Successfully reconciled CronJob",
				"type", playbook.Type,
				"schedule", playbook.Schedule)
		} else {
			// No schedule, clean up any existing CronJob
			if err := r.deleteCronJobIfExists(ctx, workflow, playbook.Type); err != nil {
				l.Error(err, "failed to delete CronJob")
				return ctrl.Result{}, err
			}
		}

		// If commit changed, add to pending playbooks for immediate execution
		if updated && !contains(workflow.Status.PendingPlaybooks, playbook.Type) {
			workflow.Status.PendingPlaybooks = append(workflow.Status.PendingPlaybooks, playbook.Type)
			statusUpdated = true
			l.Info("Added playbook to pending queue (new commit detected)",
				"type", playbook.Type,
				"commit", commitInfo.SHA,
				"pendingPlaybooks", workflow.Status.PendingPlaybooks)
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
		l.Info("Successfully created ansible-pull Job",
			"type", playbook.Type,
			"repo", playbook.Source.Repository,
			"path", playbook.Source.Path)

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

	if len(workflow.Status.PendingPlaybooks) > 0 {
		l.Info("More playbooks pending, will requeue to check job status",
			"pendingPlaybooks", workflow.Status.PendingPlaybooks)
		return ctrl.Result{RequeueAfter: time.Second * 30}, nil
	}

	return ctrl.Result{RequeueAfter: time.Minute * 3}, nil
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
		if job.Status.Active > 0 {
			logf.FromContext(ctx).Info("Found running job",
				"job", job.Name,
				"active", job.Status.Active)
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

	l.Info("Creating new ansible-pull Job", "job", jobName, "type", playbook.Type)

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

	l.Info("Job created", "job", jobName, "args", ansiblePullArgs)
	return nil
}

func (r *K8sibleWorkflowReconciler) reconcileCronJob(ctx context.Context, workflow *k8siblev1alpha1.K8sibleWorkflow, playbook Playbook) error {
	l := logf.FromContext(ctx)

	cronJobName := fmt.Sprintf("%s-%s", workflow.Name, playbook.Type)
	ansiblePullArgs := buildAnsiblePullArgs(playbook.Source)

	existingCronJob := &batchv1.CronJob{}
	err := r.Get(ctx, client.ObjectKey{Name: cronJobName, Namespace: workflow.Namespace}, existingCronJob)

	cronJobSpec := batchv1.CronJobSpec{
		Schedule:          playbook.Schedule,
		ConcurrencyPolicy: batchv1.ForbidConcurrent,
		JobTemplate: batchv1.JobTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"app.kubernetes.io/managed-by":       "k8sible",
					"app.kubernetes.io/k8sible-workflow": workflow.Name,
					"app.kubernetes.io/k8sible-type":     playbook.Type,
				},
			},
			Spec: batchv1.JobSpec{
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
		},
	}

	if err == nil {
		if existingCronJob.Spec.Schedule == playbook.Schedule {
			l.Info("CronJob unchanged, skipping update",
				"cronjob", cronJobName,
				"schedule", playbook.Schedule)
			return nil
		}

		l.Info("Updating CronJob",
			"cronjob", cronJobName,
			"oldSchedule", existingCronJob.Spec.Schedule,
			"newSchedule", playbook.Schedule)

		existingCronJob.Spec = cronJobSpec
		return r.Update(ctx, existingCronJob)
	}

	if !apierrors.IsNotFound(err) {
		return err
	}

	l.Info("Creating new CronJob",
		"cronjob", cronJobName,
		"schedule", playbook.Schedule)

	cronJob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cronJobName,
			Namespace: workflow.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by":       "k8sible",
				"app.kubernetes.io/k8sible-workflow": workflow.Name,
				"app.kubernetes.io/k8sible-type":     playbook.Type,
			},
		},
		Spec: cronJobSpec,
	}

	if err := controllerutil.SetControllerReference(workflow, cronJob, r.Scheme); err != nil {
		return err
	}

	if err := r.Create(ctx, cronJob); err != nil {
		return err
	}

	l.Info("CronJob created", "cronjob", cronJobName, "schedule", playbook.Schedule)
	return nil
}

func (r *K8sibleWorkflowReconciler) deleteCronJobIfExists(ctx context.Context, workflow *k8siblev1alpha1.K8sibleWorkflow, playbookType string) error {
	l := logf.FromContext(ctx)

	cronJobName := fmt.Sprintf("%s-%s", workflow.Name, playbookType)

	existingCronJob := &batchv1.CronJob{}
	err := r.Get(ctx, client.ObjectKey{Name: cronJobName, Namespace: workflow.Namespace}, existingCronJob)

	if apierrors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		return err
	}

	l.Info("Deleting CronJob (schedule removed)", "cronjob", cronJobName)
	return r.Delete(ctx, existingCronJob)
}

func (r *K8sibleWorkflowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&k8siblev1alpha1.K8sibleWorkflow{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&batchv1.Job{}).
		Owns(&batchv1.CronJob{}).
		Named("k8sibleworkflow").
		Complete(r)
}
