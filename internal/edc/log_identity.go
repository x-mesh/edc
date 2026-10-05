package edc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"unicode/utf8"
)

func commandKey(argv []string) (string, error) {
	if len(argv) == 0 || argv[0] == "" {
		return "", errors.New("empty command")
	}
	for _, arg := range argv {
		if !utf8.ValidString(arg) {
			return "", errors.New("invalid UTF-8 argument")
		}
	}
	data, err := json.Marshal(argv)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
