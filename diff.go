package main

import (
	"regexp"
	"strings"
	"sync"
)

// prevCache holds the last-seen markdown content per session so we can
// compute a line-diff on the next update. In-memory only, per-process.
type prevCache struct {
	mu   sync.Mutex
	data map[string]string
}

func newPrevCache() *prevCache {
	return &prevCache{data: map[string]string{}}
}

func (p *prevCache) get(id string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.data[id]
	return v, ok
}

func (p *prevCache) set(id, content string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.data[id] = content
}

func (p *prevCache) forget(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.data, id)
}

// newLines returns the set of non-empty lines in curr that are not present
// in prev (exact match after trimming trailing whitespace). Order-agnostic.
func newLines(prev, curr string) map[string]struct{} {
	prevSet := map[string]struct{}{}
	for _, raw := range strings.Split(prev, "\n") {
		line := strings.TrimRight(raw, " \t\r")
		if line == "" {
			continue
		}
		prevSet[line] = struct{}{}
	}
	out := map[string]struct{}{}
	for _, raw := range strings.Split(curr, "\n") {
		line := strings.TrimRight(raw, " \t\r")
		if line == "" {
			continue
		}
		if _, ok := prevSet[line]; ok {
			continue
		}
		out[line] = struct{}{}
	}
	return out
}

// stripMD reduces a markdown line to its visible text so it can be matched
// against the text extracted from a rendered HTML block. Handles the common
// leaf markers: list bullets, task checkboxes, heading hashes, blockquote
// prefix, emphasis, inline code, and links.
var (
	mdBulletRe   = regexp.MustCompile(`^\s*([-*+]|\d+\.)\s+`)
	mdTaskRe     = regexp.MustCompile(`^\s*\[[ xX]\]\s+`)
	mdHeadRe     = regexp.MustCompile(`^\s*#{1,6}\s+`)
	mdQuoteRe    = regexp.MustCompile(`^\s*>\s?`)
	mdLinkRe     = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
	mdEmphRe     = regexp.MustCompile("[*_`~]+")
	mdTableRowRe = regexp.MustCompile(`^\s*\|(.*)\|\s*$`)
	mdTableSepRe = regexp.MustCompile(`^\s*\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)+\|?\s*$`)
	wsCollapse   = regexp.MustCompile(`\s+`)
)

func stripMD(line string) string {
	s := line
	// Table rows: `| a | b | c |` → `a b c` so they match the rendered
	// <tr>'s plain text. Separator rows like `|---|---|` reduce to empty
	// via the same path and naturally fail the whole-block match later.
	if mdTableSepRe.MatchString(s) {
		return ""
	}
	if m := mdTableRowRe.FindStringSubmatch(s); len(m) == 2 {
		parts := strings.Split(m[1], "|")
		cells := make([]string, 0, len(parts))
		for _, p := range parts {
			cells = append(cells, strings.TrimSpace(p))
		}
		s = strings.Join(cells, " ")
	}
	s = mdHeadRe.ReplaceAllString(s, "")
	s = mdQuoteRe.ReplaceAllString(s, "")
	s = mdBulletRe.ReplaceAllString(s, "")
	s = mdTaskRe.ReplaceAllString(s, "")
	s = mdLinkRe.ReplaceAllString(s, "$1")
	s = mdEmphRe.ReplaceAllString(s, "")
	s = wsCollapse.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// Precomputed regexes for HTML post-processing.
var (
	blockOpenRe = regexp.MustCompile(`<(p|li|h[1-6]|pre|blockquote|tr)(\s[^>]*)?>`)
	htmlTagRe   = regexp.MustCompile(`<[^>]+>`)
)

// markHTML scans htmlSrc for top-level block elements (<p>, <li>, <h1-6>,
// <pre>, <blockquote>) and adds class="md-new" when the block's plain-text
// content matches a line from the newLines set. Only whole-block matches
// count; substring hits are rejected to avoid false positives.
//
// "Top-level" here means: we don't descend into a block to mark nested
// children — when the outer <li> matches, it wins. We still skip an <li>
// that directly nests another <li> (rare with goldmark) by recursing.
func markHTML(htmlSrc string, newSet map[string]struct{}) string {
	if len(newSet) == 0 {
		return htmlSrc
	}
	// Normalise the target set to the same "stripped" form we'll compare
	// against. Keep originals for the tooltip.
	stripped := make(map[string]string, len(newSet)) // stripped -> original
	for line := range newSet {
		key := stripMD(line)
		if key == "" {
			continue
		}
		stripped[key] = line
	}
	if len(stripped) == 0 {
		return htmlSrc
	}

	var out strings.Builder
	out.Grow(len(htmlSrc) + 64)
	i := 0
	for i < len(htmlSrc) {
		loc := blockOpenRe.FindStringSubmatchIndex(htmlSrc[i:])
		if loc == nil {
			out.WriteString(htmlSrc[i:])
			break
		}
		// loc is relative to htmlSrc[i:]
		openStart := i + loc[0]
		openEnd := i + loc[1]
		tag := htmlSrc[i+loc[2] : i+loc[3]]

		// Write everything before the opening tag.
		out.WriteString(htmlSrc[i:openStart])

		// Find the matching close tag, accounting for nested same-tag pairs.
		closeTag := "</" + tag + ">"
		openPat := "<" + tag
		depth := 1
		j := openEnd
		for j < len(htmlSrc) {
			nextClose := strings.Index(htmlSrc[j:], closeTag)
			if nextClose < 0 {
				break
			}
			// Count any nested opens strictly between j and j+nextClose.
			segment := htmlSrc[j : j+nextClose]
			nestedOpens := 0
			k := 0
			for k < len(segment) {
				o := strings.Index(segment[k:], openPat)
				if o < 0 {
					break
				}
				// Must be followed by space, `>` or `/` to be a real tag.
				end := k + o + len(openPat)
				if end < len(segment) {
					c := segment[end]
					if c == ' ' || c == '>' || c == '\t' || c == '\n' || c == '/' {
						nestedOpens++
					}
				}
				k = end
			}
			depth += nestedOpens
			depth--
			j = j + nextClose + len(closeTag)
			if depth <= 0 {
				break
			}
		}
		// If we couldn't find a close, bail out — write rest and stop.
		if depth > 0 {
			out.WriteString(htmlSrc[openStart:])
			break
		}

		blockEnd := j
		block := htmlSrc[openStart:blockEnd]

		// Extract inner text. Keep newlines for the per-line fallback below.
		inner := htmlSrc[openEnd:(blockEnd - len(closeTag))]
		innerText := htmlTagRe.ReplaceAllString(inner, " ")
		plain := wsCollapse.ReplaceAllString(innerText, " ")
		plain = strings.TrimSpace(plain)

		matched := ""
		if plain != "" {
			// Fast path: whole-block match.
			if orig, ok := stripped[plain]; ok {
				matched = orig
			} else {
				// Fallback: line-by-line. Handles lazy-continuation cases
				// where a new source line got merged into an otherwise-
				// unchanged block (e.g. a bare line appended to a list with
				// no blank line — goldmark folds it into the last <li>).
				for _, part := range strings.Split(innerText, "\n") {
					p := wsCollapse.ReplaceAllString(part, " ")
					p = strings.TrimSpace(p)
					if p == "" {
						continue
					}
					if orig2, ok := stripped[p]; ok {
						matched = orig2
						break
					}
				}
			}
		}

		if matched != "" {
			// Inject class="md-new" into the opening tag. `loc[2..3]` is
			// the tag name; existing attrs (if any) sit after it.
			openTagRaw := htmlSrc[openStart:openEnd]
			out.WriteString(injectNewClass(openTagRaw))
			out.WriteString(block[len(openTagRaw):])
		} else {
			out.WriteString(block)
		}
		i = blockEnd
	}
	return out.String()
}

var classAttrRe = regexp.MustCompile(`class\s*=\s*"([^"]*)"`)

func injectNewClass(openTag string) string {
	if classAttrRe.MatchString(openTag) {
		return classAttrRe.ReplaceAllStringFunc(openTag, func(m string) string {
			sub := classAttrRe.FindStringSubmatch(m)
			existing := sub[1]
			if existing == "" {
				return `class="md-new"`
			}
			return `class="` + existing + ` md-new"`
		})
	}
	// No class attr: insert `class="md-new"` before the closing `>` (or `/>`).
	end := len(openTag) - 1
	if end > 0 && openTag[end-1] == '/' {
		return openTag[:end-1] + ` class="md-new"/>`
	}
	return openTag[:end] + ` class="md-new">`
}
