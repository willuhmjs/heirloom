// Package v1alpha1 contains the backshots.io API types.
// +kubebuilder:object:generate=true
// +groupName=backshots.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "backshots.io", Version: "v1alpha1"}
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion} //nolint:staticcheck // deprecated but still the kubebuilder-scaffolded registration helper, with no replacement
	AddToScheme   = SchemeBuilder.AddToScheme
)
