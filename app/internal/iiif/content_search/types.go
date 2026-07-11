// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package content_search

// Context is the JSON-LD context URI for Content Search 2.0. Emitted
// as the top-level @context on every AnnotationPage response.
const Context = "http://iiif.io/api/search/2/context.json"

// LangString is a language-tagged value. Duplicated locally (rather
// than imported from presentation) so the content_search package
// doesn't take a compile-time dependency on presentation types —
// the two are wired independently at the http layer.
type LangString map[string][]string

func en(v string) LangString {
	if v == "" {
		return LangString{"en": {""}}
	}
	return LangString{"en": {v}}
}

// AnnotationPage is the top-level shape returned to viewers.
type AnnotationPage struct {
	Context string       `json:"@context"`
	ID      string       `json:"id"`
	Type    string       `json:"type"`
	Items   []Annotation `json:"items"`
	// Ignored is the array of spec-defined query parameters the
	// endpoint declined to honour. Emitted per spec so viewers can
	// surface a "your filter was ignored" UI hint. MVP always
	// includes any filter that wasn't `q`.
	Ignored []string `json:"ignored,omitempty"`
	// Partof surfaces the parent search-service URL so viewers can
	// re-run with additional params. Present when the response is
	// non-empty; omitted for empty result pages to keep the payload
	// terse.
	PartOf []PartOfRef `json:"partOf,omitempty"`
}

// Annotation is one match. Motivation is fixed to "supplementing"
// per the spec's guidance for text-search hits — "commenting" is
// reserved for user-added notes.
type Annotation struct {
	ID         string      `json:"id"`
	Type       string      `json:"type"`
	Motivation string      `json:"motivation"`
	Body       TextualBody `json:"body"`
	Target     string      `json:"target"`
}

// TextualBody is the text-content variant of an Annotation body.
// Format is always text/plain here — we don't emit rich text.
// Value carries the matched text; Language tag defaults to "en".
type TextualBody struct {
	Type     string `json:"type"`
	Value    string `json:"value"`
	Format   string `json:"format,omitempty"`
	Language string `json:"language,omitempty"`
	// Granularity is the IIIF Content Search 2.0 optional field
	// declaring how the body is granulated. "line" for asset-scope
	// hits (one metadata pair per line); "manifest" for collection-
	// scope hits (one member manifest per annotation).
	Granularity string `json:"textGranularity,omitempty"`
}

// PartOfRef points at the parent search service so viewers can
// reconstruct the query.
type PartOfRef struct {
	ID    string     `json:"id"`
	Type  string     `json:"type"`
	Label LangString `json:"label,omitempty"`
}
