package common

import (
	"encoding/base64"
	"fmt"
	"mime"
	"net/http"
	"strings"
)

const (
	base64Table = "ABCDEFGHIJKLMNOPQRSTpqrstuvwxyz0123456789+/UVWXYZabcdefghijklmno"
)

var coder = base64.NewEncoding(base64Table)

func Base64Encode(src []byte) []byte {
	return []byte(coder.EncodeToString(src))
}

func Base64Decode(src []byte) ([]byte, error) {
	return coder.DecodeString(string(src))
}

//If we don't do this, POST method without Content-type (even with empty body) will fail
func ParseForm(r *http.Request) error {
	if r == nil {
		return nil
	}
	if err := r.ParseForm(); err != nil && !strings.HasPrefix(err.Error(), "mime:") {
		return err
	}
	return nil
}

func matchesContentType(contentType, expectedType string) bool {
	mimetype, _, err := mime.ParseMediaType(contentType)
	return err == nil && mimetype == expectedType
}

func CheckForJson(r *http.Request) error {
	ct := r.Header.Get("Content-Type")

	// No Content-Type header is ok as long as there's no Body
	if ct == "" {
		if r.Body == nil || r.ContentLength == 0 {
			return nil
		}
	}

	// Otherwise it better be json
	if matchesContentType(ct, "application/json") {
		return nil
	}
	return fmt.Errorf("Content-Type specified (%s) must be 'application/json'", ct)
}
