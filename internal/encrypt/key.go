package encrypt

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

const aes256KeySize = 32

func decodeAES256Key(key string) ([]byte, error) {
	if len(key) == aes256KeySize {
		return []byte(key), nil
	}

	decoders := []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}
	for _, encoding := range decoders {
		decoded, err := encoding.DecodeString(key)
		if err == nil && len(decoded) == aes256KeySize {
			return decoded, nil
		}
	}

	if decoded, err := hex.DecodeString(key); err == nil && len(decoded) == aes256KeySize {
		return decoded, nil
	}

	return nil, fmt.Errorf("encryption key must be 32 raw bytes, 64 hexadecimal characters, or base64-encoded 32 bytes")
}
