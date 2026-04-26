package main

import (
	"bytes"
	"fmt"
	stdhtml "html"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	gmtext "github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Wikilink is an Obsidian-style wikilink (`[[target]]`, `[[target|label]]`)
// or embed (`![[file.png]]`). Embeds with image extensions render to <img>;
// everything else renders to <a class="wikilink"> so the dashboard's click
// handler can switch sessions when the target matches a known file stem.
type Wikilink struct {
	ast.BaseInline
	Target []byte
	Label  []byte
	Embed  bool
}

var KindWikilink = ast.NewNodeKind("Wikilink")

func (n *Wikilink) Kind() ast.NodeKind { return KindWikilink }

func (n *Wikilink) Dump(src []byte, level int) {
	ast.DumpHelper(n, src, level, map[string]string{
		"Target": string(n.Target),
		"Label":  string(n.Label),
		"Embed":  fmt.Sprintf("%t", n.Embed),
	}, nil)
}

type wikilinkParser struct{}

func (p *wikilinkParser) Trigger() []byte { return []byte{'[', '!'} }

func (p *wikilinkParser) Parse(parent ast.Node, block gmtext.Reader, pc parser.Context) ast.Node {
	line, _ := block.PeekLine()
	if len(line) < 4 {
		return nil
	}
	embed := false
	offset := 0
	switch line[0] {
	case '!':
		if !bytes.HasPrefix(line, []byte("![[")) {
			return nil
		}
		embed = true
		offset = 3
	case '[':
		if !bytes.HasPrefix(line, []byte("[[")) {
			return nil
		}
		offset = 2
	default:
		return nil
	}
	rest := line[offset:]
	end := bytes.Index(rest, []byte("]]"))
	if end <= 0 {
		return nil
	}
	inner := strings.TrimSpace(string(rest[:end]))
	if inner == "" {
		return nil
	}
	target, label := inner, inner
	if i := strings.Index(inner, "|"); i >= 0 {
		target = strings.TrimSpace(inner[:i])
		label = strings.TrimSpace(inner[i+1:])
		if target == "" {
			return nil
		}
		if label == "" {
			label = target
		}
	}
	block.Advance(offset + end + 2)
	return &Wikilink{
		Target: []byte(target),
		Label:  []byte(label),
		Embed:  embed,
	}
}

type wikilinkRenderer struct{}

func (r *wikilinkRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(KindWikilink, r.render)
}

var imgExts = map[string]bool{
	".png":  true,
	".jpg":  true,
	".jpeg": true,
	".gif":  true,
	".webp": true,
	".svg":  true,
	".bmp":  true,
	".avif": true,
}

func (r *wikilinkRenderer) render(w util.BufWriter, src []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	wl := n.(*Wikilink)
	target := string(wl.Target)
	label := string(wl.Label)
	if wl.Embed && imgExts[strings.ToLower(filepath.Ext(target))] {
		fmt.Fprintf(w, `<img src="/api/files/%s" alt="%s" class="md-embed" loading="lazy">`,
			stdhtml.EscapeString(encodeFilePath(target)),
			stdhtml.EscapeString(label))
		return ast.WalkSkipChildren, nil
	}
	fmt.Fprintf(w, `<a class="wikilink" href="#" data-wikilink="%s">%s</a>`,
		stdhtml.EscapeString(target),
		stdhtml.EscapeString(label))
	return ast.WalkSkipChildren, nil
}

// encodeFilePath URL-escapes each path segment so spaces and other reserved
// chars in Obsidian filenames survive intact. The slash separators stay
// unescaped so the browser still routes through /api/files/<rel>.
func encodeFilePath(p string) string {
	parts := strings.Split(filepath.ToSlash(p), "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

type wikilinkExt struct{}

func (e *wikilinkExt) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(parser.WithInlineParsers(
		util.Prioritized(&wikilinkParser{}, 199),
	))
	m.Renderer().AddOptions(renderer.WithNodeRenderers(
		util.Prioritized(&wikilinkRenderer{}, 199),
	))
}
