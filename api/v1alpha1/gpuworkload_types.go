package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WorkloadState is the desired state of a GPU workload.
// +kubebuilder:validation:Enum=Running;Suspended
type WorkloadState string

const (
	// WorkloadRunning means the workload's processes run on their GPUs.
	WorkloadRunning WorkloadState = "Running"
	// WorkloadSuspended means the GPU state of the workload's processes is
	// kept in host memory and its GPUs are free for other workloads.
	WorkloadSuspended WorkloadState = "Suspended"
)

// WorkloadPhase is the observed state of a GPU workload.
// +kubebuilder:validation:Enum=Pending;Running;Suspending;Suspended;Waiting;Resuming;Failed
type WorkloadPhase string

const (
	// PhasePending means no container of the workload holds a lease yet.
	PhasePending WorkloadPhase = "Pending"
	// PhaseRunning means the processes run on their leased GPUs.
	PhaseRunning WorkloadPhase = "Running"
	// PhaseSuspending means the processes are being checkpointed.
	PhaseSuspending WorkloadPhase = "Suspending"
	// PhaseSuspended means the GPU state is in host memory and no GPU is
	// leased.
	PhaseSuspended WorkloadPhase = "Suspended"
	// PhaseWaiting means the workload should run, but no suitable GPUs are
	// free in the pool.
	PhaseWaiting WorkloadPhase = "Waiting"
	// PhaseResuming means the processes are being restored onto GPUs.
	PhaseResuming WorkloadPhase = "Resuming"
	// PhaseFailed means a checkpoint or restore failed. The message tells why.
	PhaseFailed WorkloadPhase = "Failed"
)

// Isolation tells which GPUs a workload's containers can see.
// +kubebuilder:validation:Enum=strict;migratable
type Isolation string

const (
	// IsolationStrict shows a container only the GPUs of its lease. It
	// resumes only on the same GPUs.
	IsolationStrict Isolation = "strict"
	// IsolationMigratable shows a container every GPU of the node's pool,
	// with the leased GPUs first in CUDA_VISIBLE_DEVICES. It can resume on
	// other pool GPUs of the same chip. Only for trusted workloads.
	IsolationMigratable Isolation = "migratable"
)

// GPUWorkloadSpec is the desired state of a GPU workload.
type GPUWorkloadSpec struct {
	// State is Running or Suspended. Suspending keeps the processes alive
	// and moves their GPU memory into host memory, which frees the GPUs.
	// +kubebuilder:default=Running
	// +optional
	State WorkloadState `json:"state,omitempty"`
}

// ContainerLease describes the GPUs of one container of the workload.
type ContainerLease struct {
	// Name of the container.
	Name string `json:"name"`
	// ContainerID is the runtime's ID of the container.
	// +optional
	ContainerID string `json:"containerID,omitempty"`
	// GPUCount is the number of GPUs the container requested.
	GPUCount int32 `json:"gpuCount"`
	// VisibleGPUs are the GPUs the container's processes see, in the order
	// of CUDA_VISIBLE_DEVICES. With the isolation strict, they are the
	// leased GPUs.
	// +optional
	// +listType=atomic
	VisibleGPUs []string `json:"visibleGPUs,omitempty"`
	// PhysicalGPUs are the physical GPUs behind VisibleGPUs, in the same
	// order. They differ after the workload resumed on other GPUs.
	// +optional
	// +listType=atomic
	PhysicalGPUs []string `json:"physicalGPUs,omitempty"`
	// LeasedGPUs are the GPUs the container holds right now. Empty while
	// the workload is suspended.
	// +optional
	// +listType=atomic
	LeasedGPUs []string `json:"leasedGPUs,omitempty"`
}

// GPUWorkloadStatus is the observed state of a GPU workload.
type GPUWorkloadStatus struct {
	// Phase is the current state of the workload.
	// +optional
	Phase WorkloadPhase `json:"phase,omitempty"`
	// NodeName is the node the workload runs on.
	// +optional
	NodeName string `json:"nodeName,omitempty"`
	// Isolation of the workload's containers.
	// +optional
	Isolation Isolation `json:"isolation,omitempty"`
	// Containers lists the containers with GPUs.
	// +optional
	// +listType=map
	// +listMapKey=name
	Containers []ContainerLease `json:"containers,omitempty"`
	// Message explains the phase, for example why the workload waits.
	// +optional
	Message string `json:"message,omitempty"`
	// LastTransitionTime is when the phase last changed.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
	// LastSuspendSeconds is how long the last suspend took.
	// +optional
	LastSuspendSeconds string `json:"lastSuspendSeconds,omitempty"`
	// LastResumeSeconds is how long the last resume took.
	// +optional
	LastResumeSeconds string `json:"lastResumeSeconds,omitempty"`
}

// GPUWorkload is a pod with the RuntimeClass kybernate whose containers hold
// GPUs from the Kybernate pool. The node agent creates it for every such pod,
// with the pod's name, and deletes it together with the pod. Set spec.state
// to suspend or resume the workload.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=gpuw
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.spec.state`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.status.nodeName`
// +kubebuilder:printcolumn:name="Isolation",type=string,JSONPath=`.status.isolation`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type GPUWorkload struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GPUWorkloadSpec   `json:"spec,omitempty"`
	Status GPUWorkloadStatus `json:"status,omitempty"`
}

// GPUWorkloadList is a list of GPUWorkloads.
//
// +kubebuilder:object:root=true
type GPUWorkloadList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GPUWorkload `json:"items"`
}
