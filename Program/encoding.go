package main

import (
	"encoding/hex"
	"fmt"
	"log"
	"math/rand"
)

const randomFieldDigits = "0123456789"

// asciiToEBCDIC maps the ASCII characters SPDH-style fields actually use (space, digits,
// uppercase letters) to their EBCDIC (cp500) code points. Anything else is an error rather
// than a silent guess — see encodeFieldValue.
var asciiToEBCDIC = map[byte]byte{
	' ': 0x40,
	'0': 0xF0, '1': 0xF1, '2': 0xF2, '3': 0xF3, '4': 0xF4,
	'5': 0xF5, '6': 0xF6, '7': 0xF7, '8': 0xF8, '9': 0xF9,
	'A': 0xC1, 'B': 0xC2, 'C': 0xC3, 'D': 0xC4, 'E': 0xC5, 'F': 0xC6, 'G': 0xC7, 'H': 0xC8, 'I': 0xC9,
	'J': 0xD1, 'K': 0xD2, 'L': 0xD3, 'M': 0xD4, 'N': 0xD5, 'O': 0xD6, 'P': 0xD7, 'Q': 0xD8, 'R': 0xD9,
	'S': 0xE2, 'T': 0xE3, 'U': 0xE4, 'V': 0xE5, 'W': 0xE6, 'X': 0xE7, 'Y': 0xE8, 'Z': 0xE9,
}

// encodeFieldValue turns a config-declared value into the exact bytes to write into the
// response — "ascii" writes the string's own bytes, "hex" decodes a hex string, "ebcdic"
// maps each character through asciiToEBCDIC, "bcd" packs a decimal-digit string 2
// digits/byte (same bit layout as "hex", but rejects a-f since BCD digits are 0-9 only) —
// and requires the result to be exactly length bytes, matching how RandomField already
// requires a value's length to equal the declared field length rather than silently
// padding or truncating.
func encodeFieldValue(value, encoding string, length int) ([]byte, error) {
	var raw []byte
	switch encoding {
	case "", "ascii":
		raw = []byte(value)
	case "hex":
		decoded, err := hex.DecodeString(value)
		if err != nil {
			return nil, fmt.Errorf("invalid hex value %q: %w", value, err)
		}
		raw = decoded
	case "ebcdic":
		encoded := make([]byte, len(value))
		for i := 0; i < len(value); i++ {
			b, ok := asciiToEBCDIC[value[i]]
			if !ok {
				return nil, fmt.Errorf("invalid ebcdic value %q: unsupported character %q", value, value[i])
			}
			encoded[i] = b
		}
		raw = encoded
	case "bcd":
		for i := 0; i < len(value); i++ {
			if value[i] < '0' || value[i] > '9' {
				return nil, fmt.Errorf("invalid bcd value %q: must be decimal digits only", value)
			}
		}
		decoded, err := hex.DecodeString(value)
		if err != nil {
			return nil, fmt.Errorf("invalid bcd value %q: %w", value, err)
		}
		raw = decoded
	default:
		return nil, fmt.Errorf("unsupported encoding %q (use \"ascii\", \"hex\", \"ebcdic\" or \"bcd\")", encoding)
	}
	if len(raw) != length {
		return nil, fmt.Errorf("value %q (encoding %s) is %d byte(s), declared length is %d", value, orDefault(encoding, "ascii"), len(raw), length)
	}
	return raw, nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// buildFields assembles a response from scratch: response is pre-allocated (all zero
// bytes) by the caller at the stub/variant's declared Length; each field writes its
// bytes at [Offset:Offset+Length] in order, so later fields win on any overlap — that's
// what lets one big fixed field stand in for a whole captured template while a few
// small request/random fields overlay the parts that vary per message. Invalid fields
// are skipped and logged rather than aborting the response.
func buildFields(stubName string, fields []TCPResponseField, request []byte, response []byte) {
	for _, f := range fields {
		if f.Echo {
			f.Source, f.RequestOffset = "request", f.Offset
		}
		if f.Length <= 0 {
			log.Printf("[TCP:%s] Skipping field %q: length must be > 0", stubName, f.Name)
			continue
		}
		if f.Offset < 0 || f.Offset+f.Length > len(response) {
			log.Printf("[TCP:%s] Skipping field %q: offset %d + length %d exceeds response %d bytes",
				stubName, f.Name, f.Offset, f.Length, len(response))
			continue
		}

		var raw []byte
		switch f.Source {
		case "", "fixed":
			encoded, err := encodeFieldValue(f.Value, f.Encoding, f.Length)
			if err != nil {
				log.Printf("[TCP:%s] Skipping field %q: %v", stubName, f.Name, err)
				continue
			}
			raw = encoded
		case "request":
			if f.RequestOffset < 0 || f.RequestOffset+f.Length > len(request) {
				log.Printf("[TCP:%s] Skipping field %q: request_offset %d + length %d exceeds received %d bytes",
					stubName, f.Name, f.RequestOffset, f.Length, len(request))
				continue
			}
			raw = request[f.RequestOffset : f.RequestOffset+f.Length]
		case "random":
			if len(f.Values) > 0 {
				var candidates [][]byte
				for _, v := range f.Values {
					encoded, err := encodeFieldValue(v, f.Encoding, f.Length)
					if err != nil {
						log.Printf("[TCP:%s] Ignoring field %q value %q: %v", stubName, f.Name, v, err)
						continue
					}
					candidates = append(candidates, encoded)
				}
				if len(candidates) == 0 {
					log.Printf("[TCP:%s] Skipping field %q: no usable values", stubName, f.Name)
					continue
				}
				raw = candidates[rand.Intn(len(candidates))]
			} else {
				digits := make([]byte, f.Length)
				for i := range digits {
					digits[i] = randomFieldDigits[rand.Intn(len(randomFieldDigits))]
				}
				encoded, err := encodeFieldValue(string(digits), f.Encoding, f.Length)
				if err != nil {
					log.Printf("[TCP:%s] Skipping field %q: generated digits %q don't fit encoding %q: %v",
						stubName, f.Name, digits, orDefault(f.Encoding, "ascii"), err)
					continue
				}
				raw = encoded
			}
		default:
			log.Printf("[TCP:%s] Skipping field %q: unknown source %q (use \"fixed\", \"request\" or \"random\")", stubName, f.Name, f.Source)
			continue
		}

		copy(response[f.Offset:f.Offset+f.Length], raw)
		log.Printf("[TCP:%s] Built field %q (%d bytes, source=%s) at response[%d:%d]: %s",
			stubName, f.Name, f.Length, orDefault(f.Source, "fixed"), f.Offset, f.Offset+f.Length,
			hex.EncodeToString(response[f.Offset:f.Offset+f.Length]))
	}
}

// pickResponseVariant chooses one of a stub's declared outcomes at random, weighted by
// each variant's Weight (default 1 when unset). Returns nil if the stub declares none,
// so callers fall back to the stub's single top-level response.
func pickResponseVariant(stub *TCPStub) *TCPResponseVariant {
	if len(stub.Responses) == 0 {
		return nil
	}
	total := 0
	for _, v := range stub.Responses {
		w := v.Weight
		if w <= 0 {
			w = 1
		}
		total += w
	}
	r := rand.Intn(total)
	for i := range stub.Responses {
		w := stub.Responses[i].Weight
		if w <= 0 {
			w = 1
		}
		if r < w {
			return &stub.Responses[i]
		}
		r -= w
	}
	return &stub.Responses[len(stub.Responses)-1]
}

// buildResponse produces the bytes to send back for one received message: it picks a
// response variant (if the stub declares more than one outcome) and builds the response
// from Fields (see TCPResponseField) — every TCP stub is built this way, there's no
// captured-template fallback. Returns the response bytes and the name of whichever
// variant/stub was used, for logging.
func buildResponse(stub *TCPStub, data []byte) ([]byte, string, error) {
	variant := pickResponseVariant(stub)

	fields := stub.Fields
	length := stub.Length
	name := stub.Name

	if variant != nil {
		fields = variant.Fields
		length = variant.Length
		name = fmt.Sprintf("%s/%s", stub.Name, variant.Name)
	}

	if len(fields) == 0 {
		return nil, name, fmt.Errorf("no fields declared - every TCP stub/variant must declare fields and length")
	}
	if length <= 0 {
		return nil, name, fmt.Errorf("fields declared but length is not set (or <= 0)")
	}
	responseData := make([]byte, length)
	buildFields(name, fields, data, responseData)
	return responseData, name, nil
}
