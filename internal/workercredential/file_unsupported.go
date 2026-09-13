//go:build !linux

package workercredential

import "errors"

func LoadFile(_ string) (string, error) {
	return "", errors.New("Worker Credential file permission checks require Linux")
}
