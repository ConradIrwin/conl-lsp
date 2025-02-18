package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"unicode/utf16"

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
			return nil, fmt.Errorf("failed to read schema %#s: %w", result.Path, err)
		}
		return schema.Parse(bytes)
	}
	return nil, fmt.Errorf("unsupported schema location: %s", result)
}

func (s *Server) updateDiagnostics(doc *TextDocument) {
	defer logPanic()

	errs := schema.Validate([]byte(doc.Content), func(name string) (*schema.Schema, error) {
		return s.loadSchema(&doc.URI, name)
	})

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
