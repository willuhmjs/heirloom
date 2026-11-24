// Package v1 contains the heirloom.io API types.
// +kubebuilder:object:generate=true
// +groupName=heirloom.io
package v1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "heirloom.io", Version: "v1"}
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion} //nolint:staticcheck // deprecated but still the kubebuilder-scaffolded registration helper, with no replacement
	AddToScheme   = SchemeBuilder.AddToScheme
)
