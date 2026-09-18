package main

import (
	"bytes"
	"testing"
)

func TestEncodeFieldValue(t *testing.T) {
	cases := []struct {
		name     string
		value    string
		encoding string
		length   int
		want     []byte
		wantErr  bool
	}{
		{"ascii", "AB", "ascii", 2, []byte{0x41, 0x42}, false},
		{"hex", "0102", "hex", 2, []byte{0x01, 0x02}, false},
		{"ebcdic digits+letters+space", "A0 9", "ebcdic", 4, []byte{0xC1, 0xF0, 0x40, 0xF9}, false},
		{"ebcdic unsupported char", "a", "ebcdic", 1, nil, true},
		{"bcd", "0186", "bcd", 2, []byte{0x01, 0x86}, false},
		{"bcd rejects hex letters", "01ab", "bcd", 2, nil, true},
		{"wrong length", "AB", "ascii", 3, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := encodeFieldValue(c.value, c.encoding, c.length)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %x", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !bytes.Equal(got, c.want) {
				t.Fatalf("got %x, want %x", got, c.want)
			}
		})
	}
}
