// Package v1alpha1 contains the Hecate API.
//
// Hecate models delivery as four things:
//
//	Beacon  — watches artifact sources and emits Bundles when something new appears.
//	Bundle  — an immutable, content-addressed set of artifact versions. The unit that moves.
//	Gate    — an environment, and the threshold a Bundle must cross to enter it.
//	Passage — one attempt to move a Bundle through a Gate.
//
// The vocabulary is deliberate: Hecate Kleidouchos is the key-holder who stands
// at the threshold and decides what may pass.
//
// +kubebuilder:object:generate=true
// +groupName=hecate.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the group and version of the Hecate API.
var GroupVersion = schema.GroupVersion{Group: "hecate.dev", Version: "v1alpha1"}

// SchemeBuilder registers the Hecate types with a runtime.Scheme.
//
// apimachinery's builder rather than controller-runtime's, which is deprecated
// on the grounds that an API package should have minimal dependencies. Taking
// that seriously is the point: this package is importable on its own, by a
// controller, an operator or a CLI that wants the types and nothing else, and
// it no longer drags controller-runtime along to get them.
var SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

// AddToScheme adds the Hecate types to a runtime.Scheme.
var AddToScheme = SchemeBuilder.AddToScheme

// addKnownTypes registers the types and the group's metadata.
//
// The metav1 registration is not optional bookkeeping: without it the scheme
// has no ListOptions or DeleteOptions for this group, and a client's List or
// Delete fails to convert its own options.
func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&Beacon{}, &BeaconList{},
		&Bundle{}, &BundleList{},
		&Gate{}, &GateList{},
		&Passage{}, &PassageList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
