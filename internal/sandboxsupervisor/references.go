package sandboxsupervisor

import "strings"

func profileDigest(identity string) (string, bool) {
	const prefix = "sha256:"
	if !strings.HasPrefix(identity, prefix) {
		return "", false
	}
	digest := strings.TrimPrefix(identity, prefix)
	if len(digest) != 64 {
		return "", false
	}
	for _, character := range digest {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return "", false
			}
		}
	}
	return digest, true
}

func validOpaqueIdentifier(identifier string) bool {
	if len(identifier) == 0 || len(identifier) > 64 {
		return false
	}
	for index, character := range identifier {
		lowercaseLetter := character >= 'a' && character <= 'z'
		digit := character >= '0' && character <= '9'
		if lowercaseLetter || digit || (index > 0 && (character == '-' || character == '_')) {
			continue
		}
		return false
	}
	return true
}
