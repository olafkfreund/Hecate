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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion is the group and version of the Hecate API.
var GroupVersion = schema.GroupVersion{Group: "hecate.dev", Version: "v1alpha1"}

// SchemeBuilder registers the Hecate types with a runtime.Scheme.
// Deprecated on the grounds that an api package should have minimal
// dependencies; replacing it means constructing the scheme by hand.
// Tracked in #116 with the events migration, which touches the same area.
var SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion} //nolint:staticcheck // see #116

// AddToScheme adds the Hecate types to a runtime.Scheme.
var AddToScheme = SchemeBuilder.AddToScheme

func init() {
	SchemeBuilder.Register(
		&Beacon{}, &BeaconList{},
		&Bundle{}, &BundleList{},
		&Gate{}, &GateList{},
		&Passage{}, &PassageList{},
	)
}
