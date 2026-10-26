// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package library

import (
	"cmp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// Source key lines shared by the fixtures; a case copies one and changes one line.
const (
	keyName   = "name: Go blog"
	keyURL    = "url: https://go.dev/blog/feed.atom"
	keyDesc   = "description: Release notes and design posts from the Go team."
	keyLabels = "labels: [Go, Open Source]"
)

// sourceItem returns one item of a sources list whose key lines are lines.
func sourceItem(lines ...string) string {
	var b strings.Builder
	for i, l := range lines {
		indent := "        "
		if i == 0 {
			indent = "      - "
		}
		b.WriteString(indent + l + "\n")
	}
	return b.String()
}

// groupItem returns one item of a groups list with the given name, description value and source items.
func groupItem(name, desc string, sources ...string) string {
	return "  - name: " + name + "\n    description: " + desc + "\n    sources:\n" + strings.Join(sources, "")
}

// topBlock returns the top-level keys that precede groups.
func topBlock(schemaVersion, typ, desc string) string {
	return "schema_version: " + schemaVersion + "\ntype: " + typ + "\ndescription: " + desc + "\n"
}

// fileYAML joins a top-level block and a groups list into one file.
func fileYAML(top string, groups ...string) string {
	return top + "groups:\n" + strings.Join(groups, "")
}

// Fixtures built from the helpers above.
var (
	goodSource   = sourceItem(keyName, keyURL, keyDesc, keyLabels)
	otherSource  = sourceItem("name: Other blog", "url: https://example.org/feed.xml", keyDesc, keyLabels)
	goodTop      = topBlock("1", "rss", "Engineering blogs.")
	goodGroup    = groupItem("blogs", "Engineering blogs.", goodSource)
	goodRSS      = fileYAML(goodTop, goodGroup)
	academicTop  = topBlock("1", "academic", "Scholarly search feeds.")
	academicDesc = "Newest arXiv papers on agentic coding."
	academicGood = fileYAML(academicTop, groupItem("papers", "Agent research.",
		sourceItem("name: arXiv agentic coding", "url: https://arxiv.org/search?q=agentic+coding",
			"description: "+academicDesc, "labels: [AI, Papers]")))
)

// parseFiles runs ParseFS over an in-memory library directory.
func parseFiles(files map[string]string) (*Library, Diagnostics) {
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return ParseFS(fsys)
}

// parseOne runs ParseFS over a directory holding only file.
func parseOne(file, body string) (*Library, Diagnostics) {
	return parseFiles(map[string]string{file: body})
}

// requireDiag fails unless a finding in file at path has msg as a substring of its message.
func requireDiag(t *testing.T, diags Diagnostics, file, path, msg string) {
	t.Helper()
	for _, d := range diags {
		if d.File == file && d.Path == path && strings.Contains(d.Message, msg) {
			return
		}
	}
	t.Fatalf("diags = %+v, want %q in %s at path %q", diags, msg, file, path)
}

// shapeOf counts the kept files and groups and lists the names of the kept sources in order.
func shapeOf(lib *Library) (files, groups int, names []string) {
	for _, f := range lib.Files {
		files++
		for _, g := range f.Groups {
			groups++
			for _, s := range g.Sources {
				names = append(names, s.Name)
			}
		}
	}
	return files, groups, names
}

// parseCase is one single-file parse: the finding that must appear and what survives.
type parseCase struct {
	name    string
	file    string   // file name, "rss.yaml" when empty
	body    string   // file content
	want    string   // substring of a finding; empty means no findings at all
	path    string   // Path of that finding, empty at file level
	files   int      // kept files
	groups  int      // kept groups
	sources int      // kept sources
	names   []string // kept source names in order, nil to skip
}

// runParseCases parses each case as one file and checks its finding and what survived.
func runParseCases(t *testing.T, cases []parseCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file := tc.file
			if file == "" {
				file = "rss.yaml"
			}
			lib, diags := parseOne(file, tc.body)
			if tc.want == "" {
				if len(diags) != 0 {
					t.Fatalf("diags = %+v, want none", diags)
				}
			} else {
				requireDiag(t, diags, file, tc.path, tc.want)
			}
			files, groups, names := shapeOf(lib)
			if files != tc.files || groups != tc.groups || len(names) != tc.sources {
				t.Fatalf("kept files %d groups %d sources %d, want %d %d %d",
					files, groups, len(names), tc.files, tc.groups, tc.sources)
			}
			if tc.names != nil && !slices.Equal(names, tc.names) {
				t.Fatalf("kept sources %q, want %q", names, tc.names)
			}
		})
	}
}

func TestParseFSValidFile(t *testing.T) {
	body := fileYAML(goodTop, groupItem("blogs", "Engineering blogs.",
		sourceItem(keyName, keyURL, keyDesc, keyLabels, "max_items: 25")))
	lib, diags := parseOne("rss.yaml", body)
	if len(diags) != 0 {
		t.Fatalf("diags = %+v, want none", diags)
	}
	if len(lib.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(lib.Files))
	}
	f := lib.Files[0]
	if f.Name != "rss.yaml" || f.Type != TypeRSS || f.SchemaVersion != SchemaVersion ||
		f.Description != "Engineering blogs." {
		t.Errorf("file = %+v, want rss.yaml of type rss with schema %d", f, SchemaVersion)
	}
	if len(f.Groups) != 1 || len(f.Groups[0].Sources) != 1 {
		t.Fatalf("groups = %+v, want one group with one source", f.Groups)
	}
	src := f.Groups[0].Sources[0]
	if !slices.Equal(src.Labels, []string{"Go", "Open Source"}) {
		t.Errorf("labels = %q, want [Go Open Source]", src.Labels)
	}
	if src.MaxItems != 25 {
		t.Errorf("MaxItems = %d, want 25", src.MaxItems)
	}
}

func TestParseFSBacklogIgnored(t *testing.T) {
	lib, diags := parseFiles(map[string]string{
		"backlog.yaml": "\t: [garbage: :\n",
		"rss.yaml":     goodRSS,
	})
	if len(diags) != 0 {
		t.Fatalf("diags = %+v, want none", diags)
	}
	if files, _, _ := shapeOf(lib); files != 1 {
		t.Fatalf("kept files = %d, want 1 (rss.yaml only)", files)
	}
}

func TestParseFSNoLibraryFiles(t *testing.T) {
	tests := []struct {
		name string
		fsys fstest.MapFS
	}{
		{name: "empty directory", fsys: fstest.MapFS{}},
		{name: "only backlog", fsys: fstest.MapFS{"backlog.yaml": &fstest.MapFile{Data: []byte("- idea\n")}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lib, diags := ParseFS(tc.fsys)
			if len(diags) != 1 || diags[0] != (Diagnostic{Message: "no library files found"}) {
				t.Fatalf("diags = %+v, want one finding with no file and the no library files message", diags)
			}
			if len(lib.Files) != 0 {
				t.Fatalf("Files = %+v, want none", lib.Files)
			}
		})
	}
}

func TestParseFSFileLevel(t *testing.T) {
	runParseCases(t, []parseCase{
		{name: "unknown stem", file: "feeds.yaml", body: goodRSS,
			want: "file name must be"},
		{name: "syntax error", body: "groups: [\n",
			want: "does not parse"},
		{name: "empty file", body: "",
			want: "file is empty"},
		{name: "whitespace only", body: "\n  \n",
			want: "file is empty"},
		{name: "second document", body: goodRSS + "---\nfoo: 1\n",
			want: "file must hold one YAML document"},
		{name: "anchor", body: fileYAML(topBlock("1", "rss", "&d Engineering blogs."), goodGroup),
			want: "anchors, aliases and merge keys are not allowed"},
		{name: "alias", body: topBlock("1", "rss", "&d Engineering blogs.") +
			"groups:\n  - name: blogs\n    description: *d\n    sources:\n" + goodSource,
			want: "anchors, aliases and merge keys are not allowed"},
		{name: "merge key", body: fileYAML(goodTop, goodGroup, "  - <<: {name: other}\n"),
			want: "anchors, aliases and merge keys are not allowed"},
		{name: "duplicate top-level key", body: fileYAML(goodTop+"type: rss\n", goodGroup),
			want: `duplicate key "type"`},
		{name: "missing schema_version", body: fileYAML("type: rss\ndescription: Engineering blogs.\n", goodGroup),
			want: `missing key "schema_version"`},
		{name: "missing type", body: fileYAML("schema_version: 1\ndescription: Engineering blogs.\n", goodGroup),
			want: `missing key "type"`},
		{name: "missing groups", body: goodTop,
			want: `missing key "groups"`},
		{name: "schema_version bool", body: fileYAML(topBlock("true", "rss", "Engineering blogs."), goodGroup),
			want: "must be the integer 1"},
		{name: "schema_version quoted", body: fileYAML(topBlock(`"1"`, "rss", "Engineering blogs."), goodGroup),
			want: "must be the integer 1"},
		{name: "schema_version zero", body: fileYAML(topBlock("0", "rss", "Engineering blogs."), goodGroup),
			want: "must be the integer 1"},
		{name: "schema_version newer", body: fileYAML(topBlock("2", "rss", "Engineering blogs."), goodGroup),
			want: "newer than supported"},
		{name: "type not the stem", body: fileYAML(topBlock("1", "academic", "Engineering blogs."), goodGroup),
			want: "must equal the file stem"},
		{name: "description blank", body: fileYAML(topBlock("1", "rss", `""`), goodGroup),
			want: "description must be a non-empty string", files: 1, groups: 1, sources: 1},
		{name: "missing description", body: fileYAML("schema_version: 1\ntype: rss\n", goodGroup),
			want: "description must be a non-empty string", files: 1, groups: 1, sources: 1},
		{name: "description is a number", body: fileYAML(topBlock("1", "rss", "42"), goodGroup),
			want: "description must be a non-empty string", files: 1, groups: 1, sources: 1},
		{name: "groups empty list", body: goodTop + "groups: []\n",
			want: "groups must be a non-empty list"},
		{name: "keys out of order", body: "schema_version: 1\ntype: rss\ngroups:\n" + goodGroup +
			"description: Engineering blogs.\n",
			want: "top-level keys must be in the order", files: 1, groups: 1, sources: 1},
		{name: "unknown top-level key", body: fileYAML(goodTop+"extra: 1\n", goodGroup),
			want: `unknown key "extra"`, files: 1, groups: 1, sources: 1},
	})

	t.Run("top level list", func(t *testing.T) {
		_, diags := parseOne("rss.yaml", "- a\n- b\n")
		requireDiag(t, diags, "rss.yaml", "", "top level must be a mapping")
	})
}

func TestParseFSSkippedFileOnlyFileLevelFindings(t *testing.T) {
	body := fileYAML(topBlock("2", "rss", "Engineering blogs."),
		"  - name: blogs\n    description: Engineering blogs.\n    colour: red\n    sources:\n"+goodSource)
	lib, diags := parseOne("rss.yaml", body)
	if len(diags) != 1 {
		t.Fatalf("diags = %+v, want exactly one", diags)
	}
	requireDiag(t, diags, "rss.yaml", "", "newer than supported")
	if files, _, _ := shapeOf(lib); files != 0 {
		t.Fatalf("kept %d files, want none", files)
	}
}

func TestParseFSGroupLevel(t *testing.T) {
	runParseCases(t, []parseCase{
		{name: "group is a scalar", body: fileYAML(goodTop, "  - just text\n"),
			want: "group must be a mapping", path: "groups[0]"},
		{name: "name uppercase", body: fileYAML(goodTop, groupItem("Blogs", "Engineering blogs.", goodSource)),
			want: "must match", path: "groups[0]"},
		{name: "name with space", body: fileYAML(goodTop, groupItem("my blogs", "Engineering blogs.", goodSource)),
			want: "must match", path: "groups[0]"},
		{name: "name is an int", body: fileYAML(goodTop, groupItem("5", "Engineering blogs.", goodSource)),
			want: "must match", path: "groups[0]"},
		{name: "duplicate key in group", body: fileYAML(goodTop,
			"  - name: blogs\n    description: Engineering blogs.\n    description: Other.\n    sources:\n"+goodSource),
			want: `duplicate key "description"`, path: "groups[0]"},
		{name: "duplicate group name",
			body: fileYAML(goodTop, goodGroup, groupItem("blogs", "Engineering blogs.", otherSource)),
			want: `duplicate group name "blogs"`, path: "blogs", files: 1, groups: 1, sources: 1,
			names: []string{"Go blog"}},
		{name: "description multi-line",
			body: fileYAML(goodTop, groupItem("blogs", "|\n      line one\n      line two", goodSource)),
			want: "description must be a non-empty single line", path: "blogs"},
		{name: "description blank", body: fileYAML(goodTop, groupItem("blogs", `""`, goodSource)),
			want: "description must be a non-empty single line", path: "blogs"},
		{name: "sources empty list",
			body: fileYAML(goodTop, "  - name: blogs\n    description: Engineering blogs.\n    sources: []\n"),
			want: "sources must be a non-empty list", path: "blogs"},
		{name: "missing name",
			body: fileYAML(goodTop, "  - description: Engineering blogs.\n    sources:\n"+goodSource),
			want: `missing key "name"`, path: "groups[0]"},
		{name: "missing description", body: fileYAML(goodTop, "  - name: blogs\n    sources:\n"+goodSource),
			want: `missing key "description"`, path: "blogs"},
		{name: "missing sources",
			body: fileYAML(goodTop, "  - name: blogs\n    description: Engineering blogs.\n"),
			want: `missing key "sources"`, path: "blogs"},
		{name: "invalid group keeps valid sibling", body: fileYAML(goodTop,
			groupItem("Blogs", "Engineering blogs.", goodSource),
			groupItem("news", "Tech news.", otherSource)),
			want: "must match", path: "groups[0]", files: 1, groups: 1, sources: 1, names: []string{"Other blog"}},
	})
}

func TestParseFSGroupWithoutSourcesDisappears(t *testing.T) {
	body := fileYAML(goodTop, groupItem("blogs", "Engineering blogs.", sourceItem(keyURL, keyDesc, keyLabels)))
	lib, diags := parseOne("rss.yaml", body)
	requireDiag(t, diags, "rss.yaml", "blogs/sources[0]", `missing required key "name"`)
	if len(diags) != 1 {
		t.Fatalf("diags = %+v, want only the source finding", diags)
	}
	if len(lib.Files) != 0 {
		t.Fatalf("Files = %+v, want the file gone", lib.Files)
	}
}

func TestParseFSSourceLevel(t *testing.T) {
	// bad builds a group whose first source has the given lines and whose second source is valid.
	bad := func(lines ...string) string {
		return fileYAML(goodTop, groupItem("blogs", "Engineering blogs.", sourceItem(lines...), goodSource))
	}
	const broken = "name: Broken blog"
	runParseCases(t, []parseCase{
		{name: "source is a scalar",
			body: fileYAML(goodTop, groupItem("blogs", "Engineering blogs.", "      - just text\n", goodSource)),
			want: "source must be a mapping", path: "blogs/sources[0]", files: 1, groups: 1, sources: 1,
			names: []string{"Go blog"}},
		{name: "missing name", body: bad(keyURL, keyDesc, keyLabels),
			want: `missing required key "name"`, path: "blogs/sources[0]", files: 1, groups: 1, sources: 1,
			names: []string{"Go blog"}},
		{name: "missing url", body: bad(broken, keyDesc, keyLabels),
			want: `missing required key "url"`, path: "blogs/Broken blog", files: 1, groups: 1, sources: 1,
			names: []string{"Go blog"}},
		{name: "missing description", body: bad(broken, keyURL, keyLabels),
			want: `missing required key "description"`, path: "blogs/Broken blog", files: 1, groups: 1, sources: 1,
			names: []string{"Go blog"}},
		{name: "missing labels", body: bad(broken, keyURL, keyDesc),
			want: `missing required key "labels"`, path: "blogs/Broken blog", files: 1, groups: 1, sources: 1,
			names: []string{"Go blog"}},
		{name: "name is an int", body: bad("name: 2024", keyURL, keyDesc, keyLabels),
			want: "name must be a string", path: "blogs/sources[0]", files: 1, groups: 1, sources: 1,
			names: []string{"Go blog"}},
		{name: "url is an int", body: bad(broken, "url: 5", keyDesc, keyLabels),
			want: "url must be a string", path: "blogs/Broken blog", files: 1, groups: 1, sources: 1,
			names: []string{"Go blog"}},
		{name: "duplicate key in source", body: bad(broken, keyURL, keyDesc, keyLabels, keyURL),
			want: `duplicate key "url"`, path: "blogs/Broken blog", files: 1, groups: 1, sources: 1,
			names: []string{"Go blog"}},
		{name: "non-scalar key in source", body: bad(broken, keyURL, keyDesc, keyLabels, "[a]: 1"),
			want: "mapping key must be a scalar", path: "blogs/Broken blog", files: 1, groups: 1, sources: 1,
			names: []string{"Go blog"}},
		{name: "name blank", body: bad(`name: ""`, keyURL, keyDesc, keyLabels),
			want: "name must be a non-empty string", path: "blogs/", files: 1, groups: 1, sources: 1,
			names: []string{"Go blog"}},
		{name: "labels scalar", body: bad(broken, keyURL, keyDesc, "labels: Go"),
			want: "labels must be a list of strings", path: "blogs/Broken blog", files: 1, groups: 1, sources: 1,
			names: []string{"Go blog"}},
		{name: "labels holds a number", body: bad(broken, keyURL, keyDesc, "labels: [Go, 2024]"),
			want: "labels must be a list of strings", path: "blogs/Broken blog", files: 1, groups: 1, sources: 1,
			names: []string{"Go blog"}},
		{name: "labels empty list", body: bad(broken, keyURL, keyDesc, "labels: []"),
			want: "labels must be a non-empty list", path: "blogs/Broken blog", files: 1, groups: 1, sources: 1,
			names: []string{"Go blog"}},
		{name: "max_items zero", body: bad(broken, keyURL, keyDesc, keyLabels, "max_items: 0"),
			want: "max_items must be a positive integer", path: "blogs/Broken blog", files: 1, groups: 1, sources: 1,
			names: []string{"Go blog"}},
		{name: "max_items negative", body: bad(broken, keyURL, keyDesc, keyLabels, "max_items: -1"),
			want: "max_items must be a positive integer", path: "blogs/Broken blog", files: 1, groups: 1, sources: 1,
			names: []string{"Go blog"}},
		{name: "max_items bool", body: bad(broken, keyURL, keyDesc, keyLabels, "max_items: true"),
			want: "max_items must be a positive integer", path: "blogs/Broken blog", files: 1, groups: 1, sources: 1,
			names: []string{"Go blog"}},
		{name: "max_items quoted", body: bad(broken, keyURL, keyDesc, keyLabels, `max_items: "5"`),
			want: "max_items must be a positive integer", path: "blogs/Broken blog", files: 1, groups: 1, sources: 1,
			names: []string{"Go blog"}},
		{name: "url not https", body: bad(broken, "url: http://go.dev/blog/feed.atom", keyDesc, keyLabels),
			want: "url must be https", path: "blogs/Broken blog", files: 1, groups: 1, sources: 1,
			names: []string{"Go blog"}},
	})
}

func TestParseFSReservedKeys(t *testing.T) {
	words := []struct{ word, hint string }{
		{"tags", "reserved for #40 item tags, use labels"},
		{"keywords", "reserved for #40 item tags"},
		{"assumptions", "reserved for #40 item tags"},
		{"aliases", "reserved for #40 item tags"},
		{"categories", "renamed to groups"},
	}
	levels := []struct {
		name string
		path string
		body func(word string) string
	}{
		{"top", "", func(w string) string {
			return fileYAML(goodTop+w+": x\n", goodGroup)
		}},
		{"group", "blogs", func(w string) string {
			return fileYAML(goodTop, "  - name: blogs\n    description: Engineering blogs.\n    "+w+
				": x\n    sources:\n"+goodSource)
		}},
		{"source", "blogs/Go blog", func(w string) string {
			return fileYAML(goodTop, groupItem("blogs", "Engineering blogs.",
				sourceItem(keyName, keyURL, keyDesc, keyLabels, w+": x")))
		}},
	}
	for _, wd := range words {
		for _, lv := range levels {
			t.Run(wd.word+" at "+lv.name, func(t *testing.T) {
				lib, diags := parseOne("rss.yaml", lv.body(wd.word))
				requireDiag(t, diags, "rss.yaml", lv.path, `unknown key "`+wd.word+`": `+wd.hint)
				files, groups, names := shapeOf(lib)
				if files != 1 || groups != 1 || !slices.Equal(names, []string{"Go blog"}) {
					t.Fatalf("kept files %d groups %d sources %q, want the file kept with its source",
						files, groups, names)
				}
			})
		}
	}
}

func TestParseFSCrossFile(t *testing.T) {
	t.Run("rss and academic kept in display order", func(t *testing.T) {
		lib, diags := parseFiles(map[string]string{"academic.yaml": academicGood, "rss.yaml": goodRSS})
		if len(diags) != 0 {
			t.Fatalf("diags = %+v, want none", diags)
		}
		if len(lib.Files) != 2 || lib.Files[0].Type != TypeRSS || lib.Files[1].Type != TypeAcademic {
			t.Fatalf("Files = %+v, want rss then academic", lib.Files)
		}
		if lib.Files[1].Name != "academic.yaml" {
			t.Errorf("second file = %q, want academic.yaml", lib.Files[1].Name)
		}
	})

	t.Run("duplicate name across files", func(t *testing.T) {
		// files parse in library order, so the rss source is the first and the academic source is dropped.
		academicDup := fileYAML(academicTop, groupItem("papers", "Agent research.",
			sourceItem("name: go BLOG", "url: https://arxiv.org/search?q=go+blog", "description: Blog papers.",
				"labels: [AI, Papers]"),
			sourceItem("name: arXiv agentic coding", "url: https://arxiv.org/search?q=agentic+coding",
				"description: "+academicDesc, "labels: [AI, Papers]")))
		lib, diags := parseFiles(map[string]string{"rss.yaml": goodRSS, "academic.yaml": academicDup})
		if len(diags) != 1 {
			t.Fatalf("diags = %+v, want one finding", diags)
		}
		requireDiag(t, diags, "academic.yaml", "papers/go BLOG", "duplicate name (also in rss.yaml)")
		files, _, names := shapeOf(lib)
		if files != 2 || lib.Files[0].Type != TypeRSS || lib.Files[1].Type != TypeAcademic {
			t.Fatalf("Files = %+v, want rss.yaml and academic.yaml", lib.Files)
		}
		if !slices.Equal(names, []string{"Go blog", "arXiv agentic coding"}) {
			t.Errorf("kept sources %q, want the rss source and the other academic source", names)
		}
	})

	t.Run("diagnostics sorted by file then line", func(t *testing.T) {
		rss := fileYAML(goodTop, groupItem("Blogs", "Engineering blogs.", goodSource)) + "extra: 1\n"
		academic := fileYAML(academicTop, groupItem("papers", "Agent research.",
			sourceItem("name: Broken paper", "url: https://example.com/search?q=x", "description: Blocked host.",
				"labels: [AI]")))
		_, diags := parseFiles(map[string]string{"rss.yaml": rss, "academic.yaml": academic})
		if len(diags) < 3 {
			t.Fatalf("diags = %+v, want at least three findings", diags)
		}
		if diags[0].File != "academic.yaml" {
			t.Errorf("first finding in %q, want academic.yaml", diags[0].File)
		}
		sorted := slices.IsSortedFunc(diags, func(a, b Diagnostic) int {
			if c := cmp.Compare(a.File, b.File); c != 0 {
				return c
			}
			return cmp.Compare(a.Line, b.Line)
		})
		if !sorted {
			t.Fatalf("diags = %+v, want sorted by file then line", diags)
		}
	})
}
