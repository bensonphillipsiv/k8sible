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

// NOTE: json tags are required. Any new fields you add must have json tags for the fields to be serialized.

type SourceSpec struct {
	Repository string `json:"repository"`
	Reference  string `json:"reference,omitempty"`
}

// PlaybookSpec defines a playbook configuration
type PlaybookSpec struct {
	// Path is the path to the playbook file in the repository
	Path string `json:"path"`

	// Schedule is an optional cron schedule for running the playbook
	// +optional
	Schedule string `json:"schedule,omitempty"`
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

	// MaxRetries is the maximum number of retry attempts for failed jobs
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10
	MaxRetries int `json:"maxRetries,omitempty"`
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

	// Changed indicates if the playbook made changes
	// +optional
	Changed bool `json:"changed,omitempty"`

	// Message contains additional information about the run
	// +optional
	Message string `json:"message,omitempty"`
}

// K8sibleWorkflowStatus defines the observed state of K8sibleWorkflow.
type K8sibleWorkflowStatus struct {
	// PendingPlaybooks is a list of playbook types waiting to be executed
	// +optional
	PendingPlaybooks []string `json:"pendingPlaybooks,omitempty"`

	// RetryCount tracks the number of retries for each playbook type
	// +optional
	RetryCount map[string]int `json:"retryCount,omitempty"`

	// LastSuccessfulRun contains information about the last successful playbook run
	// +optional
	LastSuccessfulRun *PlaybookRunStatus `json:"lastSuccessfulRun,omitempty"`

	// LastFailedRun contains information about the last failed playbook run
	// +optional
	LastFailedRun *PlaybookRunStatus `json:"lastFailedRun,omitempty"`

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
// +kubebuilder:printcolumn:name="Last Success",type=date,JSONPath=`.status.lastSuccessfulRun.endTime`
// +kubebuilder:printcolumn:name="Last Failure",type=date,JSONPath=`.status.lastFailedRun.endTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// K8sibleWorkflow is the Schema for the k8sibleworkflows API
type K8sibleWorkflow struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of K8sibleWorkflow
	// +required
	Spec K8sibleWorkflowSpec `json:"spec"`

	// status defines the observed state of K8sibleWorkflow
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
