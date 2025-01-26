package protocol

import (
	"bufio"
	"fmt"
	"io"
	"iter"
	"strconv"
	"strings"
)

// WriteFrames writes successive frames to the given writer
// until either it returns an error, or the channel is closed
func WriteFrames(w io.Writer, ch <-chan []byte) error {
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

	for msg := range ch {
		header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(msg))
		if err := writeAll([]byte(header)); err != nil {
			return err
		}
		if err := writeAll(msg); err != nil {
			return err
		}
	}
	return nil
}

// ReadFrames reads successive frames from the given reader
// On an unexpected error it yields (nil, error), otherwise
// it yields (frame, nil). When the reader is closed no
// more frames are yielded.
func ReadFrames(r io.Reader) iter.Seq2[[]byte, error] {
	br := bufio.NewReader(r)

	return func(yield func([]byte, error) bool) {
		for {
			headers := make(map[string]string)
			var frameErr error

			for {
				line, err := br.ReadString('\n')
				if err != nil {
					if err == io.EOF && len(headers) > 0 {
						err = io.ErrUnexpectedEOF
					}
					if err != io.EOF {
						yield(nil, err)
					}
					return
				}
				if strings.TrimSpace(line) == "" {
					break
				}
				key, value, found := strings.Cut(line, ":")
				if !found {
					frameErr = fmt.Errorf("invalid header line: %q", line)
				}
				headers[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
			}

			contentLength, hasContentLength := headers["content-length"]
			if !hasContentLength {
				frameErr = fmt.Errorf("missing content-length header")
			}
			byteLen, err := strconv.Atoi(contentLength)
			if err != nil {
				frameErr = fmt.Errorf("invalid content-length header: %w", err)
			}

			if frameErr != nil {
				yield(nil, frameErr)
				return
			}
			buf := make([]byte, byteLen)

			_, err = io.ReadFull(br, buf)
			if err != nil {
				if err == io.EOF {
					err = io.ErrUnexpectedEOF
				}
				yield(nil, err)
				return
			}
			if !yield(buf, nil) {
				return
			}
		}
	}
}
