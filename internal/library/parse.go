// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package library

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// fileCtx names the file being parsed.
type fileCtx struct {
	name string // e.g. "rss.yaml"
	typ  Type
}

// issue is one finding at a node line.
type issue struct {
	msg  string
	line int
}

// parser collects findings and the valid sources of every file.
type parser struct {
	diags Diagnostics
	refs  []sourceRef // every valid source, across files
	files []draftFile
}

// draftFile is a file whose top-level keys are valid; groups hold indexes into refs.
type draftFile struct {
	file   File
	groups []draftGroup
}

// draftGroup is a valid group; refs are its source indexes into parser.refs.
type draftGroup struct {
	group Group
	refs  []int
}

// topKeys is the required top-level key order.
var topKeys = []string{"schema_version", "type", "description", "groups"}

// keyHints explains reserved and renamed keys in unknown key findings.
var keyHints = map[string]string{
	"tags":        "reserved for #40 item tags, use labels",
	"keywords":    "reserved for #40 item tags",
	"assumptions": "reserved for #40 item tags",
	"aliases":     "reserved for #40 item tags",
	"categories":  "renamed to groups",
}

var groupNamePattern = regexp.MustCompile("^[a-z0-9-]+$")

// ParseFS reads every *.yaml in fsys except backlog.yaml and returns the valid subset plus every finding.
// A file it cannot read, an unknown stem or a newer schema_version is skipped; an invalid group or source
// is dropped and its siblings kept; an unknown key or key order is reported and the data kept.
// A directory with no library files is one finding with no file name.
// A repo check requires no findings; a loader logs them and uses the result.
func ParseFS(fsys fs.FS) (*Library, Diagnostics) {
	// Glob fails only on a bad pattern, and this pattern is constant.
	names, _ := fs.Glob(fsys, "*.yaml")
	slices.SortStableFunc(names, compareFileNames)
	p := &parser{}
	parsed := 0
	for _, name := range names {
		if name != "backlog.yaml" {
			p.parseFile(fsys, name)
			parsed++
		}
	}
	if parsed == 0 {
		p.diags = append(p.diags, Diagnostic{Message: "no library files found"})
	}
	diags, dropped := checkLibrary(p.refs)
	p.diags = append(p.diags, diags...)
	lib := p.build(dropped)
	p.diags.sort()
	return lib, p.diags
}

// compareFileNames orders library files rss, then academic, then unknown stems by name.
func compareFileNames(a, b string) int {
	ra, okA := typeOrder(Type(strings.TrimSuffix(a, ".yaml")))
	rb, okB := typeOrder(Type(strings.TrimSuffix(b, ".yaml")))
	switch {
	case okA && okB:
		return cmp.Compare(ra, rb)
	case okA:
		return -1
	case okB:
		return 1
	}
	return cmp.Compare(a, b)
}

// parseFile reads one library file into p, or records why it is skipped.
func (p *parser) parseFile(fsys fs.FS, name string) {
	fc := fileCtx{name: name, typ: Type(strings.TrimSuffix(name, ".yaml"))}
	if _, ok := typeOrder(fc.typ); !ok {
		p.report(fc, "", 0, "file name must be rss.yaml or academic.yaml (backlog.yaml is skipped)")
		return
	}
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		p.report(fc, "", 0, err.Error())
		return
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err = dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			p.report(fc, "", 0, "file is empty")
		} else {
			p.report(fc, "", 0, "does not parse: "+err.Error())
		}
		return
	}
	if err = dec.Decode(&yaml.Node{}); !errors.Is(err, io.EOF) {
		p.report(fc, "", 0, "file must hold one YAML document")
		return
	}
	if len(doc.Content) == 0 || isNull(doc.Content[0]) {
		p.report(fc, "", 0, "file is empty")
		return
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		p.report(fc, "", root.Line, "top level must be a mapping")
		return
	}
	if issues := structuralIssues(root); len(issues) > 0 {
		p.reportAll(fc, "", issues)
		return
	}
	p.parseTop(fc, root)
}

// structuralIssues returns one issue per anchor, alias or merge key under n, once per line.
func structuralIssues(n *yaml.Node) []issue {
	var out []issue
	seen := map[int]bool{}
	add := func(line int) {
		if !seen[line] {
			seen[line] = true
			out = append(out, issue{"anchors, aliases and merge keys are not allowed", line})
		}
	}
	var walk func(*yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Anchor != "" || n.Kind == yaml.AliasNode {
			add(n.Line)
		}
		for i, c := range n.Content {
			if n.Kind == yaml.MappingNode && i%2 == 0 && (c.Tag == "!!merge" || c.Value == "<<") {
				add(c.Line)
			}
			walk(c)
		}
	}
	walk(n)
	return out
}

// parseTop validates the top-level mapping of a file and parses its groups.
func (p *parser) parseTop(fc fileCtx, root *yaml.Node) {
	keys, vals, problems := pairs(root)
	if len(problems) > 0 {
		p.reportAll(fc, "", problems)
		return
	}
	p.reportUnknown(fc, "", root, topKeys...)
	p.checkOrder(fc, root, keys)
	complete := true
	for _, k := range []string{"schema_version", "type", "groups"} {
		if !p.requireKey(fc, "", root, vals, k) {
			complete = false
		}
	}
	if !complete {
		return
	}

	valid := p.checkSchemaVersion(fc, vals["schema_version"])
	if !p.checkTypeName(fc, vals["type"]) {
		valid = false
	}
	desc := p.fileDescription(fc, root, vals["description"])
	groups := vals["groups"]
	if groups.Kind != yaml.SequenceNode || len(groups.Content) == 0 {
		p.report(fc, "", groups.Line, "groups must be a non-empty list")
		return
	}
	if !valid {
		return
	}
	draft := draftFile{file: File{Name: fc.name, SchemaVersion: SchemaVersion, Type: fc.typ, Description: desc}}
	draft.groups = p.parseGroups(fc, groups)
	p.files = append(p.files, draft)
}

// checkOrder reports top-level keys out of order, when all four known keys are present.
func (p *parser) checkOrder(fc fileCtx, root *yaml.Node, keys []string) {
	var known []string
	for _, k := range keys {
		if slices.Contains(topKeys, k) {
			known = append(known, k)
		}
	}
	if len(known) == len(topKeys) && !slices.Equal(known, topKeys) {
		p.report(fc, "", root.Line, "top-level keys must be in the order schema_version, type, description, groups")
	}
}

// checkSchemaVersion reports a schema_version that is not a supported integer.
func (p *parser) checkSchemaVersion(fc fileCtx, sv *yaml.Node) bool {
	n, isInt := intValue(sv)
	switch {
	case isInt && n > SchemaVersion:
		p.report(fc, "", sv.Line, fmt.Sprintf("schema_version %d is newer than supported %d", n, SchemaVersion))
		return false
	case !isInt || n != SchemaVersion:
		p.report(fc, "", sv.Line, fmt.Sprintf("schema_version must be the integer %d, got %q", SchemaVersion, sv.Value))
		return false
	}
	return true
}

// checkTypeName reports a type that does not equal the file stem.
func (p *parser) checkTypeName(fc fileCtx, t *yaml.Node) bool {
	if isStr(t) && t.Value == string(fc.typ) {
		return true
	}
	p.report(fc, "", t.Line, fmt.Sprintf("type must equal the file stem %q, got %q", fc.typ, t.Value))
	return false
}

// fileDescription returns the file description, reporting it when missing or blank.
func (p *parser) fileDescription(fc fileCtx, root, d *yaml.Node) string {
	if d != nil && isStr(d) && strings.TrimSpace(d.Value) != "" {
		return d.Value
	}
	line := root.Line
	if d != nil {
		line = d.Line
	}
	p.report(fc, "", line, "description must be a non-empty string")
	return ""
}

// parseGroups parses each item of the groups list; invalid groups are dropped.
func (p *parser) parseGroups(fc fileCtx, list *yaml.Node) []draftGroup {
	var out []draftGroup
	seen := map[string]bool{}
	for i, item := range list.Content {
		if dg, ok := p.parseGroup(fc, i, item, seen); ok {
			out = append(out, dg)
		}
	}
	return out
}

// parseGroup validates one group and its sources; ok is false when the group is dropped.
func (p *parser) parseGroup(fc fileCtx, i int, n *yaml.Node, seen map[string]bool) (draftGroup, bool) {
	path := fmt.Sprintf("groups[%d]", i)
	if n.Kind != yaml.MappingNode {
		p.report(fc, path, n.Line, "group must be a mapping")
		return draftGroup{}, false
	}
	_, vals, problems := pairs(n)
	p.reportAll(fc, path, problems)
	name, nameOK := p.groupName(fc, path, n, vals, seen)
	if nameOK {
		path = name
	}
	p.reportUnknown(fc, path, n, "name", "description", "sources")
	fieldsOK := p.checkGroupFields(fc, path, n, vals)
	if !nameOK || !fieldsOK || len(problems) > 0 {
		return draftGroup{}, false
	}

	dg := draftGroup{group: Group{Name: name, Description: vals["description"].Value}}
	for si, src := range vals["sources"].Content {
		if idx, ok := p.parseSource(fc, name, si, src); ok {
			dg.refs = append(dg.refs, idx)
		}
	}
	return dg, true
}

// groupName returns the group name when it is a valid, unused slug.
func (p *parser) groupName(fc fileCtx, path string, owner *yaml.Node, vals map[string]*yaml.Node,
	seen map[string]bool) (string, bool) {
	nm, ok := vals["name"]
	if !ok {
		p.report(fc, path, owner.Line, `missing key "name"`)
		return "", false
	}
	if !isStr(nm) || !groupNamePattern.MatchString(nm.Value) {
		p.report(fc, path, nm.Line, fmt.Sprintf("name %q must match ^[a-z0-9-]+$", nm.Value))
		return "", false
	}
	if seen[nm.Value] {
		p.report(fc, nm.Value, nm.Line, fmt.Sprintf("duplicate group name %q", nm.Value))
		return "", false
	}
	seen[nm.Value] = true
	return nm.Value, true
}

// checkGroupFields reports the description and sources problems of a group.
func (p *parser) checkGroupFields(fc fileCtx, path string, n *yaml.Node, vals map[string]*yaml.Node) bool {
	ok := true
	if p.requireKey(fc, path, n, vals, "description") {
		if d := vals["description"]; !isStr(d) || !singleLine(d.Value) {
			p.report(fc, path, d.Line, "description must be a non-empty single line")
			ok = false
		}
	} else {
		ok = false
	}
	if p.requireKey(fc, path, n, vals, "sources") {
		if s := vals["sources"]; s.Kind != yaml.SequenceNode || len(s.Content) == 0 {
			p.report(fc, path, s.Line, "sources must be a non-empty list")
			ok = false
		}
	} else {
		ok = false
	}
	return ok
}

// parseSource validates one source and returns its index in p.refs; ok is false when it is dropped.
func (p *parser) parseSource(fc fileCtx, group string, si int, n *yaml.Node) (int, bool) {
	path := fmt.Sprintf("%s/sources[%d]", group, si)
	if n.Kind != yaml.MappingNode {
		p.report(fc, path, n.Line, "source must be a mapping")
		return 0, false
	}
	_, vals, problems := pairs(n)
	if nm, ok := vals["name"]; ok && isStr(nm) {
		path = group + "/" + nm.Value
	}
	p.reportUnknown(fc, path, n, "name", "url", "description", "labels", "max_items")
	src, issues := readSource(n, vals)
	issues = append(problems, issues...)
	if len(issues) > 0 {
		p.reportAll(fc, path, issues)
		return 0, false
	}
	if msgs := checkSource(fc.typ, src); len(msgs) > 0 {
		for _, msg := range msgs {
			p.report(fc, path, n.Line, msg)
		}
		return 0, false
	}
	p.refs = append(p.refs, sourceRef{file: fc.name, line: n.Line, path: path, src: src})
	return len(p.refs) - 1, true
}

// readSource reads the fields of a source mapping, returning one issue per missing key or wrong type.
func readSource(n *yaml.Node, vals map[string]*yaml.Node) (Source, []issue) {
	var out []issue
	for _, k := range []string{"name", "url", "description"} {
		v, ok := vals[k]
		switch {
		case !ok:
			out = append(out, issue{fmt.Sprintf("missing required key %q", k), n.Line})
		case !isStr(v):
			out = append(out, issue{k + " must be a string", v.Line})
		}
	}
	labels, ok := vals["labels"]
	switch {
	case !ok:
		out = append(out, issue{`missing required key "labels"`, n.Line})
	case !isStrList(labels):
		out = append(out, issue{"labels must be a list of strings", labels.Line})
	}
	if mi, ok := vals["max_items"]; ok {
		if v, isInt := intValue(mi); !isInt || v <= 0 {
			out = append(out, issue{"max_items must be a positive integer", mi.Line})
		}
	}
	if len(out) > 0 {
		return Source{}, out
	}
	src := Source{
		Name:        vals["name"].Value,
		URL:         vals["url"].Value,
		Description: vals["description"].Value,
		Labels:      strValues(labels),
	}
	if mi, ok := vals["max_items"]; ok {
		src.MaxItems, _ = intValue(mi)
	}
	return src, nil
}

// requireKey reports a missing key of owner and returns whether it is present.
func (p *parser) requireKey(fc fileCtx, path string, owner *yaml.Node, vals map[string]*yaml.Node, key string) bool {
	if _, ok := vals[key]; ok {
		return true
	}
	p.report(fc, path, owner.Line, fmt.Sprintf("missing key %q", key))
	return false
}

// reportUnknown reports each key of mapping n outside allowed, once per name.
func (p *parser) reportUnknown(fc fileCtx, path string, n *yaml.Node, allowed ...string) {
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i]
		if k.Kind != yaml.ScalarNode || seen[k.Value] || slices.Contains(allowed, k.Value) {
			continue
		}
		seen[k.Value] = true
		msg := fmt.Sprintf("unknown key %q", k.Value)
		if hint, ok := keyHints[k.Value]; ok {
			msg += ": " + hint
		}
		p.report(fc, path, k.Line, msg)
	}
}

// pairs maps each scalar key of mapping n to its value. A non-scalar key is a problem and is skipped,
// and a repeated key is a problem at its repeat, keeping the first value.
func pairs(n *yaml.Node) (keys []string, vals map[string]*yaml.Node, problems []issue) {
	vals = map[string]*yaml.Node{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Kind != yaml.ScalarNode {
			problems = append(problems, issue{"mapping key must be a scalar", k.Line})
			continue
		}
		if _, dup := vals[k.Value]; dup {
			problems = append(problems, issue{fmt.Sprintf("duplicate key %q", k.Value), k.Line})
			continue
		}
		keys = append(keys, k.Value)
		vals[k.Value] = v
	}
	return keys, vals, problems
}

// build assembles the kept files, leaving out dropped sources and then the groups and files left empty.
func (p *parser) build(dropped map[int]bool) *Library {
	lib := &Library{}
	for _, df := range p.files {
		f := df.file
		for _, dg := range df.groups {
			g := dg.group
			for _, idx := range dg.refs {
				if !dropped[idx] {
					g.Sources = append(g.Sources, p.refs[idx].src)
				}
			}
			if len(g.Sources) > 0 {
				f.Groups = append(f.Groups, g)
			}
		}
		if len(f.Groups) > 0 {
			lib.Files = append(lib.Files, f)
		}
	}
	slices.SortStableFunc(lib.Files, func(a, b File) int {
		ra, _ := typeOrder(a.Type)
		rb, _ := typeOrder(b.Type)
		return cmp.Compare(ra, rb)
	})
	return lib
}

// report records one finding against fc.
func (p *parser) report(fc fileCtx, path string, line int, msg string) {
	p.diags = append(p.diags, Diagnostic{File: fc.name, Line: line, Path: path, Message: msg})
}

// reportAll records each issue against fc and path.
func (p *parser) reportAll(fc fileCtx, path string, issues []issue) {
	for _, is := range issues {
		p.report(fc, path, is.line, is.msg)
	}
}

// isStr reports whether n is a plain string scalar.
func isStr(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Tag == "!!str"
}

// isStrList reports whether n is a sequence of string scalars.
func isStrList(n *yaml.Node) bool {
	if n.Kind != yaml.SequenceNode {
		return false
	}
	for _, c := range n.Content {
		if !isStr(c) {
			return false
		}
	}
	return true
}

// strValues returns the values of the scalars in n.
func strValues(n *yaml.Node) []string {
	out := make([]string, 0, len(n.Content))
	for _, c := range n.Content {
		out = append(out, c.Value)
	}
	return out
}

// isNull reports whether n is a null scalar.
func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}

// intValue returns the integer value of an !!int scalar.
func intValue(n *yaml.Node) (int, bool) {
	if n.Kind != yaml.ScalarNode || n.Tag != "!!int" {
		return 0, false
	}
	v, err := strconv.Atoi(n.Value)
	return v, err == nil
}

// singleLine reports whether s has text and no line break once trimmed.
func singleLine(s string) bool {
	t := strings.TrimSpace(s)
	return t != "" && !strings.Contains(t, "\n")
}
