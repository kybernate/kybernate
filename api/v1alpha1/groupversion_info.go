// Package v1alpha1 contains the Kybernate API types of the group kybernate.io.
//
// +kubebuilder:object:generate=true
// +groupName=kybernate.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the API group and version of the Kybernate types.
var GroupVersion = schema.GroupVersion{Group: "kybernate.io", Version: "v1alpha1"}

var (
	// SchemeBuilder registers the Kybernate types with a scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	// AddToScheme adds the Kybernate types to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&GPUNode{},
		&GPUNodeList{},
		&GPUWorkload{},
		&GPUWorkloadList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
