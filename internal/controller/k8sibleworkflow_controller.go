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
	"path/filepath"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

// +kubebuilder:rbac:groups=k8sible.core.k8sible.io,resources=k8sibleworkflows,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8sible.core.k8sible.io,resources=k8sibleworkflows/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=k8sible.core.k8sible.io,resources=k8sibleworkflows/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
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
		"path", workflow.Spec.Source.Path,
		"reference", workflow.Spec.Source.Reference,
		"schedule", workflow.Spec.Schedule)

	// Convert to git.Source
	source := git.Source{
		Repository: workflow.Spec.Source.Repository,
		Path:       workflow.Spec.Source.Path,
		Reference:  workflow.Spec.Source.Reference,
	}

	// Fetch the file contents from the source
	contents, err := r.GitClient.FetchFile(ctx, source)
	if err != nil {
		l.Error(err, "failed to fetch file from source",
			"repo", source.Repository,
			"path", source.Path)
		return ctrl.Result{}, err
	}
	l.Info("Fetched file contents",
		"path", source.Path,
		"contentLength", len(contents))

	// Reconcile the ConfigMap
	configMapName, fileName, err := r.reconcileConfigMap(ctx, workflow, contents)
	if err != nil {
		l.Error(err, "failed to reconcile ConfigMap")
		return ctrl.Result{}, err
	}
	l.Info("Successfully reconciled ConfigMap", "configmap", configMapName)

	// Reconcile the Job
	if err := r.reconcileJob(ctx, workflow, configMapName, fileName); err != nil {
		l.Error(err, "failed to reconcile Job")
		return ctrl.Result{}, err
	}
	l.Info("Successfully reconciled Job", "job", "ansible-"+workflow.Name+"-job")

	return ctrl.Result{}, nil
}

// reconcileConfigMap creates or updates a ConfigMap with the workflow file contents
func (r *K8sibleWorkflowReconciler) reconcileConfigMap(ctx context.Context, workflow *k8siblev1alpha1.K8sibleWorkflow, contents string) (string, string, error) {
	configMapName := workflow.Name + "-workflow"
	fileName := filepath.Base(workflow.Spec.Source.Path)

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName,
			Namespace: workflow.Namespace,
		},
	}

	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, configMap, func() error {
		configMap.Data = map[string]string{
			fileName: contents,
		}

		configMap.Labels = map[string]string{
			"app.kubernetes.io/managed-by": "k8sible",
			"app.kubernetes.io/name":       workflow.Name,
		}

		return controllerutil.SetControllerReference(workflow, configMap, r.Scheme)
	})

	if err != nil {
		return "", "", err
	}

	logf.FromContext(ctx).Info("ConfigMap reconciled",
		"configmap", configMapName,
		"operation", result)

	return configMapName, fileName, nil
}

// reconcileJob creates a Job to run the Ansible playbook
func (r *K8sibleWorkflowReconciler) reconcileJob(ctx context.Context, workflow *k8siblev1alpha1.K8sibleWorkflow, configMapName, fileName string) error {
	jobName := "ansible-" + workflow.Name + "-job"

	// Check if job already exists
	existingJob := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: workflow.Namespace}, existingJob)
	if err == nil {
		// Job already exists, check if it's completed or failed
		if existingJob.Status.Succeeded > 0 || existingJob.Status.Failed > 0 {
			logf.FromContext(ctx).Info("Job already completed, skipping creation",
				"job", jobName,
				"succeeded", existingJob.Status.Succeeded,
				"failed", existingJob.Status.Failed)
			return nil
		}
		// Job is still running
		logf.FromContext(ctx).Info("Job is still running", "job", jobName)
		return nil
	}

	if !apierrors.IsNotFound(err) {
		return err
	}

	// Create the Job
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: workflow.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "k8sible",
				"app.kubernetes.io/name":       workflow.Name,
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": "k8sible",
						"app.kubernetes.io/name":       workflow.Name,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "ansible",
							Image:   "quay.io/ansible/ansible-runner:latest",
							Command: []string{"ansible-playbook"},
							Args:    []string{"/playbook/" + fileName},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "playbook",
									MountPath: "/playbook",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "playbook",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: configMapName,
									},
								},
							},
						},
					},
				},
			},
		},
	}

	// Set owner reference
	if err := controllerutil.SetControllerReference(workflow, job, r.Scheme); err != nil {
		return err
	}

	if err := r.Create(ctx, job); err != nil {
		return err
	}

	logf.FromContext(ctx).Info("Job created", "job", jobName)
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *K8sibleWorkflowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&k8siblev1alpha1.K8sibleWorkflow{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&batchv1.Job{}).
		Named("k8sibleworkflow").
		Complete(r)
}
