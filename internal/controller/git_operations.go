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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	k8siblev1alpha1 "github.com/bensonphillipsiv/k8sible.git/api/v1alpha1"
	"github.com/bensonphillipsiv/k8sible.git/internal/git"
)

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

// checkForNewCommit checks if there's a new commit and updates status
func (r *K8sibleWorkflowReconciler) checkForNewCommit(ctx context.Context, workflow *k8siblev1alpha1.K8sibleWorkflow, commitInfo *git.CommitInfo, playbook Playbook) bool {
	l := logf.FromContext(ctx)

	var currentCommit *k8siblev1alpha1.CommitStatus
	switch playbook.Type {
	case PlaybookTypeApply:
		currentCommit = workflow.Status.ApplyCommit
	case PlaybookTypeReconcile:
		currentCommit = workflow.Status.ReconcileCommit
	}

	// Check if commit changed
	if currentCommit != nil && currentCommit.SHA == commitInfo.SHA {
		return false
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

	switch playbook.Type {
	case PlaybookTypeApply:
		workflow.Status.ApplyCommit = newCommitStatus
	case PlaybookTypeReconcile:
		workflow.Status.ReconcileCommit = newCommitStatus
	}

	return true
}
