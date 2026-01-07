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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SecretRef references a Kubernetes secret
type SecretRef struct {
	// Name is the name of the secret
	Name string `json:"name"`

	// Key is the key in the secret containing the token (defaults to "token")
	// +optional
	// +kubebuilder:default=token
	Key string `json:"key,omitempty"`
}

// SourceSpec defines the git repository source
type SourceSpec struct {
	// Repository is the URL of the git repository
	Repository string `json:"repository"`

	// Reference is the git reference (branch, tag, or commit)
	// +optional
	Reference string `json:"reference,omitempty"`

	// SecretRef references a secret containing the git credentials
	// +optional
	SecretRef *SecretRef `json:"secretRef,omitempty"`
}

// PlaybookSpec defines a playbook configuration
type PlaybookSpec struct {
	// Path is the path to the playbook file in the repository
	Path string `json:"path"`

	// Schedule is an optional cron schedule for running the playbook
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// MaxRetries is the maximum number of retry attempts for failed jobs
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10
	MaxRetries *int32 `json:"maxRetries,omitempty"`
}

// K8sibleWorkflowSpec defines the desired state of K8sibleWorkflow
type K8sibleWorkflowSpec struct {
	// Source defines the git repository source
	Source SourceSpec `json:"source"`

	// Apply defines the apply playbook configuration
	Apply PlaybookSpec `json:"apply"`

	// Reconcile defines the optional reconcile playbook configuration
	// +optional
	Reconcile *PlaybookSpec `json:"reconcile,omitempty"`

	// FailureCycleCooldown is the duration to wait before retrying after max retries exhausted.
	// If not set, always retry immediately (can cause cycles).
	// If set to "0", never retry after max retries (manual intervention required).
	// Examples: "1h", "30m", "24h"
	// +optional
	FailureCycleCooldown *metav1.Duration `json:"failureCycleCooldown,omitempty"`
}

// PlaybookRunStatus represents the status of a playbook run
type PlaybookRunStatus struct {
	// Type is the playbook type (apply or reconcile)
	Type string `json:"type"`

	// StartTime is when the job started
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// EndTime is when the job ended
	// +optional
	EndTime *metav1.Time `json:"endTime,omitempty"`

	// Succeeded indicates if the job succeeded
	Succeeded bool `json:"succeeded"`

	// Message contains additional information about the run
	// +optional
	Message string `json:"message,omitempty"`
}

// ScheduleStatus tracks the last scheduled run time for a playbook
type ScheduleStatus struct {
	// LastScheduledTime is when the playbook was last scheduled to run
	// +optional
	LastScheduledTime *metav1.Time `json:"lastScheduledTime,omitempty"`
}

// CommitStatus tracks the last seen commit for a playbook
type CommitStatus struct {
	// SHA is the commit SHA
	SHA string `json:"sha,omitempty"`

	// Date is when the commit was authored
	// +optional
	Date *metav1.Time `json:"date,omitempty"`

	// Message is the commit message (truncated)
	// +optional
	Message string `json:"message,omitempty"`
}

// FailureCycleStatus tracks failure cycle information
type FailureCycleStatus struct {
	// InCooldown indicates if the workflow is currently in cooldown
	InCooldown bool `json:"inCooldown,omitempty"`

	// CooldownStartTime is when the cooldown period started
	// +optional
	CooldownStartTime *metav1.Time `json:"cooldownStartTime,omitempty"`

	// CooldownReason describes why cooldown was triggered
	// +optional
	CooldownReason string `json:"cooldownReason,omitempty"`

	// ReconcileTriggeredApply tracks if the current apply was triggered by reconcile failure
	ReconcileTriggeredApply bool `json:"reconcileTriggeredApply,omitempty"`

	// ConsecutiveReconcileFailuresAfterApply tracks reconcile failures after a reconcile-triggered apply
	ConsecutiveReconcileFailuresAfterApply int `json:"consecutiveReconcileFailuresAfterApply,omitempty"`
}

// K8sibleWorkflowStatus defines the observed state of K8sibleWorkflow.
type K8sibleWorkflowStatus struct {
	// ApplyCommit tracks the last seen commit for the apply playbook
	// +optional
	ApplyCommit *CommitStatus `json:"applyCommit,omitempty"`

	// ReconcileCommit tracks the last seen commit for the reconcile playbook
	// +optional
	ReconcileCommit *CommitStatus `json:"reconcileCommit,omitempty"`

	// PendingPlaybooks is a list of playbook types waiting to be executed
	// +optional
	PendingPlaybooks []string `json:"pendingPlaybooks,omitempty"`

	// LastSuccessfulRun contains information about the last successful playbook run
	// +optional
	LastSuccessfulRun *PlaybookRunStatus `json:"lastSuccessfulRun,omitempty"`

	// LastFailedRun contains information about the last failed playbook run
	// +optional
	LastFailedRun *PlaybookRunStatus `json:"lastFailedRun,omitempty"`

	// ApplyScheduleStatus tracks the apply playbook schedule
	// +optional
	ApplyScheduleStatus *ScheduleStatus `json:"applyScheduleStatus,omitempty"`

	// ReconcileScheduleStatus tracks the reconcile playbook schedule
	// +optional
	ReconcileScheduleStatus *ScheduleStatus `json:"reconcileScheduleStatus,omitempty"`

	// FailureCycleStatus tracks failure cycle and cooldown information
	// +optional
	FailureCycleStatus *FailureCycleStatus `json:"failureCycleStatus,omitempty"`

	// Conditions represent the current state of the K8sibleWorkflow resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Repository",type=string,JSONPath=`.spec.source.repository`
// +kubebuilder:printcolumn:name="Pending",type=string,JSONPath=`.status.pendingPlaybooks`
// +kubebuilder:printcolumn:name="Cooldown",type=boolean,JSONPath=`.status.failureCycleStatus.inCooldown`
// +kubebuilder:printcolumn:name="Last Success",type=date,JSONPath=`.status.lastSuccessfulRun.endTime`
// +kubebuilder:printcolumn:name="Last Failure",type=date,JSONPath=`.status.lastFailedRun.endTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// K8sibleWorkflow is the Schema for the k8sibleworkflows API
type K8sibleWorkflow struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec K8sibleWorkflowSpec `json:"spec"`

	// +optional
	Status K8sibleWorkflowStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// K8sibleWorkflowList contains a list of K8sibleWorkflow
type K8sibleWorkflowList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []K8sibleWorkflow `json:"items"`
}

func init() {
	SchemeBuilder.Register(&K8sibleWorkflow{}, &K8sibleWorkflowList{})
}
