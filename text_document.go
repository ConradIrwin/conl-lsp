package main

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf16"

	"github.com/ConradIrwin/conl-lsp/lsp"
)

type TextDocument struct {
	URI      lsp.DocumentURI
	Version  int32
	Content  string
	Language string
}

var lineEndRe = regexp.MustCompile(`\r\n?`)

func normalizeNewlines(s string) string {
	if strings.Contains(s, "\r") {
		return lineEndRe.ReplaceAllString(s, "\n")
	}
	return s
}

func NewTextDocument(uri lsp.DocumentURI, version int32, content string, language string) *TextDocument {
	return &TextDocument{
		URI:      uri,
		Version:  version,
		Content:  normalizeNewlines(content),
		Language: language,
	}
}

func (t *TextDocument) Clone() *TextDocument {
	clone := *t
	return &clone
}

func (t *TextDocument) applyChange(change lsp.TextDocumentContentChangeEvent) {
	content := normalizeNewlines(change.Text)
	if change.Range == nil {
		t.Content = content
		return
	}
	start := t.resolve(change.Range.Start)
	end := t.resolve(change.Range.End)
	t.Content = t.Content[:start] + content + t.Content[end:]
}

func (t *TextDocument) lines() []string {
	return strings.Split(t.Content, "\n")
}

func (t *TextDocument) resolve(p lsp.Position) int {
	for ix, c := range t.Content {
		if p.Line == 0 {
			if p.Character == 0 {
				return ix
			}
			if c == '\n' {
				lsp.FrameLogger("textDocument error", []byte(fmt.Sprintf("overshoot of line %v", ix)))
				return ix
			}
			delta := utf16.RuneLen(c)
			if delta == -1 || p.Character == 1 && delta == 2 {
				lsp.FrameLogger("textDocument error", []byte(fmt.Sprintf("invalid utf-16 at %v", ix)))
				delta = 1
			}
			p.Character -= uint32(delta)
		} else if c == '\n' {
			p.Line -= 1
		}
	}
	if p.Line != 0 && p.Character != 0 {
		lsp.FrameLogger("textDocument error", []byte(fmt.Sprintf("overshoot")))
	}
	return len(t.Content)
}

func (t *TextDocument) unresolve(ix int) lsp.Position {
	p := lsp.Position{Line: 0, Character: 0}
	for _, c := range t.Content[:ix] {
		if c == '\n' {
			p.Line++
			p.Character = 0
		} else {
			delta := utf16.RuneLen(c)
			if delta == -1 {
				lsp.FrameLogger("textDocument error", []byte(fmt.Sprintf("invalid utf-16 at %v", ix)))
				delta = 1
			}
			p.Character += uint32(delta)
		}
	}
	return p
}

func indexUtf16To8(line string, utf16Pos uint32) int {
	pos := 0
	for ix, c := range line {
		if pos >= int(utf16Pos) {
			return ix
		}
		pos += utf16.RuneLen(c)
	}

	return len(line)
}

func indexUtf8To16(line string, utf8Pos int) uint32 {
	pos := 0
	for ix, c := range line {
		if ix >= utf8Pos {
			return uint32(pos)
		}
		pos += utf16.RuneLen(c)
	}

	return uint32(pos)
}
