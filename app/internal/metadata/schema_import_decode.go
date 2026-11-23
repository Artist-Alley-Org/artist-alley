// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// ---------------------------------------------------------------------------
// Strict decoding of a schema-import request (#1576, ADR 0083)
// ---------------------------------------------------------------------------
//
// The generated decoder cannot be used for an import. encoding/json
// matches object keys without regard to case, lets a repeated key
// overwrite the first, ignores unknown members, and turns a missing
// boolean into false. Each of those loses exactly what an import has to
// refuse. So the request is first parsed into an ordered tree that keeps
// every key as written, and every rule is checked on that tree. The
// typed document is built from the tree directly; nothing is decoded
// generically.

// importMaxNesting bounds JSON nesting while parsing. A valid document
// needs about 2 x 64 + 8 levels; anything far deeper is refused before
// it is walked.
const importMaxNesting = 256

// importMaxProblems bounds how many problems one refusal reports.
const importMaxProblems = 100

type jsonKind int

const (
	jsonObject jsonKind = iota
	jsonArray
	jsonString
	jsonNumber
	jsonBool
	jsonNull
)

type jsonMember struct {
	key string
	val *jsonNode
}

type jsonNode struct {
	kind    jsonKind
	members []jsonMember
	items   []*jsonNode
	str     string
	num     string
	boolean bool
}

func (n *jsonNode) kindName() string {
	switch n.kind {
	case jsonObject:
		return "an object"
	case jsonArray:
		return "an array"
	case jsonString:
		return "a string"
	case jsonNumber:
		return "a number"
	case jsonBool:
		return "a boolean"
	}
	return "null"
}

// ImportProblem is one rule a request breaks, with its JSON path.
type ImportProblem struct {
	Path    string
	Problem string
}

// strictParseError is a problem found while parsing. It carries a path
// when the parser knew where it was.
type strictParseError struct {
	path string
	msg  string
	// syntax marks malformed JSON (or bytes that cannot be decoded
	// faithfully), which is a 400 wherever it occurs. A strict rule
	// broken inside well-formed JSON (a repeated key, extreme nesting)
	// is not syntax, and inside the document it is a 422.
	syntax bool
	// inDocument is structural: the error lies inside the value of the
	// top-level member named exactly "document". It is decided by the
	// parser's position, never by matching path text, so an envelope
	// key such as "document.x" or "document[" is not mistaken for it.
	inDocument bool
}

func (e *strictParseError) Error() string { return e.path + ": " + e.msg }

// parseStrictJSON parses b into an ordered tree, refusing invalid
// UTF-8, an escape that would decode to a replacement character (a lone
// surrogate), a repeated key, nesting past importMaxNesting, and
// anything after the first value.
func parseStrictJSON(b []byte) (*jsonNode, error) {
	if !utf8.Valid(b) {
		return nil, &strictParseError{path: "$", msg: "the body is not valid UTF-8", syntax: true}
	}
	if err := checkSurrogateEscapes(b); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	root, err := readStrictValue(dec, "$", 0, false)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, &strictParseError{path: "$", msg: "there is content after the JSON value", syntax: true}
	}
	return root, nil
}

func readStrictValue(dec *json.Decoder, path string, depth int, inDoc bool) (*jsonNode, error) {
	if depth > importMaxNesting {
		return nil, &strictParseError{path: path, msg: fmt.Sprintf("nests deeper than %d levels", importMaxNesting), inDocument: inDoc}
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, &strictParseError{path: path, msg: "not valid JSON: " + err.Error(), syntax: true, inDocument: inDoc}
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			n := &jsonNode{kind: jsonObject}
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, &strictParseError{path: path, msg: "not valid JSON: " + err.Error(), syntax: true, inDocument: inDoc}
				}
				key, ok := kt.(string)
				if !ok {
					return nil, &strictParseError{path: path, msg: "not valid JSON: object key is not a string", syntax: true, inDocument: inDoc}
				}
				if seen[key] {
					return nil, &strictParseError{path: path, msg: fmt.Sprintf("member %q appears more than once", key), inDocument: inDoc}
				}
				seen[key] = true
				childInDoc := inDoc || (depth == 0 && key == "document")
				v, err := readStrictValue(dec, path+"."+key, depth+1, childInDoc)
				if err != nil {
					return nil, err
				}
				n.members = append(n.members, jsonMember{key: key, val: v})
			}
			if _, err := dec.Token(); err != nil {
				return nil, &strictParseError{path: path, msg: "not valid JSON: " + err.Error(), syntax: true, inDocument: inDoc}
			}
			return n, nil
		case '[':
			n := &jsonNode{kind: jsonArray}
			for i := 0; dec.More(); i++ {
				v, err := readStrictValue(dec, fmt.Sprintf("%s[%d]", path, i), depth+1, inDoc)
				if err != nil {
					return nil, err
				}
				n.items = append(n.items, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, &strictParseError{path: path, msg: "not valid JSON: " + err.Error(), syntax: true, inDocument: inDoc}
			}
			return n, nil
		}
		return nil, &strictParseError{path: path, msg: "not valid JSON", syntax: true, inDocument: inDoc}
	case string:
		return &jsonNode{kind: jsonString, str: t}, nil
	case json.Number:
		return &jsonNode{kind: jsonNumber, num: t.String()}, nil
	case bool:
		return &jsonNode{kind: jsonBool, boolean: t}, nil
	case nil:
		return &jsonNode{kind: jsonNull}, nil
	}
	return nil, &strictParseError{path: path, msg: "not valid JSON", syntax: true, inDocument: inDoc}
}

// checkSurrogateEscapes refuses a \u escape naming a lone UTF-16
// surrogate. encoding/json decodes one to U+FFFD silently, which would
// store something the sender never wrote.
func checkSurrogateEscapes(b []byte) error {
	inString := false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			continue
		}
		switch c {
		case '"':
			inString = false
		case '\\':
			if i+1 >= len(b) {
				return nil // the parser reports the truncation
			}
			if b[i+1] != 'u' {
				i++
				continue
			}
			r, ok := hex4(b, i+2)
			if !ok {
				return nil // the parser reports the bad escape
			}
			switch {
			case r >= 0xD800 && r <= 0xDBFF:
				if i+11 < len(b) && b[i+6] == '\\' && b[i+7] == 'u' {
					if lo, ok := hex4(b, i+8); ok && lo >= 0xDC00 && lo <= 0xDFFF {
						i += 11
						continue
					}
				}
				return &strictParseError{path: "$", msg: fmt.Sprintf("byte %d: a \\u escape names an unpaired surrogate", i), syntax: true}
			case r >= 0xDC00 && r <= 0xDFFF:
				return &strictParseError{path: "$", msg: fmt.Sprintf("byte %d: a \\u escape names an unpaired surrogate", i), syntax: true}
			}
			i += 5
		}
	}
	return nil
}

func hex4(b []byte, at int) (rune, bool) {
	if at+4 > len(b) {
		return 0, false
	}
	v, err := strconv.ParseUint(string(b[at:at+4]), 16, 32)
	if err != nil {
		return 0, false
	}
	return rune(v), true
}

// ---------------------------------------------------------------------------
// The envelope
// ---------------------------------------------------------------------------

// ImportSource is the operator's untrusted claim about where a document
// came from. Either member may be nil.
type ImportSource struct {
	Label *string
	URI   *string
}

const (
	importSourceLabelMaxRunes = 200
	importSourceURIMaxBytes   = 2048
)

// decodePreviewEnvelope checks the preview request envelope and returns
// the document node. Problems here are 400s: the request, not the
// document, is wrong.
func decodePreviewEnvelope(root *jsonNode) (*jsonNode, ImportSource, []ImportProblem) {
	var src ImportSource
	if root.kind != jsonObject {
		return nil, src, []ImportProblem{{"$", "must be an object"}}
	}
	o := newObjectReader(root, "$")
	doc := o.required("document")
	if s := o.optional("source"); s != nil {
		src = decodeSource(s, "$.source", o)
	}
	o.noOthers()
	return doc, src, o.problems
}

func decodeSource(n *jsonNode, path string, parent *objectReader) ImportSource {
	var src ImportSource
	if n.kind != jsonObject {
		parent.add(path, "must be an object")
		return src
	}
	o := newObjectReader(n, path)
	if l := o.optional("label"); l != nil {
		if l.kind != jsonString || l.str == "" {
			o.add(path+".label", "must be a non-empty string")
		} else if hasNUL(l.str) {
			o.add(path+".label", nulProblem)
		} else if utf8.RuneCountInString(l.str) > importSourceLabelMaxRunes {
			o.add(path+".label", fmt.Sprintf("must be at most %d characters", importSourceLabelMaxRunes))
		} else {
			v := l.str
			src.Label = &v
		}
	}
	if u := o.optional("uri"); u != nil {
		if u.kind != jsonString || u.str == "" {
			o.add(path+".uri", "must be a non-empty string")
		} else if len(u.str) > importSourceURIMaxBytes {
			o.add(path+".uri", fmt.Sprintf("must be at most %d bytes", importSourceURIMaxBytes))
		} else if pu, err := url.Parse(u.str); err != nil || !pu.IsAbs() || pu.Host == "" ||
			(pu.Scheme != "http" && pu.Scheme != "https") {
			o.add(path+".uri", "must be an absolute http or https URI")
		} else {
			v := u.str
			src.URI = &v
		}
	}
	o.noOthers()
	parent.problems = mergeProblems(parent.problems, o.problems)
	return src
}

// objectReader takes members off an object by exact name and reports the
// rest as unknown. A key differing only in case is unknown, because the
// comparison is exact.
type objectReader struct {
	n        *jsonNode
	path     string
	used     map[string]bool
	problems []ImportProblem
}

func newObjectReader(n *jsonNode, path string) *objectReader {
	return &objectReader{n: n, path: path, used: map[string]bool{}}
}

func (o *objectReader) add(path, problem string) {
	o.problems = appendProblem(o.problems, path, problem)
}

// appendProblem adds a problem unless importMaxProblems are already
// held. Once the cap is reached the list stays at the cap, so a refusal
// is still a refusal, while the work spent on details stays bounded.
func appendProblem(list []ImportProblem, path, problem string) []ImportProblem {
	if len(list) >= importMaxProblems {
		return list
	}
	return append(list, ImportProblem{Path: path, Problem: problem})
}

// mergeProblems appends src to dst under the same cap.
func mergeProblems(dst, src []ImportProblem) []ImportProblem {
	for _, p := range src {
		if len(dst) >= importMaxProblems {
			break
		}
		dst = append(dst, p)
	}
	return dst
}

// hasNUL reports a U+0000, which PostgreSQL TEXT and JSONB cannot hold.
// A value that would fail to store is refused, never stripped.
func hasNUL(s string) bool { return strings.IndexByte(s, 0) >= 0 }

const nulProblem = "must not contain U+0000, which this server cannot store"

func (o *objectReader) get(key string) *jsonNode {
	for _, m := range o.n.members {
		if m.key == key {
			o.used[key] = true
			return m.val
		}
	}
	return nil
}

func (o *objectReader) required(key string) *jsonNode {
	v := o.get(key)
	if v == nil {
		o.add(o.path+"."+key, "is required")
	}
	return v
}

func (o *objectReader) optional(key string) *jsonNode { return o.get(key) }

func (o *objectReader) noOthers() {
	for _, m := range o.n.members {
		if !o.used[m.key] {
			o.add(o.path+"."+m.key, "is not a member of this object")
		}
	}
}

// ---------------------------------------------------------------------------
// The document
// ---------------------------------------------------------------------------

// errUnsupportedFormat marks a document whose `format` is a string other
// than the one this server implements.
var errUnsupportedFormat = errors.New("unsupported format")

var integerToken = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

// decodeImportDocument checks a document node against the
// `aa-field-schema/1` contract and builds the typed document from it.
// It returns errUnsupportedFormat when `format` names another format,
// and the problems otherwise.
func decodeImportDocument(n *jsonNode) (openapi.FieldSchemaExport, []ImportProblem, error) {
	var doc openapi.FieldSchemaExport
	const path = "$.document"
	if n.kind != jsonObject {
		return doc, []ImportProblem{{path, "must be an object"}}, nil
	}
	o := newObjectReader(n, path)
	f := o.required("format")
	fields := o.required("fields")
	o.noOthers()
	if f != nil {
		if f.kind != jsonString {
			o.add(path+".format", "must be a string")
		} else if f.str != string(FieldSchemaExportFormat) {
			return doc, nil, errUnsupportedFormat
		}
	}
	doc.Format = FieldSchemaExportFormat
	if fields == nil {
		return doc, o.problems, nil
	}
	if fields.kind != jsonArray {
		o.add(path+".fields", "must be an array")
		return doc, o.problems, nil
	}
	if len(fields.items) < 1 || len(fields.items) > FieldSchemaExportMaxCodes {
		// Refused on the count alone: the list is not walked, so an
		// enormous list costs no per-element work.
		o.add(path+".fields", fmt.Sprintf("must hold 1 to %d fields, not %d", FieldSchemaExportMaxCodes, len(fields.items)))
		return doc, o.problems, nil
	}
	w := &docWalker{problems: o.problems}
	prev := ""
	doc.Fields = make([]openapi.FieldSchemaExportField, 0, len(fields.items))
	for i, item := range fields.items {
		if len(w.problems) >= importMaxProblems || w.nodes > FieldSchemaMaxOptionNodes {
			// Already refused, and the detail list is full or the node
			// bound is broken: walking further adds nothing.
			break
		}
		fp := fmt.Sprintf("%s.fields[%d]", path, i)
		fd, ok := w.field(item, fp)
		if ok {
			if prev != "" && fd.Code <= prev {
				w.add(fp+".code", fmt.Sprintf("fields must be strictly ascending by code; %q follows %q", fd.Code, prev))
			}
			prev = fd.Code
		}
		doc.Fields = append(doc.Fields, fd)
	}
	if w.nodes > FieldSchemaMaxOptionNodes {
		w.add(path+".fields", fmt.Sprintf("holds more than %d option terms, the limit", FieldSchemaMaxOptionNodes))
	}
	return doc, w.problems, nil
}

type docWalker struct {
	problems []ImportProblem
	nodes    int
}

func (w *docWalker) add(path, problem string) {
	w.problems = appendProblem(w.problems, path, problem)
}

// text reads a stored portable string: a string, with no U+0000.
func (w *docWalker) text(n *jsonNode, path string) (string, bool) {
	s, ok := w.str(n, path)
	if ok && hasNUL(s) {
		w.add(path, nulProblem)
		return s, false
	}
	return s, ok
}

func (w *docWalker) str(n *jsonNode, path string) (string, bool) {
	if n == nil {
		return "", false
	}
	if n.kind != jsonString {
		w.add(path, "must be a string, not "+n.kindName())
		return "", false
	}
	return n.str, true
}

func (w *docWalker) boolean(n *jsonNode, path string) bool {
	if n == nil {
		return false
	}
	if n.kind != jsonBool {
		w.add(path, "must be a boolean, not "+n.kindName())
		return false
	}
	return n.boolean
}

// optString reads a member that is a string or null.
func (w *docWalker) optString(n *jsonNode, path string) (*string, bool) {
	if n == nil {
		return nil, false
	}
	if n.kind == jsonNull {
		return nil, true
	}
	if n.kind != jsonString {
		w.add(path, "must be a string or null, not "+n.kindName())
		return nil, false
	}
	v := n.str
	return &v, true
}

// field checks one portable field. All 17 members are required; a
// member's absence is a problem even when its value would be false or
// null. The bool reports whether the code itself was readable.
func (w *docWalker) field(n *jsonNode, path string) (openapi.FieldSchemaExportField, bool) {
	var f openapi.FieldSchemaExportField
	if n.kind != jsonObject {
		w.add(path, "must be an object")
		return f, false
	}
	o := newObjectReader(n, path)
	m := map[string]*jsonNode{}
	for _, k := range []string{
		"code", "label", "description", "type", "subject_kind", "required", "searchable",
		"display_group", "display_order", "options", "regexp_filter", "display_condition",
		"show_on_card", "show_in_advanced_search", "show_on_upload", "edit_tab", "open_vocabulary",
	} {
		m[k] = o.required(k)
	}
	o.noOthers()
	w.problems = mergeProblems(w.problems, o.problems)

	codeOK := false
	if c, ok := w.str(m["code"], path+".code"); ok {
		f.Code = c
		if !codePattern.MatchString(c) {
			w.add(path+".code", "must match ^[a-z][a-z0-9_]*$")
		} else {
			codeOK = true
		}
	}
	if l, ok := w.text(m["label"], path+".label"); ok {
		f.Label = l
		// The ordinary create path trims a label and refuses a blank
		// one. An import refuses what that path would change, rather
		// than changing it.
		if l == "" || strings.TrimSpace(l) != l {
			w.add(path+".label", "must be non-empty with no leading or trailing whitespace")
		}
	}
	if d, ok := w.text(m["description"], path+".description"); ok {
		f.Description = d
	}
	typeOK := false
	if t, ok := w.str(m["type"], path+".type"); ok {
		f.Type = t
		if !validFieldType(t) {
			w.add(path+".type", fmt.Sprintf("%q is not a field type", t))
		} else {
			typeOK = true
		}
	}
	if s, ok := w.str(m["subject_kind"], path+".subject_kind"); ok {
		f.SubjectKind = openapi.FieldSchemaExportFieldSubjectKind(s)
		if _, err := ParseSubjectKind(s); err != nil {
			w.add(path+".subject_kind", "must be asset or collection")
		}
	}
	f.Required = w.boolean(m["required"], path+".required")
	f.Searchable = w.boolean(m["searchable"], path+".searchable")
	f.ShowOnCard = w.boolean(m["show_on_card"], path+".show_on_card")
	f.ShowInAdvancedSearch = w.boolean(m["show_in_advanced_search"], path+".show_in_advanced_search")
	f.ShowOnUpload = w.boolean(m["show_on_upload"], path+".show_on_upload")
	f.OpenVocabulary = w.boolean(m["open_vocabulary"], path+".open_vocabulary")
	// display_group follows the ordinary contract: any string.
	if g, ok := w.text(m["display_group"], path+".display_group"); ok {
		f.DisplayGroup = g
	}
	if d := m["display_order"]; d != nil {
		if d.kind != jsonNumber || !integerToken.MatchString(d.num) {
			w.add(path+".display_order", "must be an integer")
		} else if v, err := strconv.ParseInt(d.num, 10, 32); err != nil {
			w.add(path+".display_order", "must fit in 32 bits")
		} else {
			f.DisplayOrder = int(v)
		}
	}

	if ef, ok := w.optString(m["edit_tab"], path+".edit_tab"); ok {
		f.EditTab = ef
		if ef != nil && hasNUL(*ef) {
			w.add(path+".edit_tab", nulProblem)
		} else if ef != nil && (strings.TrimSpace(*ef) == "" || strings.TrimSpace(*ef) != *ef) {
			w.add(path+".edit_tab", "must be null or non-blank with no leading or trailing whitespace")
		}
	}
	if rf, ok := w.optString(m["regexp_filter"], path+".regexp_filter"); ok {
		f.RegexpFilter = rf
		if rf != nil && hasNUL(*rf) {
			w.add(path+".regexp_filter", nulProblem)
		} else if rf != nil && typeOK {
			if *rf == "" {
				w.add(path+".regexp_filter", "must be null or a non-empty pattern")
			} else if msg := validateRegexpFilterConfig(FieldDefinition{Code: f.Code, Type: f.Type}, *rf); msg != "" {
				w.add(path+".regexp_filter", msg)
			}
		}
	}
	if dc := m["display_condition"]; dc != nil {
		switch dc.kind {
		case jsonNull:
		case jsonArray:
			if len(dc.items) == 0 {
				w.add(path+".display_condition", "must be null or a non-empty array")
			}
			terms := make([]string, 0, len(dc.items))
			for i, it := range dc.items {
				tp := fmt.Sprintf("%s.display_condition[%d]", path, i)
				s, ok := w.text(it, tp)
				terms = append(terms, s)
				if !ok {
					continue
				}
				if strings.TrimSpace(s) == "" {
					w.add(tp, "must not be blank")
					continue
				}
				t, ok := ParseDisplayConditionTerm(s)
				if !ok {
					w.add(tp, "is not a valid <code><operator><value> term; the operators are = and ~")
					continue
				}
				if codeOK && t.Code == f.Code {
					w.add(tp, "a field cannot depend on itself")
				}
			}
			if len(terms) > 0 {
				f.DisplayCondition = &terms
			}
		default:
			w.add(path+".display_condition", "must be null or an array, not "+dc.kindName())
		}
	}

	if on := m["options"]; on != nil && typeOK {
		vocab := hasVocabulary(f.Type)
		switch {
		case on.kind == jsonNull && !vocab:
		case on.kind == jsonNull && vocab:
			w.add(path+".options", "must be an array for a "+f.Type+" field")
		case on.kind == jsonArray && !vocab:
			w.add(path+".options", "must be null for a "+f.Type+" field")
		case on.kind == jsonArray:
			slugs := map[string]bool{}
			opts := w.options(on, path+".options", 1, slugs)
			f.Options = &opts
		default:
			w.add(path+".options", "must be an array or null, not "+on.kindName())
		}
	}
	return f, codeOK
}

func (w *docWalker) options(n *jsonNode, path string, depth int, slugs map[string]bool) []openapi.FieldSchemaExportOption {
	out := make([]openapi.FieldSchemaExportOption, 0, len(n.items))
	if depth > FieldSchemaMaxOptionDepth && len(n.items) > 0 {
		w.add(path, fmt.Sprintf("option tree is deeper than %d levels", FieldSchemaMaxOptionDepth))
		return out
	}
	for i, it := range n.items {
		p := fmt.Sprintf("%s[%d]", path, i)
		w.nodes++
		if w.nodes > FieldSchemaMaxOptionNodes || len(w.problems) >= importMaxProblems {
			// The node bound is broken (reported once, by the caller) or
			// the details are full: stop walking.
			return out
		}
		if it.kind != jsonObject {
			w.add(p, "must be an object")
			continue
		}
		o := newObjectReader(it, p)
		v, l, c := o.required("value"), o.required("label"), o.required("children")
		o.noOthers()
		w.problems = mergeProblems(w.problems, o.problems)
		var opt openapi.FieldSchemaExportOption
		if s, ok := w.text(v, p+".value"); ok {
			opt.Value = s
			switch {
			case s == "" || strings.TrimSpace(s) != s:
				w.add(p+".value", "must be non-empty with no leading or trailing whitespace")
			case slugs[s]:
				w.add(p+".value", fmt.Sprintf("%q appears more than once in this field's options", s))
			default:
				slugs[s] = true
			}
		}
		if ls, ok := w.optString(l, p+".label"); ok {
			opt.Label = ls
			if ls != nil && hasNUL(*ls) {
				w.add(p+".label", nulProblem)
			} else if ls != nil && (*ls == "" || strings.TrimSpace(*ls) != *ls) {
				w.add(p+".label", "must be null or non-empty with no leading or trailing whitespace")
			}
		}
		opt.Children = []openapi.FieldSchemaExportOption{}
		if c != nil {
			if c.kind != jsonArray {
				w.add(p+".children", "must be an array")
			} else {
				opt.Children = w.options(c, p+".children", depth+1, slugs)
			}
		}
		out = append(out, opt)
	}
	return out
}

// canonicalImportDocument encodes doc exactly as the exporter does
// (json.Encoder, generated member order, trailing newline) and returns
// the bytes and their "sha256:" digest.
func canonicalImportDocument(doc openapi.FieldSchemaExport) ([]byte, string, error) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(doc); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), sha256Hex(buf.Bytes()), nil
}
