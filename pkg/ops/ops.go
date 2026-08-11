// Package ops is the operations layer every human-facing surface sits on: the
// CLI, the API server, the MCP server and the UI.
//
// It exists so that those four do not each answer the same questions
// separately. The answers that matter are *rules* — what counts as eligible,
// who may approve, why a Gate is stuck — and three implementations of a rule is
// three subtly different products. Adapters may format; they may not decide.
//
// Thin over the Kubernetes API, and deliberately not a second data model: reads
// return the API types themselves. The only shapes defined here are ones that
// carry genuinely derived information — an explanation is not stored anywhere,
// which is precisely why it belongs in one place.
package ops

import (
	"context"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// Ops answers questions and performs actions against a cluster.
type Ops struct {
	Client client.Client
	// Now is the clock, injectable for tests and for judging promotion windows.
	Now func() metav1.Time
}

// New returns an Ops over the given client.
func New(c client.Client) *Ops { return &Ops{Client: c} }

func (o *Ops) now() metav1.Time {
	if o.Now != nil {
		return o.Now()
	}
	return metav1.Now()
}

// NotFoundError is a named resource that is not there.
//
// Distinguished from any other failure because every surface needs to answer
// differently: the API server owes a 404, the CLI a exit code, and the MCP
// server a message the model can act on rather than retry.
type NotFoundError struct {
	Kind      string
	Namespace string
	Name      string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("no %s named %s in %s", e.Kind, e.Name, e.Namespace)
}

// IsNotFound reports whether err is a NotFoundError.
func IsNotFound(err error) bool {
	_, ok := err.(*NotFoundError)
	return ok
}

// ------------------------------------------------------------------ reads ---

// Gates lists a namespace's Gates, by name.
//
// Ordered rather than left in list order: a surface that prints them should
// print them the same way twice, and a diffable list is worth more than a fast
// one at this size.
func (o *Ops) Gates(ctx context.Context, namespace string) ([]v1alpha1.Gate, error) {
	var list v1alpha1.GateList
	if err := o.Client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing Gates: %w", err)
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	return list.Items, nil
}

// Gate reads one Gate.
func (o *Ops) Gate(ctx context.Context, namespace, name string) (*v1alpha1.Gate, error) {
	var gate v1alpha1.Gate
	err := o.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &gate)
	if apierrors.IsNotFound(err) {
		return nil, &NotFoundError{Kind: "Gate", Namespace: namespace, Name: name}
	}
	if err != nil {
		return nil, fmt.Errorf("reading Gate %s: %w", name, err)
	}
	return &gate, nil
}

// Bundles lists a namespace's Bundles, newest first — the order an operator
// expects, because the question is nearly always about the recent ones.
func (o *Ops) Bundles(ctx context.Context, namespace string) ([]v1alpha1.Bundle, error) {
	var list v1alpha1.BundleList
	if err := o.Client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing Bundles: %w", err)
	}
	sort.Slice(list.Items, func(i, j int) bool {
		a, b := &list.Items[i], &list.Items[j]
		if a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return a.Name < b.Name
		}
		return a.CreationTimestamp.After(b.CreationTimestamp.Time)
	})
	return list.Items, nil
}

// Bundle reads one Bundle.
func (o *Ops) Bundle(ctx context.Context, namespace, name string) (*v1alpha1.Bundle, error) {
	var bundle v1alpha1.Bundle
	err := o.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &bundle)
	if apierrors.IsNotFound(err) {
		return nil, &NotFoundError{Kind: "Bundle", Namespace: namespace, Name: name}
	}
	if err != nil {
		return nil, fmt.Errorf("reading Bundle %s: %w", name, err)
	}
	return &bundle, nil
}

// Passage reads one Passage.
func (o *Ops) Passage(ctx context.Context, namespace, name string) (*v1alpha1.Passage, error) {
	var p v1alpha1.Passage
	err := o.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &p)
	if apierrors.IsNotFound(err) {
		return nil, &NotFoundError{Kind: "Passage", Namespace: namespace, Name: name}
	}
	if err != nil {
		return nil, fmt.Errorf("reading Passage %s: %w", name, err)
	}
	return &p, nil
}

// Passages lists a namespace's Passages, newest first. Filters are optional:
// an empty gate or bundle means "any".
func (o *Ops) Passages(ctx context.Context, namespace, gate, bundle string) ([]v1alpha1.Passage, error) {
	var list v1alpha1.PassageList
	if err := o.Client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing Passages: %w", err)
	}
	var out []v1alpha1.Passage
	for _, p := range list.Items {
		if gate != "" && p.Spec.Gate != gate {
			continue
		}
		if bundle != "" && p.Spec.Bundle != bundle {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := &out[i], &out[j]
		if a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return a.Name < b.Name
		}
		return a.CreationTimestamp.After(b.CreationTimestamp.Time)
	})
	return out, nil
}
