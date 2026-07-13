package migrator

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"unicode/utf8"
)

const maxPostgresIdentifierBytes = 63

func validatePostgresIdentifier(identifier string, kind string) error {
	if identifier == "" {
		return fmt.Errorf("%s cannot be empty", kind)
	}
	if !utf8.ValidString(identifier) {
		return fmt.Errorf("%s %q is not valid UTF-8", kind, identifier)
	}
	if strings.ContainsRune(identifier, '\x00') {
		return fmt.Errorf("%s cannot contain a NUL byte", kind)
	}
	if len(identifier) > maxPostgresIdentifierBytes {
		return fmt.Errorf("%s %q is %d bytes; PostgreSQL identifiers are limited to %d bytes", kind, identifier, len(identifier), maxPostgresIdentifierBytes)
	}
	return nil
}

func hashedPostgresObjectName(candidate string, identity string) string {
	digest := sha256.Sum256([]byte(identity))
	suffix := fmt.Sprintf("_%x", digest[:8])
	maxPrefixBytes := maxPostgresIdentifierBytes - len(suffix)
	prefix := []byte(candidate)
	if len(prefix) > maxPrefixBytes {
		prefix = prefix[:maxPrefixBytes]
		for len(prefix) > 0 && !utf8.Valid(prefix) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	return string(prefix) + suffix
}

func boundedPostgresObjectName(candidate string, identity string) string {
	if len(candidate) <= maxPostgresIdentifierBytes && utf8.ValidString(candidate) {
		return candidate
	}
	return hashedPostgresObjectName(candidate, identity)
}
