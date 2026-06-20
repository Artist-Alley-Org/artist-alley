package ai

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Tag + Caption providers are task-specialised wrappers over a
// CompletionProvider — they apply a prompt template, run Complete,
// and parse the result. Per the brief, the default implementations
// live here so every provider gets them for free; a provider with
// a smarter tagger (CLIP-similarity-based, 1.14.B) can implement
// Tag/Caption directly and override.
//
// Usage:
//   // Provider satisfies CompletionProvider; tag/caption are
//   // forwarded through the helpers.
//   func (p *Provider) Tag(ctx, asset, opts) ([]Tag, error) {
//       return ai.TagViaCompletion(ctx, p, p.prompts, asset, opts)
//   }
//   func (p *Provider) Caption(ctx, asset, opts) (string, error) {
//       return ai.CaptionViaCompletion(ctx, p, p.prompts, asset, opts)
//   }

// TagViaCompletion implements TagProvider on top of a
// CompletionProvider. Renders the tag template, runs Complete with
// a vision-capable message (image part + the rendered instructions),
// and parses the response as one tag per line.
//
// MaxTags defaults to 10 when zero; PromptVersion defaults to the
// registry's highest version for ConcernTag.
func TagViaCompletion(
	ctx context.Context,
	cp CompletionProvider,
	registry *PromptRegistry,
	asset AssetRef,
	opts TagOpts,
) ([]Tag, error) {
	if registry == nil {
		return nil, fmt.Errorf("ai: tag via completion: prompt registry required")
	}

	version := opts.PromptVersion
	if version == "" {
		v, ok := registry.DefaultVersion(ConcernTag)
		if !ok {
			return nil, fmt.Errorf("ai: no tag prompt template registered")
		}
		version = v
	}
	tpl, ok := registry.Lookup(ConcernTag, version)
	if !ok {
		return nil, fmt.Errorf("ai: tag template %q not found", version)
	}

	maxTags := opts.MaxTags
	if maxTags <= 0 {
		maxTags = 10
	}

	body := tpl.Render(map[string]string{
		"max_tags": strconv.Itoa(maxTags),
	})

	req := CompletionRequest{
		Messages: []Message{
			{
				Role: "user",
				Content: []Content{
					{Type: ContentTypeText, Text: body},
					assetImageContent(asset),
				},
			},
		},
		PromptVersion: version,
		AssetID:       &asset.ID,
	}
	resp, err := cp.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	return parseTagLines(resp.Text, maxTags), nil
}

// CaptionViaCompletion implements CaptionProvider on top of a
// CompletionProvider. Same prompt-render-then-Complete pattern as
// the tag helper.
//
// MaxLength defaults to 200 when zero; StyleHint defaults to
// "descriptive"; PromptVersion defaults to the registry's highest
// version for ConcernCaption.
func CaptionViaCompletion(
	ctx context.Context,
	cp CompletionProvider,
	registry *PromptRegistry,
	asset AssetRef,
	opts CaptionOpts,
) (string, error) {
	if registry == nil {
		return "", fmt.Errorf("ai: caption via completion: prompt registry required")
	}

	version := opts.PromptVersion
	if version == "" {
		v, ok := registry.DefaultVersion(ConcernCaption)
		if !ok {
			return "", fmt.Errorf("ai: no caption prompt template registered")
		}
		version = v
	}
	tpl, ok := registry.Lookup(ConcernCaption, version)
	if !ok {
		return "", fmt.Errorf("ai: caption template %q not found", version)
	}

	maxLength := opts.MaxLength
	if maxLength <= 0 {
		maxLength = 200
	}
	styleHint := opts.StyleHint
	if styleHint == "" {
		styleHint = "descriptive"
	}

	body := tpl.Render(map[string]string{
		"max_length": strconv.Itoa(maxLength),
		"style_hint": styleHint,
	})

	req := CompletionRequest{
		Messages: []Message{
			{
				Role: "user",
				Content: []Content{
					{Type: ContentTypeText, Text: body},
					assetImageContent(asset),
				},
			},
		},
		PromptVersion: version,
		AssetID:       &asset.ID,
	}
	resp, err := cp.Complete(ctx, req)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Text), nil
}

// assetImageContent prefers the OriginalURL when it's a public
// URL the provider can fetch; falls back to PreviewURL when the
// original is large. Both go through as image_url; b64 inline
// would balloon the wire payload for assets that already live in
// object storage.
//
// When neither URL is set the message degrades to text-only — the
// provider can't see the image but the prompt still goes through,
// which is the right behaviour for tests + non-vision providers.
func assetImageContent(asset AssetRef) Content {
	url := asset.OriginalURL
	if url == "" {
		url = asset.PreviewURL
	}
	if url == "" {
		return Content{Type: ContentTypeText, Text: ""}
	}
	return Content{Type: ContentTypeImageURL, ImageURL: url}
}

// parseTagLines turns a free-text model response into a Tag slice.
// Splits on newlines, trims punctuation + whitespace, drops empty
// lines, lowercases, dedupes, and caps at maxTags.
//
// The default tag prompt asks for "one per line, lowercase, no
// punctuation"; this parser tolerates models that ignore some of
// those instructions (bullet markers, numbered lists, trailing
// commas).
func parseTagLines(text string, maxTags int) []Tag {
	if text == "" {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]Tag, 0, maxTags)
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		// Strip common list markers.
		t = strings.TrimLeft(t, "-•*0123456789.) ")
		// Trim trailing punctuation.
		t = strings.TrimRight(t, ".,;:")
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, Tag{Term: t})
		if len(out) >= maxTags {
			break
		}
	}
	return out
}
