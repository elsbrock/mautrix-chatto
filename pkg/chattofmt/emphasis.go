package chattofmt

import (
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// wordBoundaryEmphasisParser ports Chatto's markdown-it rule of the same name
// (apps/frontend/src/lib/markdown.ts): `*` and `_` runs that are not at a
// word boundary are literal text. That keeps `snake_case`, `2*3*4` and
// kaomoji like `¯\_(ツ)_/¯` from turning into emphasis, while `*it*`, `_it_`
// and `**bold**` still work. markdown-it scans the whole inline content; this
// scans the current line, which only differs for markers split across lines.
type wordBoundaryEmphasisParser struct{}

func (wordBoundaryEmphasisParser) Trigger() []byte { return []byte{'*', '_'} }

func isASCIIAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func (wordBoundaryEmphasisParser) Parse(_ ast.Node, block text.Reader, _ parser.Context) ast.Node {
	line, seg := block.PeekLine()
	if len(line) == 0 {
		return nil
	}
	marker := line[0]
	runLen := 1
	for runLen < len(line) && line[runLen] == marker {
		runLen++
	}
	source := block.Source()
	start := seg.Start
	beforeAlnum := start > 0 && isASCIIAlnum(source[start-1])
	afterAlnum := runLen < len(line) && isASCIIAlnum(line[runLen])

	intraword := runLen == 1 && beforeAlnum && afterAlnum
	kaomojiLike := false
	if !beforeAlnum && !afterAlnum {
		forwardOK := true
		for i := runLen; i < len(line) && line[i] != '\n'; i++ {
			if line[i] == marker {
				break
			}
			if isASCIIAlnum(line[i]) {
				forwardOK = false
				break
			}
		}
		if forwardOK {
			kaomojiLike = true
			for i := start - 1; i >= 0 && source[i] != '\n'; i-- {
				if source[i] == marker {
					break
				}
				if isASCIIAlnum(source[i]) {
					kaomojiLike = false
					break
				}
			}
		}
	}
	if !intraword && !kaomojiLike {
		return nil
	}
	block.Advance(runLen)
	return ast.NewTextSegment(text.NewSegment(start, start+runLen))
}
