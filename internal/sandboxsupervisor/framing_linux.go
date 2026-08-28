//go:build linux

package sandboxsupervisor

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const maximumControlMessageSize = 64 << 10

var errControlMessageTooLarge = errors.New("control message is too large")

func readFrame(reader io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, fmt.Errorf("read control message length: %w", err)
	}
	length := binary.BigEndian.Uint32(header[:])
	if length == 0 {
		return nil, errors.New("control message is empty")
	}
	if length > maximumControlMessageSize {
		return nil, fmt.Errorf("%w: length %d exceeds %d bytes", errControlMessageTooLarge, length, maximumControlMessageSize)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, fmt.Errorf("read control message payload: %w", err)
	}
	return payload, nil
}

func writeFrame(writer io.Writer, payload []byte) error {
	if len(payload) == 0 {
		return errors.New("control message is empty")
	}
	if len(payload) > maximumControlMessageSize {
		return fmt.Errorf("control message length %d exceeds %d bytes", len(payload), maximumControlMessageSize)
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
