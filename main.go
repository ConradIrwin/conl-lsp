package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"unicode/utf16"

	"github.com/ConradIrwin/conl-go/schema"
	"github.com/ConradIrwin/conl-lsp/lsp"
	"github.com/ConradIrwin/dbg"
)

var log *os.File

func logPanic() {
	if r := recover(); r != nil {
		log.WriteString(fmt.Sprintf("%#v", r))
		log.WriteString(string(debug.Stack()))
	}
}

func main() {
	var err error
	log, err = os.Create("/Users/conrad/conl-lsp.log")
	if err != nil {
		panic(err)
	}
	lsp.FrameLogger = func(prefix string, data []byte) {
		log.WriteString(prefix + ": " + string(data) + "\n")
	}
	defer log.Close()
	defer func() {
		if r := recover(); r != nil {
			log.WriteString(fmt.Sprintf("%#v", r))
			log.WriteString(string(debug.Stack()))
			panic(r)
		}
	}()
	c := lsp.NewConnection()
	err = NewServer(c, log).Serve(context.Background(), os.Stdin, os.Stdout)
	if err != nil {
		dbg.To(log, err)
	}
	log.WriteString("------------")
}

type Server struct {
	c        *lsp.Connection
	log      *os.File
	mutex    sync.RWMutex
	openDocs map[lsp.DocumentURI]*TextDocument
}

func NewServer(c *lsp.Connection, log *os.File) *Server {
	s := &Server{c: c, log: log, openDocs: make(map[lsp.DocumentURI]*TextDocument)}
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
	delete(s.openDocs, params.TextDocument.URI)

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
}

func (s *Server) updateDiagnostics(doc *TextDocument) {
	defer logPanic()

	sch, err := schema.Parse([]byte(`
root
  one of
    = <map>
    = <list>
    = <scalar>

scalar
  scalar = .*

list
  items = <root>

map
  keys
    <scalar> = <root>
    `))
	if err != nil {
		log.WriteString("about to panic:)")
		panic(err)
	}

	errs := sch.Validate([]byte(doc.Content))

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
