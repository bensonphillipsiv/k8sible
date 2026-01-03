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

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

type SourceSpec struct {
	Repository string `json:"repository"`
	Reference  string `json:"reference,omitempty"`
}

type ApplySpec struct {
	Path     string `json:"path"`
	Schedule string `json:"schedule,omitempty"`
}

type ReconcileSpec struct {
	Path     string `json:"path"`
	Schedule string `json:"schedule,omitempty"`
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
}

// K8sibleWorkflowStatus defines the observed state of K8sibleWorkflow.
type K8sibleWorkflowStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the K8sibleWorkflow resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.

	// PendingPlaybooks is a list of playbook types waiting to be executed
	// +optional
	PendingPlaybooks []string `json:"pendingPlaybooks,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

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
