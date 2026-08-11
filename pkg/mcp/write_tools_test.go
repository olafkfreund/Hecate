package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/ops"
)

var base = time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

func opsFor(t *testing.T, objs ...client.Object) *ops.Ops {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Gate{}, &v1alpha1.Bundle{}, &v1alpha1.Passage{}).
		Build()
	return &ops.Ops{Client: c, Now: func() metav1.Time { return metav1.Time{Time: base} }}
}

func gateFor(name string, opts ...func(*v1alpha1.Gate)) *v1alpha1.Gate {
	g := &v1alpha1.Gate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "acme"},
		Spec: v1alpha1.GateSpec{
			Admits:  []v1alpha1.Admission{{From: v1alpha1.BundleOrigin{Beacon: "app"}}},
			Passage: &v1alpha1.PassageTemplate{Steps: []v1alpha1.Step{{Uses: "flux-wait"}}},
		},
	}
	for _, o := range opts {
		o(g)
	}
	return g
}

func bundleFor(name string) *v1alpha1.Bundle {
	return &v1alpha1.Bundle{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "acme",
			CreationTimestamp: metav1.Time{Time: base},
		},
		Spec: v1alpha1.BundleSpec{Beacon: "app"},
	}
}

// serverWith builds a server exactly as the binary does.
func serverWith(t *testing.T, o *ops.Ops, allowWrites bool) *Server {
	t.Helper()
	s := New("hecate", "test", "")
	s.MustRegister(ReadTools(o, "acme")...)
	if allowWrites {
		s.MustRegister(WriteTools(o, "acme", "olaf@example.com")...)
	}
	return s
}

func toolNames(t *testing.T, s *Server) []string {
	t.Helper()
	r := result(t, ask(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{`+modernMeta+`}}`))
	raw, _ := r["tools"].([]any)
	var names []string
	for _, item := range raw {
		tool, _ := item.(map[string]any)
		names = append(names, tool["name"].(string))
	}
	return names
}

// The decision this file exists to enforce: approval is a segregation-of-duties
// control, an agent that can satisfy it makes the control meaningless, and so
// there is no configuration of this server that exposes it.
//
// A flag to enable it would be a flag to make the guarantee untrue. This test
// asserts the guarantee holds without anyone having to check how the server was
// started.
func TestApproveIsNeverExposed(t *testing.T) {
	o := opsFor(t, gateFor("staging"), bundleFor("b1"))

	for _, allowWrites := range []bool{false, true} {
		s := serverWith(t, o, allowWrites)
		for _, name := range toolNames(t, s) {
			if strings.Contains(name, "approve") {
				t.Errorf("allowWrites=%v exposed %q", allowWrites, name)
			}
		}

		// And calling it by name must not work either, in case a model guesses
		// the name from the CLI's.
		reply := ask(t, s,
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{`+modernMeta+`,"name":"approve","arguments":{"bundle":"b1","gate":"staging"}}}`)
		if _, isError := reply["error"]; !isError {
			t.Errorf("allowWrites=%v answered a call to approve: %v", allowWrites, reply)
		}
	}

	// The Bundle must be unapproved afterwards — the point is that nothing
	// happened, not merely that the reply was an error.
	b, err := o.Bundle(context.Background(), "acme", "b1")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Status.ApprovedFor) != 0 {
		t.Errorf("something approved the Bundle: %v", b.Status.ApprovedFor)
	}
}

// Off by default: a tool a model can call is a tool it will call.
func TestWritesAreOffByDefault(t *testing.T) {
	o := opsFor(t, gateFor("staging"), bundleFor("b1"))
	s := serverWith(t, o, false)

	names := strings.Join(toolNames(t, s), ",")
	for _, forbidden := range []string{"promote", "abort"} {
		if strings.Contains(names, forbidden) {
			t.Errorf("%s is exposed on a read-only server: %s", forbidden, names)
		}
	}

	reply := ask(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{`+modernMeta+`,"name":"promote","arguments":{"gate":"staging","bundle":"b1"}}}`)
	if _, isError := reply["error"]; !isError {
		t.Errorf("a read-only server promoted something: %v", reply)
	}

	var passages v1alpha1.PassageList
	if err := o.Client.List(context.Background(), &passages); err != nil {
		t.Fatal(err)
	}
	if len(passages.Items) != 0 {
		t.Errorf("a read-only server opened %d Passage(s)", len(passages.Items))
	}
}

func TestPromoteAndAbort(t *testing.T) {
	o := opsFor(t, gateFor("staging"), bundleFor("b1"))
	s := serverWith(t, o, true)

	r := result(t, ask(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{`+modernMeta+`,"name":"promote","arguments":{"gate":"staging","bundle":"b1"}}}`))
	if r["isError"] == true {
		t.Fatalf("promote failed: %v", r["content"])
	}
	structured, _ := r["structuredContent"].(map[string]any)
	passage, _ := structured["passage"].(string)
	if passage == "" {
		t.Fatalf("no Passage name in %v", structured)
	}

	// An agent acting for someone is not that person acting, and the trail
	// should say which.
	if actor, _ := structured["actor"].(string); actor != ActorPrefix+"olaf@example.com" {
		t.Errorf("actor = %q, want it marked as having come through MCP", actor)
	}

	// Then stop it.
	r = result(t, ask(t, s,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{`+modernMeta+`,"name":"abort","arguments":{"passage":"`+passage+`"}}}`))
	if r["isError"] == true {
		t.Fatalf("abort failed: %v", r["content"])
	}

	got, err := o.Passage(context.Background(), "acme", passage)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Spec.Abort {
		t.Error("the Passage was not asked to stop")
	}
}

// An MCP client is a client, not a bypass: the rules are pkg/ops', and a
// refusal reaches the model as something it can read rather than as a silent
// failure or a protocol error it cannot act on.
func TestPromoteObeysTheSameRules(t *testing.T) {
	for _, tc := range []struct {
		name string
		gate *v1alpha1.Gate
		says string
	}{
		{
			"a Bundle that has not cleared upstream",
			gateFor("staging", func(g *v1alpha1.Gate) { g.Spec.Admits[0].After = []string{"dev"} }),
			"has not cleared dev",
		},
		{
			"a Bundle nobody has approved",
			gateFor("staging", func(g *v1alpha1.Gate) { g.Spec.Admits[0].RequireApproval = true }),
			"awaiting approval",
		},
		{
			"a suspended Gate",
			gateFor("staging", func(g *v1alpha1.Gate) { g.Spec.Suspend = true }),
			"suspended",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := opsFor(t, tc.gate, bundleFor("b1"))
			s := serverWith(t, o, true)

			r := result(t, ask(t, s,
				`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{`+modernMeta+`,"name":"promote","arguments":{"gate":"staging","bundle":"b1"}}}`))

			if r["isError"] != true {
				t.Fatalf("the promotion was not refused: %v", r)
			}
			content, _ := r["content"].([]any)
			first, _ := content[0].(map[string]any)
			text, _ := first["text"].(string)
			if !strings.Contains(text, tc.says) {
				t.Errorf("the model is not told why: %q", text)
			}

			var passages v1alpha1.PassageList
			if err := o.Client.List(context.Background(), &passages); err != nil {
				t.Fatal(err)
			}
			if len(passages.Items) != 0 {
				t.Errorf("a refused promotion still opened %d Passage(s)", len(passages.Items))
			}
		})
	}
}

// The model is told plainly what it must not attempt, so it reports the
// blockage rather than hunting for another route.
func TestWriteInstructionsRefuseApproval(t *testing.T) {
	var tools []Tool
	tools = append(tools, WriteTools(opsFor(t), "acme", "olaf")...)
	if len(tools) != 2 {
		t.Fatalf("got %d write tools, want exactly promote and abort", len(tools))
	}

	// And the constant that records the reasoning is not vacuous.
	if !strings.Contains(ApproveIsNotAvailable, "segregation-of-duties") {
		t.Errorf("ApproveIsNotAvailable = %q", ApproveIsNotAvailable)
	}
}

func TestWriteToolsValidateArguments(t *testing.T) {
	o := opsFor(t, gateFor("staging"), bundleFor("b1"))
	s := serverWith(t, o, true)

	for _, args := range []string{
		`"name":"promote","arguments":{"gate":"staging"}`,
		`"name":"promote","arguments":{"bundle":"b1"}`,
		`"name":"abort","arguments":{}`,
	} {
		r := result(t, ask(t, s,
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{`+modernMeta+`,`+args+`}}`))
		if r["isError"] != true {
			t.Errorf("%s was accepted", args)
		}
	}
}
