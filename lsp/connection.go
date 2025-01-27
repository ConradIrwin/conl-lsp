package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
)

type ErrorCode int
type MessageID json.RawMessage

const (
	EParseError     ErrorCode = -32700
	EInvalidRequest ErrorCode = -32600
	EMethodNotFound ErrorCode = -32601
	EInvalidParams  ErrorCode = -32602
	EInternalError  ErrorCode = -32603
)

type recv struct {
	JsonRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
	Id      json.RawMessage `json:"id"`
}

type request struct {
	JsonRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  any             `json:"params"`
}

type response struct {
	JsonRPC string          `json:"jsonrpc"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	Id      json.RawMessage `json:"id,omitempty"`
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

type Connection struct {
	handlers map[string]handler
	out      chan []byte
	cancel   context.CancelFunc
}

func NewConnection() *Connection {
	return &Connection{
		handlers: make(map[string]handler),
	}
}

func HandleNotification[T any](c *Connection, method string, fn func(ctx context.Context, val T)) {
	c.handlers[method] = handler{
		notification: func(ctx context.Context, val any) {
			fn(ctx, val.(T))
		},
		pType: reflect.TypeOf((*T)(nil)).Elem(),
	}
}

func HandleRequest[T any, U any](c *Connection, method string, fn func(ctx context.Context, val T) (U, error)) {
	c.handlers[method] = handler{
		request: func(ctx context.Context, val any) (any, error) {
			return fn(ctx, val.(T))
		},
		pType: reflect.TypeOf((*T)(nil)).Elem(),
	}
}

func (c *Connection) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	errCh := make(chan error, 1)
	ctx, cancel := context.WithCancel(ctx)
	c.out = make(chan []byte)
	c.cancel = cancel
	defer cancel()

	go func() {
		if err := WriteFrames(os.Stdout, c.out); err != nil {
			FrameLogger("output error", []byte(err.Error()))
			errCh <- err
		}
		close(errCh)
	}()

	for msg, err := range ReadFrames(in) {
		if err != nil {
			FrameLogger("input error", []byte(err.Error()))
			break
		}
		c.handleFrame(ctx, msg)
		select {
		case err := <-errCh:
			close(c.out)
			return err
		default:
		}
	}
	close(c.out)
	return <-errCh
}

func (c *Connection) Exit() {
	c.cancel()
}

func (c *Connection) Notify(method string, params any) {
	bytes, err := json.Marshal(&request{
		JsonRPC: "2.0",
		Method:  method,
		Params:  params,
	})
	if err != nil {
		panic(err)
	}
	c.out <- bytes
}

func (c *Connection) handleFrame(ctx context.Context, msg []byte) {
	msgId := json.RawMessage(nil)
	recv := recv{}

	if len(msg) > 0 && msg[0] == '[' {
		c.respondError(msgId, EParseError, fmt.Errorf("batch requests are not yet supported"))
		return
	}

	if err := json.Unmarshal(msg, &recv); err != nil {
		c.respondError(msgId, EParseError, err)
		return
	}
	msgId = recv.Id
	handler, ok := c.handlers[recv.Method]
	if !ok {
		c.respondError(msgId, EMethodNotFound, fmt.Errorf("%s not found", recv.Method))
		return
	}

	param := reflect.New(handler.pType)
	if err := json.Unmarshal(recv.Params, param.Interface()); err != nil {
		c.respondError(msgId, EInvalidParams, err)
		return
	}

	if handler.notification != nil {
		if recv.Id != nil {
			c.respondError(msgId, EInvalidRequest, fmt.Errorf("notification cannot have an 'id'"))
		}
		handler.notification(ctx, param.Elem().Interface())
		return
	}

	if len(recv.Id) == 0 {
		return
	}
	result, err := handler.request(ctx, param.Elem().Interface())
	if err != nil {
		c.respondError(msgId, EInternalError, err)
		return
	}
	c.respond(msgId, result)
}

func (c *Connection) respond(id json.RawMessage, result any) {
	bytes, err := json.Marshal(&response{
		JsonRPC: "2.0",
		Result:  result,
		Id:      id,
	})
	if err != nil {
		panic(err)
	}
	c.out <- bytes
}

func (c *Connection) respondError(id json.RawMessage, code ErrorCode, err error) {
	if id == nil {
		return
	}
	bytes, err := json.Marshal(&response{
		JsonRPC: "2.0",
		Error: &rpcError{
			Code:    code,
			Message: err.Error(),
		},
		Id: id,
	})
	if err != nil {
		panic(err)
	}
	c.out <- bytes
}
