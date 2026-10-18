package integrations

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"strings"
	"unicode/utf8"
)

// cloudSSEReader implements framing only. It never decodes provider payloads,
// retries a connection, or sends Last-Event-ID on another request.
// Framing source: https://html.spec.whatwg.org/multipage/server-sent-events.html
// Invalid UTF-8 and incomplete events fail explicitly in this bounded profile.
type cloudSSEReader struct {
	stream            *CloudObservationStream
	reader            *bufio.Reader
	firstLine, skipLF bool
	lastID            string
	hasID             bool
}

func newCloudSSEReader(stream *CloudObservationStream, reader io.Reader) *cloudSSEReader {
	return &cloudSSEReader{stream: stream, reader: bufio.NewReaderSize(reader, 4096), firstLine: true}
}
func (r *cloudSSEReader) line(ctx context.Context) ([]byte, error) {
	line := make([]byte, 0, 128)
	for {
		if ctx.Err() != nil {
			return nil, &CloudError{kind: CloudBudgetExhausted}
		}
		b, err := r.reader.ReadByte()
		if err != nil {
			if err == io.EOF && len(line) > 0 {
				return nil, &CloudError{kind: CloudStreamProtocolFailed}
			}
			return nil, err
		}
		if r.skipLF {
			r.skipLF = false
			if b == '\n' {
				continue
			}
		}
		if b == '\n' {
			return line, nil
		}
		if b == '\r' {
			r.skipLF = true
			return line, nil
		}
		line = append(line, b)
		if int64(len(line)) > r.stream.limits.MaxSSELineBytes {
			return nil, &CloudError{kind: CloudResponseTooLarge}
		}
	}
}
func (r *cloudSSEReader) receive(ctx context.Context) (CloudStreamMessage, error) {
	var data []byte
	eventType := "message"
	hasData, pending := false, false
	var total int64
	// An operation-specific cancellation must unblock a pending HTTP body read.
	callbackDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { r.stream.cancel(); close(callbackDone) })
	defer func() {
		if !stop() {
			<-callbackDone
		}
	}()
	for {
		line, err := r.line(ctx)
		if err != nil {
			if err == io.EOF {
				if pending || hasData {
					return CloudStreamMessage{}, &CloudError{kind: CloudStreamProtocolFailed}
				}
				return CloudStreamMessage{}, &CloudError{kind: CloudStreamEnded}
			}
			return CloudStreamMessage{}, err
		}
		if r.firstLine {
			r.firstLine = false
			line = bytes.TrimPrefix(line, []byte{0xef, 0xbb, 0xbf})
		}
		if !utf8.Valid(line) {
			return CloudStreamMessage{}, &CloudError{kind: CloudStreamProtocolFailed}
		}
		if len(line) == 0 {
			if hasData {
				return CloudStreamMessage{kind: CloudStreamSSEEvent, data: data, eventType: eventType, id: r.lastID, hasID: r.hasID}, nil
			}
			eventType = "message"
			pending = false
			total = 0
			continue
		}
		total += int64(len(line)) + 1
		if total > r.stream.limits.MaxReceiveBytes {
			return CloudStreamMessage{}, &CloudError{kind: CloudResponseTooLarge}
		}
		if line[0] == ':' {
			continue
		}
		pending = true
		field, value, found := strings.Cut(string(line), ":")
		if !found {
			value = ""
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			if hasData {
				data = append(data, '\n')
			}
			data = append(data, value...)
			hasData = true
		case "event":
			if value != "" {
				eventType = value
			} else {
				eventType = "message"
			}
		case "id":
			if !strings.ContainsRune(value, 0) {
				r.lastID = value
				r.hasID = true
			}
		case "retry": // Provider metadata cannot authorize transport replay.
		default: // Unknown SSE fields are ignored by the SSE framing standard.
		}
	}
}
