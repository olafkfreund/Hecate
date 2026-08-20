package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olafkfreund/hecate/api/v1alpha1"
)

// ClusterLabel marks a Secret as holding a remote cluster's kubeconfig.
//
// A label rather than a naming convention: names are chosen by whoever created
// the Secret and cannot be queried, while a label selector is an indexed server-
// side filter. It also means an operator can adopt a kubeconfig Secret they
// created by hand simply by labelling it.
const ClusterLabel = "hecate.dev/cluster"

// bindableRoles are the only ClusterRoles this API will bind a subject to.
//
// An allowlist rather than "whatever the caller names", and it is the most
// important line in this file. The obvious implementation takes a role name
// from the request; that turns a settings screen into a way to grant
// cluster-admin to yourself, limited only by what the *server's* ServiceAccount
// can grant — which is not the same as what the caller can, because the server
// does the writing.
//
// These are the roles the chart creates for people, and binding one is the
// whole of "add a user" in a system that has no users of its own.
//
// hecate-flux-operator is here rather than left to kubectl: anyone who can
// reach this endpoint can already grant promoter, and a role that exists but
// can only be bound by hand is a surprise rather than a safeguard. What keeps
// it honest is that it must be granted deliberately — nothing binds it by
// default, and no other role implies it.
var bindableRoles = map[string]struct{}{
	"hecate-viewer":        {},
	"hecate-promoter":      {},
	"hecate-approver":      {},
	"hecate-flux-operator": {},
}

// bindRoleRequest is "let this person do this".
type bindRoleRequest struct {
	// Subject is a username or a group name, as the cluster sees it. With OIDC
	// and usernameClaim=email that is an email address; the settings screen
	// shows the caller their own so there is a worked example on the page.
	Subject string `json:"subject"`
	// Kind is User or Group.
	Kind string `json:"kind"`
	// Role is one of bindableRoles.
	Role string `json:"role"`
}

// bindRole grants a Hecate role to a user or group.
//
// Hecate has no user store, so this creates a ClusterRoleBinding — the same
// object an administrator would apply by hand. That is deliberate: the
// permission model stays Kubernetes RBAC, and anything created here is visible
// to `kubectl get clusterrolebinding` rather than living in a second place that
// eventually disagrees with the first.
func (s *Server) bindRole(ctx context.Context, subject Subject, r *http.Request) (any, error) {
	var req bindRoleRequest
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<16)).Decode(&req); err != nil {
		return nil, &BadRequest{Reason: "the request body is not the expected JSON: " + err.Error()}
	}

	req.Subject = strings.TrimSpace(req.Subject)
	if req.Subject == "" {
		return nil, &BadRequest{Reason: "subject is required — the username or group the cluster will see, which for OIDC is usually an email address"}
	}
	switch req.Kind {
	case rbacv1.UserKind, rbacv1.GroupKind:
	case "":
		req.Kind = rbacv1.UserKind
	default:
		return nil, &BadRequest{Reason: fmt.Sprintf("kind must be %s or %s", rbacv1.UserKind, rbacv1.GroupKind)}
	}
	if _, ok := bindableRoles[req.Role]; !ok {
		return nil, &BadRequest{Reason: "role must be one of hecate-viewer, " +
			"hecate-promoter, hecate-approver or hecate-flux-operator — this API binds only the roles the chart " +
			"creates for people, because binding an arbitrary role would let a caller grant " +
			"themselves anything the server can grant"}
	}

	// The caller, not the server. See ActionBindRole.
	if err := s.Auth.Authorize(ctx, subject, ActionBindRole, ""); err != nil {
		return nil, err
	}

	name := bindingName(req.Role, req.Kind, req.Subject)
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "hecate",
			},
			Annotations: map[string]string{
				// Who did this, in the object itself. A binding that appears in
				// a cluster with no record of who added it is the kind of thing
				// an audit asks about and nobody can answer.
				"hecate.dev/granted-by": subject.Name,
			},
		},
		Subjects: []rbacv1.Subject{{
			Kind:     req.Kind,
			Name:     req.Subject,
			APIGroup: rbacv1.GroupName,
		}},
		RoleRef: rbacv1.RoleRef{
			Kind:     "ClusterRole",
			Name:     req.Role,
			APIGroup: rbacv1.GroupName,
		},
	}

	if err := s.Ops.Client.Create(ctx, binding); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Idempotent: the grant already exists and says the same thing, so
			// reporting a conflict would be reporting success as a failure.
			return map[string]any{"binding": name, "created": false}, nil
		}
		return nil, fmt.Errorf("creating the ClusterRoleBinding: %w", err)
	}
	return map[string]any{"binding": name, "created": true}, nil
}

// listBindings shows who currently holds a Hecate role.
func (s *Server) listBindings(ctx context.Context, subject Subject, _ *http.Request) (any, error) {
	if err := s.Auth.Authorize(ctx, subject, ActionBindRole, ""); err != nil {
		return nil, err
	}

	var all rbacv1.ClusterRoleBindingList
	if err := s.Ops.Client.List(ctx, &all); err != nil {
		return nil, fmt.Errorf("listing ClusterRoleBindings: %w", err)
	}

	type grant struct {
		Binding   string `json:"binding"`
		Role      string `json:"role"`
		Kind      string `json:"kind"`
		Subject   string `json:"subject"`
		GrantedBy string `json:"grantedBy,omitempty"`
	}
	out := []grant{}
	for i := range all.Items {
		b := &all.Items[i]
		if _, ok := bindableRoles[b.RoleRef.Name]; !ok {
			// Every other binding in the cluster is none of this screen's
			// business, and listing them would make it a cluster-wide RBAC
			// browser by accident.
			continue
		}
		for _, sub := range b.Subjects {
			out = append(out, grant{
				Binding:   b.Name,
				Role:      b.RoleRef.Name,
				Kind:      sub.Kind,
				Subject:   sub.Name,
				GrantedBy: b.Annotations["hecate.dev/granted-by"],
			})
		}
	}
	return map[string]any{"grants": out}, nil
}

// connectClusterRequest carries a kubeconfig for a cluster a Gate will watch.
type connectClusterRequest struct {
	Name string `json:"name"`
	// Kubeconfig is the file's contents. Stored under `value`, which is the key
	// Flux uses for the same thing, so an operator who has wired one for a
	// Kustomization does not learn a second convention (pkg/health).
	Kubeconfig string `json:"kubeconfig"`
}

// connectCluster stores a remote cluster's credentials as a Secret.
func (s *Server) connectCluster(ctx context.Context, subject Subject, r *http.Request) (any, error) {
	namespace := r.PathValue("namespace")

	var req connectClusterRequest
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&req); err != nil {
		return nil, &BadRequest{Reason: "the request body is not the expected JSON: " + err.Error()}
	}
	if strings.TrimSpace(req.Name) == "" {
		return nil, &BadRequest{Reason: "name is required"}
	}
	if strings.TrimSpace(req.Kubeconfig) == "" {
		return nil, &BadRequest{Reason: "kubeconfig is required"}
	}

	if err := s.Auth.Authorize(ctx, subject, ActionManageSecrets, namespace); err != nil {
		return nil, err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "hecate",
				// The marker that makes this Secret findable.
				//
				// Without it a stored cluster is invisible until some Gate
				// happens to name it, because the only other way to know a
				// Secret is a kubeconfig is to read it — and listing every
				// Secret in the cluster to look inside them is not something a
				// settings screen should do. Storing a cluster and then not
				// seeing it is what this label exists to prevent.
				ClusterLabel: "true",
			},
			Annotations: map[string]string{
				"hecate.dev/created-by": subject.Name,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"value": []byte(req.Kubeconfig)},
	}

	err := s.Ops.Client.Create(ctx, secret)
	if apierrors.IsAlreadyExists(err) {
		// Rotation is the common case — the same cluster with fresh
		// credentials — so an existing Secret is updated rather than refused.
		existing := &corev1.Secret{}
		if err := s.Ops.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: req.Name}, existing); err != nil {
			return nil, fmt.Errorf("reading the existing Secret: %w", err)
		}
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		existing.Data["value"] = []byte(req.Kubeconfig)
		if err := s.Ops.Client.Update(ctx, existing); err != nil {
			return nil, fmt.Errorf("updating the Secret: %w", err)
		}
		return map[string]any{"secret": namespace + "/" + req.Name, "created": false}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("creating the Secret: %w", err)
	}
	return map[string]any{"secret": namespace + "/" + req.Name, "created": true}, nil
}

// evidenceRequest changes which Fides a Gate trusts.
type evidenceRequest struct {
	ServerURL   string `json:"serverURL"`
	Environment string `json:"fidesEnvironment"`
	Credentials string `json:"credentialsRef,omitempty"`
}

// setEvidence points a Gate at an evidence server.
//
// **This writes to the cluster, and Flux will revert it.** A Gate is normally
// reconciled from git, so a change made here survives until the next sync and
// then disappears — which looks like the UI losing the change rather than
// GitOps doing its job. The screen says so; this is the honest place to repeat
// it, because the API cannot tell whether a given Gate is managed by Flux.
func (s *Server) setEvidence(ctx context.Context, subject Subject, r *http.Request) (any, error) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")

	var req evidenceRequest
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<16)).Decode(&req); err != nil {
		return nil, &BadRequest{Reason: "the request body is not the expected JSON: " + err.Error()}
	}
	if strings.TrimSpace(req.Environment) == "" {
		return nil, &BadRequest{Reason: "fidesEnvironment is required — it is a UUID, and there is no convention that could produce one from a name"}
	}

	if err := s.Auth.Authorize(ctx, subject, ActionEditGate, namespace); err != nil {
		return nil, err
	}

	gate, err := s.Ops.Gate(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	if gate.Spec.Evidence == nil {
		gate.Spec.Evidence = &v1alpha1.EvidenceConfig{}
	}
	gate.Spec.Evidence.FidesEnvironment = req.Environment
	gate.Spec.Evidence.ServerURL = req.ServerURL
	if req.Credentials != "" {
		gate.Spec.Evidence.CredentialsRef = &v1alpha1.LocalSecretRef{Name: req.Credentials}
	}

	if err := s.Ops.Client.Update(ctx, gate); err != nil {
		return nil, fmt.Errorf("updating Gate %s: %w", name, err)
	}
	return map[string]any{
		"gate": namespace + "/" + name,
		"note": "applied to the cluster. If this Gate is reconciled from git, Flux will restore the committed value on its next sync — change it there to make it stick.",
	}, nil
}

// bindingName is deterministic, so the same grant made twice is the same
// object rather than two bindings saying the same thing.
func bindingName(role, kind, subject string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		default:
			return '-'
		}
	}, subject)
	name := fmt.Sprintf("%s-%s-%s", role, strings.ToLower(kind), safe)
	// Kubernetes names cap at 253; long email addresses get close enough to be
	// worth truncating rather than failing on.
	if len(name) > 253 {
		name = name[:253]
	}
	return strings.Trim(name, "-")
}
