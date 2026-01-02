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

	corev1 "k8s.io/api/core/v1"
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

	config := &k8siblev1alpha1.K8sibleWorkflow{}
	if err := r.Get(ctx, req.NamespacedName, config); err != nil {
		l.Error(err, "unable to fetch K8sibleWorkflow")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	l.Info("K8sibleWorkflow Source",
		"name", config.Name,
		"repo", config.Spec.Source.Repository,
		"path", config.Spec.Source.Path,
		"reference", config.Spec.Source.Reference,
		"schedule", config.Spec.Schedule)

	// Convert to git.Source
	source := git.Source{
		Repository: config.Spec.Source.Repository,
		Path:       config.Spec.Source.Path,
		Reference:  config.Spec.Source.Reference,
	}

	// Fetch and log the file contents from the source
	contents, err := r.GitClient.FetchFile(ctx, source)
	if err != nil {
		l.Error(err, "failed to fetch file from source",
			"repo", source.Repository,
			"path", source.Path)
		return ctrl.Result{}, err
	}
	l.Info("Fetched file contents",
		"path", source.Path,
		"contents", contents)

	if err := r.reconcileConfigMap(ctx, config, contents); err != nil {
		l.Error(err, "failed to reconcile ConfigMap")
		return ctrl.Result{}, err
	}
	l.Info("Successfully reconciled ConfigMap",
		"configmap", config.Name+"-workflow")

	return ctrl.Result{}, nil
}

// reconcileConfigMap creates or updates a ConfigMap with the workflow file contents
func (r *K8sibleWorkflowReconciler) reconcileConfigMap(ctx context.Context, workflow *k8siblev1alpha1.K8sibleWorkflow, contents string) error {
	configMapName := workflow.Name + "-workflow"
	fileName := filepath.Base(workflow.Spec.Source.Path)

	// Define the desired ConfigMap
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName,
			Namespace: workflow.Namespace,
		},
	}

	// Create or update the ConfigMap
	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, configMap, func() error {
		// Set the data
		configMap.Data = map[string]string{
			fileName: contents,
		}

		// Set labels
		configMap.Labels = map[string]string{
			"app.kubernetes.io/managed-by": "k8sible",
			"app.kubernetes.io/name":       workflow.Name,
		}

		// Set the owner reference so the ConfigMap is deleted when the workflow is deleted
		return controllerutil.SetControllerReference(workflow, configMap, r.Scheme)
	})

	if err != nil {
		return err
	}

	logf.FromContext(ctx).Info("ConfigMap reconciled",
		"configmap", configMapName,
		"operation", result)

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *K8sibleWorkflowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&k8siblev1alpha1.K8sibleWorkflow{}).
		Named("k8sibleworkflow").
		Complete(r)
}
