package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

type handler struct {
	notification func(ctx context.Context, val any)
	request      func(ctx context.Context, val any) (any, error)
	pType        reflect.Type
}

type Connection struct {
	handlers map[string]handler
	out      chan *Frame
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

func (c *Connection) Serve(ctx context.Context, in io.Reader, out io.WriteCloser) error {
	errCh := make(chan error, 1)
	ctx, cancel := context.WithCancel(ctx)
	c.out = make(chan *Frame)
	c.cancel = cancel
	defer cancel()

	go func() {
		if err := WriteFrames(ctx, out, c.out); err != nil {
			FrameLogger("output error", []byte(err.Error()))
			errCh <- err
		}
		out.Close()
		close(errCh)
	}()

	for msg, err := range ReadFrames(in) {
		if err != nil {
			FrameLogger("input error", []byte(err.Error()))
			break
		}
		c.handleFrame(ctx, msg)
		select {
		case <-ctx.Done():
			break
		default:
		}
	}
	cancel()
	return <-errCh
}

func (c *Connection) Notify(method string, params any) {
	raw, err := json.Marshal(params)
	if err != nil {
		panic(err)
	}
	c.out <- &Frame{
		JsonRPC: "2.0",
		Method:  method,
		Params:  json.RawMessage(raw),
	}
}

// Terminate the connection
func (c *Connection) Exit() {
	c.cancel()
}

func (c *Connection) handleFrame(ctx context.Context, recv *Frame) {
	if recv.Batch != nil {
		c.respondError(json.RawMessage(nil), EParseError, fmt.Errorf("batch requests are not yet supported"))
		return
	}

	msgId := recv.Id
	handler, ok := c.handlers[recv.Method]
	if !ok {
		if msgId != nil {
			c.respondError(msgId, EMethodNotFound, fmt.Errorf("%s not found", recv.Method))
		}
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
	raw, err := json.Marshal(result)
	if err != nil {
		panic(err)
	}
	c.out <- &Frame{
		JsonRPC: "2.0",
		Result:  json.RawMessage(raw),
		Id:      id,
	}
}

func (c *Connection) respondError(id json.RawMessage, code ErrorCode, err error) {
	c.out <- &Frame{
		JsonRPC: "2.0",
		Error: &RpcError{
			Code:    code,
			Message: err.Error(),
		},
		Id: id,
	}
}
