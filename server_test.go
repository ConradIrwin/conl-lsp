package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ConradIrwin/conl-lsp/lsp"
)

func bootServer() (*io.PipeWriter, *io.PipeReader) {
	readIn, writeIn := io.Pipe()
	readOut, writeOut := io.Pipe()

	c := lsp.NewConnection()

	go func() {
		err := NewServer(c).Serve(context.Background(),
			readIn, writeOut)
		if err != nil {
			panic(err)
		}
	}()

	return writeIn, readOut
}

type testServer struct {
	readFrame func() (*lsp.Frame, error, bool)
	writer    chan *lsp.Frame
	t         *testing.T
}

func newTestServer(t *testing.T) *testServer {
	in, out := bootServer()
	readFrame, stop := iter.Pull2(lsp.ReadFrames(out))
	ch := make(chan *lsp.Frame)
	t.Cleanup(stop)

	go func() {
		if err := lsp.WriteFrames(t.Context(), in, ch); err != nil {
			panic(err)
		}
	}()

	return &testServer{
		readFrame: readFrame,
		writer:    ch,
		t:         t,
	}
}

func newTestServerFor(t *testing.T, content string) (lsp.DocumentURI, *testServer) {
	server := newTestServer(t)
	testRequest[lsp.InitializeResult](server, "initialize", lsp.InitializeParams{})
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	uri := lsp.DocumentURI("file://" + wd + "/testdata/test.conl")
	testNotify(server, "textDocument/didOpen", lsp.DidOpenTextDocumentParams{
		TextDocument: lsp.TextDocumentItem{
			URI:        uri,
			LanguageID: "conl",
			Version:    1,
			Text:       content,
		},
	})
	return uri, server
}

var id = int32(1)

func nextId() json.RawMessage {
	id, err := json.Marshal(int(atomic.AddInt32(&id, 1)))
	if err != nil {
		panic(err)
	}
	return id
}

func contentPos(input string) (string, lsp.Position) {
	before, after, found := strings.Cut(input, "¡")
	if !found {
		panic("contentPos must contain ¡")
	}
	lines := strings.Split(before, "\n")
	return before + after, lsp.Position{
		Line:      uint32(len(lines)) - 1,
		Character: utf16Len(lines[len(lines)-1]),
	}

}

func testNotify(client *testServer, method string, params any) {
	t := client.t
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}

	client.writer <- &lsp.Frame{
		JsonRPC: "2.0",
		Method:  method,
		Params:  json.RawMessage(raw),
	}
}

func testRequest[T any](client *testServer, method string, params any) *T {
	t := client.t
	t.Helper()

	id := nextId()

	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}

	client.writer <- &lsp.Frame{
		JsonRPC: "2.0",
		Id:      id,
		Method:  method,
		Params:  json.RawMessage(raw),
	}

	var frame *lsp.Frame
outer:
	for {
		ch := make(chan *lsp.Frame)
		go func() {
			frame, err, ok := client.readFrame()
			if !ok {
				panic("no response received")
			}
			if err != nil {
				panic(err)
			}
			ch <- frame
		}()
		select {
		case frame = <-ch:
			if !bytes.Equal(frame.Id, id) {
				t.Log(frame.Id, frame.Method, string(frame.Params), string(frame.Result))
				continue
			}
			break outer
		case <-time.After(time.Second):
			t.Fatal("timeout")
		}
	}
	if frame.Error != nil {
		t.Fatal(frame.Error)
	}
	resp := new(T)
	err = json.Unmarshal(frame.Result, &resp)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestInitialize(t *testing.T) {
	in, out := bootServer()
	msg := []byte(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{}}`)
	in.Write([]byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(msg))))
	in.Write(msg)
	in.Close()
	resp, err := io.ReadAll(out)
	payload := bytes.Split(resp, []byte("\r\n"))[2]
	if !bytes.HasPrefix(resp, []byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(payload)))) {
		t.Fatalf("invalid response: %s", string(resp))
	}

	result := struct {
		Id     json.RawMessage `json:"id"`
		Result lsp.InitializeResult
	}{}
	err = json.Unmarshal(payload, &result)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result.Id, json.RawMessage(`0`)) {
		t.Fatalf("invalid id: %s", string(payload))
	}
	if result.Result.Capabilities.HoverProvider != true {
		t.Fatalf("invalid result: %s", string(payload))
	}
}

func TestHover(t *testing.T) {
	uri, server := newTestServerFor(t, "schema = ./docs.conl\ntest\n")
	hover := testRequest[lsp.Hover](server, "textDocument/hover", lsp.HoverParams{
		TextDocument: lsp.TextDocumentIdentifier{
			URI: uri,
		},
		Position: lsp.Position{
			Line:      1,
			Character: 1,
		},
	})

	expected := &lsp.Hover{
		Contents: &lsp.MarkupContent{
			Kind:  lsp.MarkupKindMarkdown,
			Value: "The test key",
		},
	}

	if !reflect.DeepEqual(hover, expected) {
		t.Fatalf("got %#v, expected %#v", hover, expected)
	}

	hover = testRequest[lsp.Hover](server, "textDocument/hover", lsp.HoverParams{
		TextDocument: lsp.TextDocumentIdentifier{
			URI: uri,
		},
		Position: lsp.Position{
			Line:      0,
			Character: 1,
		},
	})
	expected = nil

	if !reflect.DeepEqual(hover, expected) {
		t.Fatalf("got %#v, expected %#v", hover, expected)
	}
}

func expectCompletions(t *testing.T, list *lsp.CompletionList, expected ...string) {
	t.Helper()
	var actual []string
	for _, completion := range list.Items {
		actual = append(actual, completion.Label)
	}
	if !reflect.DeepEqual(expected, actual) {
		t.Fatalf("got %#v, expected %#v", actual, expected)
	}
}

func TestKeyCompletion(t *testing.T) {
	content, position := contentPos("schema = ./completions.conl\nco¡\n")
	uri, server := newTestServerFor(t, content)

	completions := testRequest[lsp.CompletionList](server, "textDocument/completion", lsp.CompletionParams{
		TextDocument: lsp.TextDocumentIdentifier{
			URI: uri,
		},
		Position: position,
	})
	expectCompletions(t, completions, "completion", "value")
}

func TestValueCompletion(t *testing.T) {
	content, position := contentPos("schema = ./completions.conl\nvalue = a¡\n")
	uri, server := newTestServerFor(t, content)

	completions := testRequest[lsp.CompletionList](server, "textDocument/completion", lsp.CompletionParams{
		TextDocument: lsp.TextDocumentIdentifier{
			URI: uri,
		},
		Position: position,
	})
	expectCompletions(t, completions, "alpha", "ant", "beta")
}

func TestCommentCompletion(t *testing.T) {
	content, position := contentPos("schema = ./completions.conl\nvalue = ;¡\n")
	uri, server := newTestServerFor(t, content)

	completions := testRequest[lsp.CompletionList](server, "textDocument/completion", lsp.CompletionParams{
		TextDocument: lsp.TextDocumentIdentifier{
			URI: uri,
		},
		Position: position,
	})
	expectCompletions(t, completions)
}
