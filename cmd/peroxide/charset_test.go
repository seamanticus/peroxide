package main

import (
	"testing"

	"github.com/emersion/go-message"
)

// Nothing linked into peroxide sets go-message's CharsetReader (importing
// go-message/charset does): the APPEND encryption (protonmail encryptText)
// decodes charsets itself. Set, go-message would decode them first, and
// encryptText would decode a second time, garbling text that isn't UTF-8.
// evidence: witnesses
func TestNoCharsetReader(t *testing.T) {
	if message.CharsetReader != nil {
		t.Error("message.CharsetReader is set: a package imports github.com/emersion/go-message/charset")
	}
}
