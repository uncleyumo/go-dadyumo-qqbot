package memepool

import (
	"errors"
	"os"
	"strconv"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func asParseError(err error, out **ParseError) bool {
	var pe *ParseError
	if errors.As(err, &pe) {
		*out = pe
		return true
	}
	return false
}

func asVersionError(err error, out **VersionError) bool {
	var ve *VersionError
	if errors.As(err, &ve) {
		*out = ve
		return true
	}
	return false
}