package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	yaml "go.yaml.in/yaml/v3"

	"github.com/olafkfreund/hecate/api/v1alpha1"
	"github.com/olafkfreund/hecate/pkg/passage"
)

// Step names.
const (
	StepEditYAML = "edit-yaml"
	StepSetImage = "set-image"
)

// Failure reasons for the edit steps.
const (
	// ReasonFileNotFound means the file the step was told to edit is not in the
	// checkout. Usually a path typo, or an edit step running before git-clone.
	ReasonFileNotFound = "FileNotFound"
	// ReasonKeyNotFound means the file exists but the field does not. Edit steps
	// update fields; they never create them, so this needs a human.
	ReasonKeyNotFound = "KeyNotFound"
	// ReasonEditFailed is any other refusal to write — an unparseable file, or a
	// field whose shape the surgical editor will not touch.
	ReasonEditFailed = "EditFailed"
)

// ---------------------------------------------------------- yaml surgery ----

// document is a file being edited: the original bytes, plus a parse of them.
//
// Edits are applied to the bytes, not to the parse tree, and the tree is
// rebuilt after each one. Re-encoding a YAML tree rewrites the entire file —
// dropping comments, renormalising quotes and indentation — which would make
// every promotion diff unreviewable. Replacing exactly the scalar's span leaves
// the rest of the file byte-identical.
type document struct {
	name string
	raw  []byte
	docs []*yaml.Node
}

func parseDocument(name string, raw []byte) (*document, error) {
	d := &document{name: name, raw: raw}
	return d, d.reparse()
}

func (d *document) reparse() error {
	dec := yaml.NewDecoder(strings.NewReader(string(d.raw)))
	d.docs = nil
	for {
		var n yaml.Node
		err := dec.Decode(&n)
		if err != nil {
			// io.EOF ends the stream; anything else is a broken file. The
			// decoder reports no sentinel for EOF other than the error string
			// being io.EOF, so compare against it by value.
			if err.Error() == "EOF" {
				return nil
			}
			return passage.FailTerminal(ReasonEditFailed, "%s: %s", d.name, err)
		}
		copied := n
		d.docs = append(d.docs, &copied)
	}
}

// find locates the scalar at path in whichever document contains it.
func (d *document) find(path string) (*yaml.Node, error) {
	var found *yaml.Node
	for _, doc := range d.docs {
		n, err := resolve(doc, path)
		if err != nil || n == nil {
			continue
		}
		if found != nil {
			// Two matches means the edit is ambiguous, and picking one silently
			// would update the wrong Deployment in a multi-document manifest.
			return nil, passage.FailTerminal(ReasonEditFailed,
				"%s: %q matches more than one document — split the file, "+
					"or point the step at a path unique to one", d.name, path)
		}
		found = n
	}
	if found == nil {
		return nil, passage.FailTerminal(ReasonKeyNotFound,
			"%s: no field at %q — edit steps update fields, they do not create them", d.name, path)
	}
	if found.Kind != yaml.ScalarNode {
		return nil, passage.FailTerminal(ReasonEditFailed,
			"%s: %q is not a single value", d.name, path)
	}
	return found, nil
}

// locator finds the node an edit targets. It is a function rather than a node
// because replace has to find it twice: once to edit, once to check the edit.
type locator struct {
	what string
	find func() (*yaml.Node, error)
}

// replace swaps the scalar's text in the raw bytes for text, and re-parses.
//
// want is the value the field should hold afterwards; the file is re-parsed and
// the node found again and checked against it before anything is written. That
// check is what makes the byte surgery safe: a mis-computed span corrupts the
// file loudly here rather than quietly in the repository.
func (d *document) replace(loc locator, text, want string) error {
	node, err := loc.find()
	if err != nil {
		return err
	}
	path := loc.what
	if node.Style == yaml.LiteralStyle || node.Style == yaml.FoldedStyle {
		return passage.FailTerminal(ReasonEditFailed,
			"%s: %q is a block scalar, which this step will not rewrite", d.name, path)
	}

	lines := strings.Split(string(d.raw), "\n")
	if node.Line < 1 || node.Line > len(lines) {
		return passage.FailTerminal(ReasonEditFailed, "%s: %q is at no line in the file", d.name, path)
	}
	runes := []rune(lines[node.Line-1])
	if node.Column < 1 || node.Column > len(runes)+1 {
		return passage.FailTerminal(ReasonEditFailed, "%s: %q is at no column in the file", d.name, path)
	}
	head, rest := string(runes[:node.Column-1]), string(runes[node.Column-1:])

	// A trailing comment is part of the line, not part of the value, and often
	// says why the value is pinned. Keep it, with its spacing.
	tail := ""
	if node.LineComment != "" {
		if i := strings.LastIndex(rest, node.LineComment); i >= 0 {
			value := rest[:i]
			gap := value[len(strings.TrimRight(value, " \t")):]
			tail = gap + rest[i:]
		}
	}

	before := d.raw
	lines[node.Line-1] = head + text + tail
	d.raw = []byte(strings.Join(lines, "\n"))

	if err := d.reparse(); err != nil {
		d.raw = before
		return err
	}
	got, err := loc.find()
	if err != nil || got.Value != want {
		d.raw = before
		_ = d.reparse()
		return passage.FailTerminal(ReasonEditFailed,
			"%s: rewriting %q did not produce the value asked for — the file is unchanged", d.name, path)
	}
	return nil
}

// resolve walks a dotted path with optional [n] indices: `spec.template[0].image`.
// A dot inside a key is escaped: `metadata.annotations.flux\.io/x`.
func resolve(n *yaml.Node, path string) (*yaml.Node, error) {
	cur := n
	if cur.Kind == yaml.DocumentNode {
		if len(cur.Content) == 0 {
			return nil, fmt.Errorf("empty document")
		}
		cur = cur.Content[0]
	}
	for _, seg := range splitPath(path) {
		key, indices, err := parseSegment(seg)
		if err != nil {
			return nil, err
		}
		if key != "" {
			if cur.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("%q is not a mapping", key)
			}
			cur = mapValue(cur, key)
			if cur == nil {
				return nil, fmt.Errorf("no key %q", key)
			}
		}
		for _, i := range indices {
			if cur.Kind != yaml.SequenceNode || i < 0 || i >= len(cur.Content) {
				return nil, fmt.Errorf("no item [%d]", i)
			}
			cur = cur.Content[i]
		}
	}
	return cur, nil
}

func splitPath(path string) []string {
	var segs []string
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		switch {
		case path[i] == '\\' && i+1 < len(path) && path[i+1] == '.':
			b.WriteByte('.')
			i++
		case path[i] == '.':
			segs = append(segs, b.String())
			b.Reset()
		default:
			b.WriteByte(path[i])
		}
	}
	return append(segs, b.String())
}

func parseSegment(seg string) (string, []int, error) {
	key, rest, found := strings.Cut(seg, "[")
	if !found {
		return key, nil, nil
	}
	var indices []int
	for rest != "" {
		num, tail, ok := strings.Cut(rest, "]")
		if !ok {
			return "", nil, fmt.Errorf("unclosed [ in %q", seg)
		}
		i, err := strconv.Atoi(num)
		if err != nil {
			return "", nil, fmt.Errorf("index %q in %q is not a number", num, seg)
		}
		indices = append(indices, i)
		rest = strings.TrimPrefix(tail, "[")
	}
	return key, indices, nil
}

// mapValue returns the value node for key, or nil.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// renderScalar turns a JSON value from a step's `with:` block into the exact
// text to write, and the value it should read back as.
func renderScalar(raw json.RawMessage, style yaml.Style) (text, want string, err error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", "", fmt.Errorf("value: %s", err)
	}
	switch v := v.(type) {
	case string:
		return renderString(v, style), v, nil
	case float64, bool:
		// The JSON literal is already the canonical YAML one, and using it
		// verbatim keeps 1.50 from becoming 1.5.
		return strings.TrimSpace(string(raw)), strings.TrimSpace(string(raw)), nil
	default:
		return "", "", fmt.Errorf("value must be a string, number or boolean")
	}
}

// renderString quotes a string only when it has to be quoted — a promotion that
// adds quotes to every value it touches makes a noisy diff — but always when
// leaving it bare would change its type. `newTag: 1.0` is a float.
func renderString(v string, style yaml.Style) string {
	if style == yaml.SingleQuotedStyle || style == yaml.DoubleQuotedStyle {
		return encodeScalar(v, style)
	}
	plain := encodeScalar(v, 0)
	var back any
	if err := yaml.Unmarshal([]byte(plain), &back); err != nil {
		return encodeScalar(v, yaml.DoubleQuotedStyle)
	}
	if s, ok := back.(string); !ok || s != v {
		return encodeScalar(v, yaml.DoubleQuotedStyle)
	}
	if yaml11Bool[strings.ToLower(v)] {
		return encodeScalar(v, yaml.DoubleQuotedStyle)
	}
	return plain
}

// yaml11Bool is checked separately because we do not read these files back —
// Flux and kustomize do, through a YAML 1.1 parser, where a bare `on` or `no`
// is a boolean. Our own 1.2 round-trip says they are strings and would happily
// leave them unquoted.
var yaml11Bool = map[string]bool{
	"y": true, "n": true, "yes": true, "no": true,
	"on": true, "off": true, "true": true, "false": true,
}

func encodeScalar(v string, style yaml.Style) string {
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	if err := enc.Encode(&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v, Style: style}); err != nil {
		return v
	}
	_ = enc.Close()
	return strings.TrimRight(b.String(), "\n")
}

// editFile loads a file from the checkout, applies edit, and writes it back if
// the bytes changed. Reporting an unchanged file as success rather than failure
// is what makes a re-run of a crossing a no-op (D23).
func editFile(sc *passage.StepContext, path string, edit func(*document) error) (bool, error) {
	if path == "" {
		return false, passage.FailTerminal(ReasonInvalidConfig, "path is required")
	}
	full, err := checkoutPath(sc.WorkDir, path)
	if err != nil {
		return false, passage.FailTerminal(ReasonInvalidConfig, "%s", err)
	}
	raw, err := os.ReadFile(full)
	if os.IsNotExist(err) {
		return false, passage.FailTerminal(ReasonFileNotFound,
			"no file at %s — check the path, and that a git-clone step ran first", path)
	}
	if err != nil {
		return false, passage.FailTerminal(ReasonEditFailed, "reading %s: %s", path, err)
	}
	info, err := os.Stat(full)
	if err != nil {
		return false, passage.FailTerminal(ReasonEditFailed, "reading %s: %s", path, err)
	}

	doc, err := parseDocument(path, raw)
	if err != nil {
		return false, err
	}
	if err := edit(doc); err != nil {
		return false, err
	}
	if string(doc.raw) == string(raw) {
		return false, nil
	}
	if err := os.WriteFile(full, doc.raw, info.Mode()); err != nil {
		return false, passage.FailTerminal(ReasonEditFailed, "writing %s: %s", path, err)
	}
	return true, nil
}

// ------------------------------------------------------------- edit-yaml ----

// EditYAMLConfig is the `with:` block of an edit-yaml step.
type EditYAMLConfig struct {
	// Path is the file to edit, relative to the Passage work dir — so a file in
	// the default checkout is `repo/apps/prod/values.yaml`.
	Path string `json:"path"`
	// Edits are applied in order, all or nothing.
	Edits []FieldEdit `json:"edits"`
}

// FieldEdit sets one field to one value.
type FieldEdit struct {
	// Key is a dotted path: `image.tag`, `spec.replicas`, `images[0].newTag`.
	Key string `json:"key"`
	// Value is a string, number or boolean.
	Value json.RawMessage `json:"value"`
}

// EditYAML sets fields in a YAML file without reformatting it.
//
// JSON files work too: JSON is YAML, and replacing a scalar in place leaves the
// document valid JSON.
type EditYAML struct{}

// NewEditYAML returns an edit-yaml step.
func NewEditYAML() *EditYAML { return &EditYAML{} }

// Name implements passage.Runner.
func (e *EditYAML) Name() string { return StepEditYAML }

// Run implements passage.Runner.
func (e *EditYAML) Run(_ context.Context, sc *passage.StepContext) (passage.StepResult, error) {
	cfg, err := passage.DecodeConfig[EditYAMLConfig](sc)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepEditYAML, err)
	}
	if len(cfg.Edits) == 0 {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig,
			"%s: no edits — a step that changes nothing is a mistake, not a no-op", StepEditYAML)
	}

	changed, err := editFile(sc, cfg.Path, func(doc *document) error {
		for _, e := range cfg.Edits {
			if e.Key == "" {
				return passage.FailTerminal(ReasonInvalidConfig, "an edit is missing its key")
			}
			node, err := doc.find(e.Key)
			if err != nil {
				return err
			}
			text, want, err := renderScalar(e.Value, node.Style)
			if err != nil {
				return passage.FailTerminal(ReasonInvalidConfig, "%s: %s", e.Key, err)
			}
			if strings.Contains(text, "\n") {
				return passage.FailTerminal(ReasonEditFailed,
					"%s: the new value does not fit on one line", e.Key)
			}
			// Already correct: leave the line alone rather than rewrite it with
			// our own quoting. A no-op crossing should produce no diff.
			if node.Value == want {
				continue
			}
			if err := doc.replace(locator{e.Key, func() (*yaml.Node, error) {
				return doc.find(e.Key)
			}}, text, want); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return passage.StepResult{}, err
	}

	return passage.StepResult{
		Phase:   v1alpha1.StepSucceeded,
		Message: editMessage(changed, cfg.Path, len(cfg.Edits)),
		Output:  map[string]any{"changed": changed, "file": cfg.Path},
	}, nil
}

func editMessage(changed bool, path string, n int) string {
	if !changed {
		return fmt.Sprintf("%s already holds the requested values", path)
	}
	return fmt.Sprintf("updated %d field(s) in %s", n, path)
}

// ------------------------------------------------------------- set-image ----

// SetImageConfig is the `with:` block of a set-image step.
type SetImageConfig struct {
	// Path is the kustomization to edit, relative to the Passage work dir.
	Path string `json:"path"`
	// Image is the repository to repin, matched against the `name:` of an entry
	// in the file's `images:` list.
	Image string `json:"image"`
	// Tag and Digest are what to pin it to. Leave both empty to take them from
	// the Bundle being promoted, which is the usual case.
	Tag    string `json:"tag,omitempty"`
	Digest string `json:"digest,omitempty"`
}

// SetImage repins an image in a kustomization's `images:` list.
//
// Separate from edit-yaml because a kustomize images entry is addressed by the
// repository it names, not by its position: `images[2].newTag` silently repins
// the wrong image the day somebody reorders the list.
//
// Values files are edit-yaml's job — there the author knows the key, and a
// second step that guesses at it would only guess wrong.
type SetImage struct{}

// NewSetImage returns a set-image step.
func NewSetImage() *SetImage { return &SetImage{} }

// Name implements passage.Runner.
func (s *SetImage) Name() string { return StepSetImage }

// Run implements passage.Runner.
func (s *SetImage) Run(_ context.Context, sc *passage.StepContext) (passage.StepResult, error) {
	cfg, err := passage.DecodeConfig[SetImageConfig](sc)
	if err != nil {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig, "%s: %s", StepSetImage, err)
	}
	if cfg.Image == "" {
		return passage.StepResult{}, passage.FailTerminal(ReasonInvalidConfig,
			"%s: image is required", StepSetImage)
	}
	tag, digest := cfg.Tag, cfg.Digest
	if tag == "" && digest == "" {
		tag, digest, err = imageFromBundle(sc.Bundle, cfg.Image)
		if err != nil {
			return passage.StepResult{}, err
		}
	}

	var pinned string
	changed, err := editFile(sc, cfg.Path, func(doc *document) error {
		node, field, err := imageEntry(doc, cfg.Image)
		if err != nil {
			return err
		}
		// Which field the entry already uses decides what we write: switching a
		// tag pin to a digest pin changes the file's shape, and that is the
		// author's decision to make once, not ours to make every crossing.
		pinned = tag
		if field == "digest" {
			pinned = digest
		}
		if pinned == "" {
			return passage.FailTerminal(ReasonInvalidConfig,
				"%s pins %s by %s, but no %s was given or found in the Bundle",
				doc.name, cfg.Image, field, field)
		}
		if node.Value == pinned {
			return nil
		}
		text, want, err := renderScalar(json.RawMessage(strconv.Quote(pinned)), node.Style)
		if err != nil {
			return passage.FailTerminal(ReasonInvalidConfig, "%s", err)
		}
		return doc.replace(locator{cfg.Image, func() (*yaml.Node, error) {
			n, _, err := imageEntry(doc, cfg.Image)
			return n, err
		}}, text, want)
	})
	if err != nil {
		return passage.StepResult{}, err
	}

	msg := fmt.Sprintf("%s already pinned to %s in %s", cfg.Image, pinned, cfg.Path)
	if changed {
		msg = fmt.Sprintf("pinned %s to %s in %s", cfg.Image, pinned, cfg.Path)
	}
	return passage.StepResult{
		Phase:   v1alpha1.StepSucceeded,
		Message: msg,
		Output:  map[string]any{"changed": changed, "file": cfg.Path, "image": cfg.Image, "pinned": pinned},
	}, nil
}

// imageEntry finds the `images:` entry naming repo, and the node holding its
// pin, along with which field that is.
func imageEntry(doc *document, repo string) (*yaml.Node, string, error) {
	for _, d := range doc.docs {
		root := d
		if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
			root = root.Content[0]
		}
		if root.Kind != yaml.MappingNode {
			continue
		}
		images := mapValue(root, "images")
		if images == nil || images.Kind != yaml.SequenceNode {
			continue
		}
		for _, entry := range images.Content {
			if entry.Kind != yaml.MappingNode {
				continue
			}
			name := mapValue(entry, "name")
			if name == nil || name.Value != repo {
				continue
			}
			for _, field := range []string{"newTag", "digest"} {
				if n := mapValue(entry, field); n != nil && n.Kind == yaml.ScalarNode {
					return n, map[string]string{"newTag": "tag", "digest": "digest"}[field], nil
				}
			}
			return nil, "", passage.FailTerminal(ReasonKeyNotFound,
				"%s: the images entry for %s has neither newTag nor digest — "+
					"pin it once by hand, so a promotion only ever changes the version",
				doc.name, repo)
		}
	}
	return nil, "", passage.FailTerminal(ReasonKeyNotFound,
		"%s: no images entry named %s", doc.name, repo)
}

// imageFromBundle finds the version this crossing is promoting.
func imageFromBundle(bundle *v1alpha1.Bundle, repo string) (tag, digest string, err error) {
	if bundle == nil {
		return "", "", passage.FailTerminal(ReasonInvalidConfig,
			"no tag or digest given and no Bundle to take one from")
	}
	for _, a := range bundle.Spec.Artifacts {
		if a.Image != nil && a.Image.Repo == repo {
			return a.Image.Tag, a.Image.Digest, nil
		}
	}
	return "", "", passage.FailTerminal(ReasonInvalidConfig,
		"Bundle %s carries no image %s — the Gate is promoting something else",
		bundle.Name, repo)
}
