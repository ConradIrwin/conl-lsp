package main

import (
	"context"
	"errors"
	"io"
	"os"
	"runtime/debug"

	"github.com/ConradIrwin/conl-lsp/lsp"
	"github.com/ConradIrwin/dbg"
)

func main() {
	log, err := os.Create("/Users/conrad/conl-lsp.log")
	if err != nil {
		panic(err)
	}
	lsp.FrameLogger = func(isWrite bool, data []byte) {
		if isWrite {
			log.WriteString("Send: " + string(data) + "\n")
		} else {
			log.WriteString("Recv: " + string(data) + "\n")
		}
	}
	// 	input := &bytes.Buffer{}
	// 	requests := make(chan []byte)
	// 	wg := sync.WaitGroup{}
	// 	wg.Add(1)

	// 	go func() {
	// 		protocol.WriteFrames(input, requests)
	// 		defer wg.Done()
	// 	}()
	// 	requests <- []byte(`{"jsonrpc":"2.0","method":"workspace/configuration","params":{"items":[{"section":"gopls"}]},"id":21}
	// `)
	// 	close(requests)
	// 	wg.Wait()
	c := lsp.NewConnection()
	err = NewServer(c, log).Serve(context.Background(), os.Stdin, os.Stdout)
	if err != nil {
		dbg.Dbg(err)
	}
}

type Server struct {
	c   *lsp.Connection
	log *os.File
}

func NewServer(c *lsp.Connection, log *os.File) *Server {
	s := &Server{c: c, log: log}
	// Recv: {"jsonrpc":"2.0","method":"workspace/didChangeConfiguration","params":{"settings":{}}}
	// Recv: {"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":"file:///Users/conrad/0/go/conl-lsp/a.conl","languageId":"conl","version":0,"text":"a = 1\n"}}}
	lsp.HandleRequest(c, "initialize", s.initialize)
	lsp.HandleNotification(c, "initialized", func(ctx context.Context, val *lsp.InitializedParams) {})

	lsp.HandleRequest(c, "shutdown", func(ctx context.Context, params *lsp.Null) (*lsp.Null, error) {
		s.log.WriteString("Shutting down server\n")
		return &lsp.Null{}, nil
	})
	lsp.HandleNotification(c, "exit", func(ctx context.Context, val *lsp.Null) {
		s.log.WriteString("Exit\n")
		c.Exit()
	})
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
