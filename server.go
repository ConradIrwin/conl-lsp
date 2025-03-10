package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"unicode/utf16"

	"github.com/ConradIrwin/conl-go"
	"github.com/ConradIrwin/conl-go/schema"
	"github.com/ConradIrwin/conl-lsp/lsp"
)

type Server struct {
	c        *lsp.Connection
	mutex    sync.RWMutex
	openDocs map[lsp.DocumentURI]*TextDocument

	schemasInUse map[lsp.DocumentURI]lsp.DocumentURI
}

func NewServer(c *lsp.Connection) *Server {
	s := &Server{c: c,
		openDocs:     make(map[lsp.DocumentURI]*TextDocument),
		schemasInUse: map[lsp.DocumentURI]lsp.DocumentURI{},
	}
	lsp.HandleRequest(c, "initialize", s.initialize)
	lsp.HandleRequest(c, "shutdown", s.shutdown)
	lsp.HandleRequest(c, "textDocument/completion", s.textDocumentCompletion)
	lsp.HandleRequest(c, "textDocument/hover", s.textDocumentHover)
	lsp.HandleNotification(c, "exit", s.exit)

	lsp.HandleNotification(c, "textDocument/didOpen", s.textDocumentDidOpen)
	lsp.HandleNotification(c, "textDocument/didChange", s.textDocumentDidChange)
	lsp.HandleNotification(c, "textDocument/didClose", s.textDocumentDidClose)
	return s
}

func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	return s.c.Serve(ctx, r, w)
}

func (s *Server) initialize(ctx context.Context, params *lsp.InitializeParams) (*lsp.InitializeResult, error) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return nil, errors.New("failed to read build info")
	}
	return &lsp.InitializeResult{
		Capabilities: lsp.ServerCapabilities{
			PositionEncodingKind: lsp.PositionEncodingUTF16,
			TextDocumentSync:     lsp.TextDocumentSyncIncremental,
			CompletionProvider:   &lsp.CompletionOptions{ResolveProvider: false, TriggerCharacters: []string{"=", " "}},
			HoverProvider:        true,
		},
		ServerInfo: &lsp.ServerInfo{
			Name:    "conl-lsp",
			Version: bi.Main.Version,
		},
	}, nil
}

func (s *Server) shutdown(ctx context.Context, params *lsp.Null) (*lsp.Null, error) {
	return &lsp.Null{}, nil
}
func (s *Server) exit(ctx context.Context, params *lsp.Null) {
	s.c.Exit()
}

func (s *Server) textDocumentDidOpen(ctx context.Context, params *lsp.DidOpenTextDocumentParams) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.openDocs[params.TextDocument.URI] = NewTextDocument(
		params.TextDocument.URI,
		params.TextDocument.Version,
		params.TextDocument.Text,
		params.TextDocument.LanguageID,
	)

	go s.updateDiagnostics(s.openDocs[params.TextDocument.URI])
}

func (s *Server) textDocumentDidClose(ctx context.Context, params *lsp.DidCloseTextDocumentParams) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	delete(s.openDocs, params.TextDocument.URI)
	delete(s.schemasInUse, params.TextDocument.URI)

	s.PublishDiagnostics(&lsp.PublishDiagnosticsParams{
		URI:         params.TextDocument.URI,
		Version:     params.TextDocument.Version,
		Diagnostics: []*lsp.Diagnostic{},
	})
}

func (s *Server) textDocumentDidChange(ctx context.Context, params *lsp.DidChangeTextDocumentParams) {
	doc, ok := s.openDocs[params.TextDocument.URI]
	if !ok {
		return
	}
	newDoc := doc.Clone()
	newDoc.Version = params.TextDocument.Version

	for _, edit := range params.ContentChanges {
		newDoc.applyChange(edit)
	}
	s.openDocs[params.TextDocument.URI] = newDoc

	go s.updateDiagnostics(newDoc)
	for doc, schema := range s.schemasInUse {
		if schema == params.TextDocument.URI {
			if doc, ok := s.openDocs[doc]; ok {
				go s.updateDiagnostics(doc)
			}
		}
	}
}

func (s *Server) textDocumentCompletion(ctx context.Context, params *lsp.CompletionParams) (*lsp.CompletionList, error) {
	defer logPanic()
	doc, ok := s.openDocs[params.TextDocument.URI]
	if !ok {
		return nil, fmt.Errorf("document %v not found", params.TextDocument.URI)
	}

	lines := doc.lines()
	line := ""
	if int(params.Position.Line) < len(lines) {
		line = lines[int(params.Position.Line)]
	} else {
		return nil, fmt.Errorf("invalid position: %v >= %v", params.Position.Line, len(lines))
	}
	if int(params.Position.Character) < len(line) {
		line = line[:params.Position.Character]
	}
	column := resolveColumn(line, int(params.Position.Character))

	result := schema.Validate([]byte(doc.Content), func(name string) (*schema.Schema, error) {
		return s.loadSchema(&doc.URI, name)
	})

	key, value := splitLine(line)

	list := &lsp.CompletionList{Items: []*lsp.CompletionItem{}}

	if isInValue(line, column) {
		if value != nil && strings.HasSuffix(line[:column], " ") {
			return list, nil
		}

		values, _ := result.SuggestedValues(int(params.Position.Line) + 1)
		for _, suggestion := range values {
			list.Items = append(list.Items, &lsp.CompletionItem{
				Label: suggestion.Value,
				Documentation: &lsp.MarkupContent{
					Value: suggestion.Docs,
					Kind:  lsp.MarkupKindMarkdown,
				},
			})
		}
		if strings.HasSuffix(line, "=") {
			for _, item := range list.Items {
				item.InsertText = " " + item.Label
			}
		}

	} else {
		if key != nil && strings.HasSuffix(line[:column], " ") {
			return list, nil
		}

		lno := getParentLine(lines, int(params.Position.Line))

		for _, suggestion := range result.SuggestedKeys(lno + 1) {
			list.Items = append(list.Items, &lsp.CompletionItem{
				Label: suggestion.Value,
				Documentation: &lsp.MarkupContent{
					Value: suggestion.Docs,
					Kind:  lsp.MarkupKindMarkdown,
				},
			})
		}
	}

	return list, nil
}

var quotedLiteral = regexp.MustCompile(`^"(?:[^\\"]|\\.)*"`)

func isInValue(line string, pos int) bool {
	line = quotedLiteral.ReplaceAllStringFunc(line, func(quoted string) string {
		return strings.Repeat("a", len(quoted))
	})

	eq := strings.Index(line, "=")
	if eq < 0 {
		return false
	}

	return pos > eq
}

func getParentLine(lines []string, lno int) int {
	line := lines[lno]
	p := 0
	for p < len(line) && (line[p] == ' ' || line[p] == '\t') {
		p += 1
	}
	lno -= 1
	for lno >= 0 {
		prefix := strings.Trim(lines[lno][0:p], " \t")
		if prefix != "" && !strings.HasPrefix(prefix, ";") {
			break
		}
		lno -= 1
	}

	return lno
}

func splitLine(line string) (*conl.Token, *conl.Token) {
	var key *conl.Token
	var value *conl.Token
	for token := range conl.Tokens([]byte(line)) {
		if token.Kind == conl.MapKey {
			key = &token
		} else if token.Kind == conl.Scalar {
			value = &token
		}
	}
	return key, value
}

func (s *Server) textDocumentHover(ctx context.Context, params *lsp.HoverParams) (*lsp.Hover, error) {
	defer logPanic()
	doc, ok := s.openDocs[params.TextDocument.URI]
	if !ok {
		return nil, fmt.Errorf("document %v not found", params.TextDocument.URI)
	}

	result := schema.Validate([]byte(doc.Content), func(name string) (*schema.Schema, error) {
		return s.loadSchema(&doc.URI, name)
	})

	lines := doc.lines()
	line := ""
	if int(params.Position.Line) < len(lines) {
		line = lines[int(params.Position.Line)]
	} else {
		return nil, fmt.Errorf("invalid position: %v >= %v", params.Position.Line, len(lines))
	}
	column := resolveColumn(line, int(params.Position.Character))

	key, value := splitLine(line)

	if isInValue(line, column) && value != nil {
		values, _ := result.SuggestedValues(int(params.Position.Line) + 1)
		for _, v := range values {
			if v.Value == value.Content {
				return &lsp.Hover{
					Contents: &lsp.MarkupContent{
						Kind:  lsp.MarkupKindMarkdown,
						Value: v.Docs,
					},
				}, nil
			}
		}
	}

	if key == nil {
		return nil, nil
	}

	lno := getParentLine(lines, int(params.Position.Line))

	keys := result.SuggestedKeys(lno + 1)
	for _, k := range keys {
		if k.Value == key.Content {
			return &lsp.Hover{
				Contents: &lsp.MarkupContent{
					Kind:  lsp.MarkupKindMarkdown,
					Value: k.Docs,
				},
			}, nil
		}
	}

	return nil, nil
}

func (s *Server) loadSchema(doc *lsp.DocumentURI, requested string) (*schema.Schema, error) {
	s.mutex.Lock()
	delete(s.schemasInUse, *doc)
	s.mutex.Unlock()
	if requested == "" {
		return schema.Any(), nil
	}

	relative, err := url.Parse(requested)
	if err != nil {
		return nil, fmt.Errorf("could not interpret %#v as a path or url", requested)
	}
	base := (*url.URL)(doc)
	result := (*lsp.DocumentURI)(base.ResolveReference(relative))
	s.mutex.Lock()
	s.schemasInUse[*doc] = *result
	s.mutex.Unlock()
	if schemaDoc, ok := s.openDocs[*result]; ok {
		return schema.Parse([]byte(schemaDoc.Content))
	}
	if result.Scheme == "file" {
		bytes, err := os.ReadFile(result.Path)
		if err != nil {
			return nil, fmt.Errorf("failed to read schema %s: %w", result.Path, err)
		}
		return schema.Parse(bytes)
	}
	return nil, fmt.Errorf("unsupported schema location: %v", result)
}

func (s *Server) updateDiagnostics(doc *TextDocument) {
	defer logPanic()

	errs := schema.Validate([]byte(doc.Content), func(name string) (*schema.Schema, error) {
		return s.loadSchema(&doc.URI, name)
	}).Errors()

	if len(errs) > 0 {
		diagnostics := make([]*lsp.Diagnostic, len(errs))
		for i, err := range errs {
			line := strings.Split(doc.Content, "\n")[err.Lno()-1]
			start, end := err.RuneRange(line)

			diagnostics[i] = &lsp.Diagnostic{
				Range: lsp.Range{
					Start: lsp.Position{
						Line:      uint32(err.Lno() - 1),
						Character: utf16Len(line[:start]),
					},
					End: lsp.Position{
						Line:      uint32(err.Lno() - 1),
						Character: utf16Len(line[:end]),
					},
				},
				Severity: lsp.DiagnosticSeverityError,
				Message:  err.Msg(),
			}
		}

		s.PublishDiagnostics(&lsp.PublishDiagnosticsParams{
			URI:         doc.URI,
			Version:     doc.Version,
			Diagnostics: diagnostics,
		})
	} else {
		s.ClearDiagnostics(doc)
	}
}

func utf16Len(s string) uint32 {
	ret := uint32(0)
	for _, r := range s {
		ret += uint32(utf16.RuneLen(r))
	}

	return ret
}

func (s *Server) PublishDiagnostics(params *lsp.PublishDiagnosticsParams) {
	s.c.Notify("textDocument/publishDiagnostics", params)
}

func (s *Server) ClearDiagnostics(doc *TextDocument) {
	s.PublishDiagnostics(&lsp.PublishDiagnosticsParams{
		URI:         doc.URI,
		Version:     doc.Version,
		Diagnostics: []*lsp.Diagnostic{},
	})
}
