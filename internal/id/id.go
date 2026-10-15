// Package id generates short random identifiers.
package id

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// New returns prefix plus 16 lowercase hex characters from crypto/rand.
func New(prefix string) (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return prefix + hex.EncodeToString(buf[:]), nil
}
