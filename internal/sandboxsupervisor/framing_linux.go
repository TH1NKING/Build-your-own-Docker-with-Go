//go:build linux

package sandboxsupervisor

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const maximumControlMessageSize = 64 << 10

// JSON can escape one output byte as six bytes (for example, NUL).
// Requests retain their smaller bound; only result responses use this bound.
// File content is base64 (less than twice the raw budget); the final allowance
// also covers the bounded path list even when every path byte is JSON-escaped.
const maximumResultMessageSize = 12*maximumOutputBytes + 2*maximumExtractionBytes + 2*maximumControlMessageSize

var errControlMessageTooLarge = errors.New("control message is too large")

func readFrame(reader io.Reader, maximumSize uint32) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, fmt.Errorf("read control message length: %w", err)
	}
	length := binary.BigEndian.Uint32(header[:])
	if length == 0 {
		return nil, errors.New("control message is empty")
	}
	if length > maximumSize {
		return nil, fmt.Errorf("%w: length %d exceeds %d bytes", errControlMessageTooLarge, length, maximumSize)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, fmt.Errorf("read control message payload: %w", err)
	}
	return payload, nil
}

func writeFrame(writer io.Writer, payload []byte, maximumSize int) error {
	if len(payload) == 0 {
		return errors.New("control message is empty")
	}
	if len(payload) > maximumSize {
		return fmt.Errorf("control message length %d exceeds %d bytes", len(payload), maximumSize)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if _, err := writer.Write(header[:]); err != nil {
		return fmt.Errorf("write control message length: %w", err)
	}
	if _, err := writer.Write(payload); err != nil {
		return fmt.Errorf("write control message payload: %w", err)
	}
	return nil
}
