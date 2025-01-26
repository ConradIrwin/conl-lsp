package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"reflect"

	"github.com/ConradIrwin/dbg"
)

type ErrorCode int

const (
	EParseError     ErrorCode = -32700
	EInvalidRequest ErrorCode = -32600
	EMethodNotFound ErrorCode = -32601
	EInvalidParams  ErrorCode = -32602
	EInternalError  ErrorCode = -32603
)

type request struct {
	JsonRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Id      json.RawMessage `json:"id"`
}

type response struct {
	JsonRPC string          `json:"jsonrpc"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
}

type rpcError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

type handler struct {
	notification func(ctx context.Context, val any)
	request      func(ctx context.Context, val any) (any, error)
	pType        reflect.Type
}

var handlers map[string]handler

func HandleNotification[T any](method string, fn func(ctx context.Context, val T)) {
	handlers[method] = handler{
		notification: func(ctx context.Context, val any) {
			fn(ctx, val.(T))
		},
		pType: reflect.TypeOf((*T)(nil)).Elem(),
	}
}

func HandleRequest[T any, U any](method string, fn func(ctx context.Context, val T) (U, error)) {
	handlers[method] = handler{
		request: func(ctx context.Context, val any) (any, error) {
			return fn(ctx, val.(T))
		},
		pType: reflect.TypeOf((*T)(nil)).Elem(),
	}
}

func Serve(in io.Reader, out io.Writer) error {
	errCh := make(chan error, 1)
	respCh := make(chan []byte, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		if err := WriteFrames(os.Stdout, respCh); err != nil {
			dbg.Dbg(err)
			log.Printf("unexpected error writing output: %v", err)
			errCh <- err
		}
		close(errCh)
	}()

	for msg, err := range ReadFrames(in) {
		if err != nil {
			log.Printf("unexpected error reading input: %v", err)
			break
		}
		handleFrame(ctx, msg, respCh)
		select {
		case err := <-errCh:
			close(respCh)
			return err
		default:
		}
	}
	close(respCh)
	return <-errCh
}

func handleFrame(ctx context.Context, msg []byte, respCh chan []byte) {
	msgId := json.RawMessage(nil)
	request := request{}

	if len(msg) > 0 && msg[0] == '[' {
		respondError(respCh, msgId, EParseError, fmt.Errorf("batch requests are not yet supported"))
		return
	}

	if err := json.Unmarshal(msg, &request); err != nil {
		respondError(respCh, msgId, EParseError, err)
		return
	}
	msgId = request.Id
	handler, ok := handlers[request.Method]
	if !ok {
		respondError(respCh, msgId, EMethodNotFound, fmt.Errorf("%s not found", request.Method))
		return
	}

	param := reflect.New(handler.pType)
	if err := json.Unmarshal(request.Params, param.Interface()); err != nil {
		respondError(respCh, msgId, EInvalidParams, err)
		return
	}

	if handler.notification != nil {
		if request.Id != nil {
			respondError(respCh, msgId, EInvalidRequest, fmt.Errorf("notification cannot have an 'id'"))
		}
		go handler.notification(ctx, param.Elem())
		return
	}

	if request.Id == nil {
		respondError(respCh, msgId, EInvalidRequest, fmt.Errorf("request must have an 'id'"))
	}
	go func() {
		result, err := handler.request(ctx, param.Elem())
		if err != nil {
			respondError(respCh, msgId, EInternalError, err)
			return
		}
		respond(respCh, msgId, result)
	}()
}

func respond(respCh chan []byte, id json.RawMessage, result any) {
	bytes, err := json.Marshal(&response{
		JsonRPC: "2.0",
		Result:  result,
		ID:      id,
	})
	if err != nil {
		panic(err)
	}
	respCh <- bytes
}

func respondError(respCh chan []byte, id json.RawMessage, code ErrorCode, err error) {
	log.Printf("Error: %v", err)
	if id == nil {
		return
	}
	bytes, err := json.Marshal(&response{
		JsonRPC: "2.0",
		Error: &rpcError{
			Code:    code,
			Message: err.Error(),
		},
		ID: id,
	})
	if err != nil {
		panic(err)
	}
	respCh <- bytes
}
