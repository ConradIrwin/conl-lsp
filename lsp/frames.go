package lsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"strconv"
	"strings"
)

// FrameLogger can be used to log frames.
// The first parameter is either "send" or "recv", depending on the direction of the frame.
var FrameLogger = func(string, []byte) {}

// A Frame is represents the wire-format of JSON RPC.
// It can be one of several things:
// - a request (Id, Method, Params)
// - a notification (Method, Params)
// - a successful response (Id, Result)
// - an error response (Id, Error)
// - a batch of frames (Batch)
type Frame struct {
	JsonRPC string          `json:"jsonrpc"`
	Id      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RpcError       `json:"error,omitempty"`
	Batch   []*Frame        `json:"-"`
}

type RpcError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

// WriteFrames writes successive frames to the given writer
// until either it returns an error, or the channel is closed
func WriteFrames(ctx context.Context, w io.Writer, ch <-chan *Frame) error {
	writeAll := func(data []byte) error {
		for len(data) > 0 {
			n, err := w.Write(data)
			if err != nil {
				return err
			}
			data = data[n:]
		}
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case frame := <-ch:
			msg, err := json.Marshal(frame)
			if err != nil {
				panic(err)
			}
			FrameLogger("send", msg)
			header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(msg))
			if err := writeAll([]byte(header)); err != nil {
				return err
			}
			if err := writeAll(msg); err != nil {
				return err
			}
		}
	}
}

// ReadFrames reads successive frames from the given reader
// On an unexpected error it yields (nil, error), otherwise
// it yields (frame, nil). When the reader is closed no
// more frames are yielded.
func ReadFrames(r io.Reader) iter.Seq2[*Frame, error] {
	br := bufio.NewReader(r)

	return func(yield func(*Frame, error) bool) {
		for {
			headers := make(map[string]string)
			var frameErr error

			for {
				line, err := br.ReadString('\n')
				if err != nil {
					if err == io.EOF && len(headers) > 0 {
						err = io.ErrUnexpectedEOF
					}
					FrameLogger("recv error", []byte(err.Error()))
					if err != io.EOF {
						yield(nil, err)
					}
					return
				}
				if strings.TrimSpace(line) == "" && len(headers) > 0 {
					break
				}
				key, value, found := strings.Cut(line, ":")
				if !found {
					frameErr = fmt.Errorf("invalid header line: %q", line)
				}
				headers[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
			}

			byteLen, err := strconv.Atoi(strings.TrimSpace(headers["content-length"]))
			if err != nil {
				frameErr = fmt.Errorf("invalid content-length header: %w", err)
			}

			if frameErr != nil {
				FrameLogger("recv error", []byte(err.Error()))
				yield(nil, frameErr)
				return
			}
			buf := make([]byte, byteLen)

			_, err = io.ReadFull(br, buf)
			if err != nil {
				if err == io.EOF {
					err = io.ErrUnexpectedEOF
				}
				FrameLogger("recv error", []byte(err.Error()))
				yield(nil, err)
				return
			}
			FrameLogger("recv", buf)
			frame := Frame{}
			if bytes.HasPrefix(buf, []byte("[")) {
				frames := []*Frame{}
				err = json.Unmarshal(buf, &frames)
				if err != nil {
					yield(nil, err)
					return
				}
				frame.Batch = frames
			} else {
				err = json.Unmarshal(buf, &frame)
				if err != nil {
					yield(nil, err)
					return
				}
			}
			if !yield(&frame, nil) {
				return
			}
		}
	}
}
