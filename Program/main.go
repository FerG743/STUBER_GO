package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// StubConfig holds the entire configuration
type StubConfig struct {
	HTTPStubs []HTTPStub `yaml:"http_stubs" json:"http_stubs"`
	TCPStubs  []TCPStub  `yaml:"tcp_stubs" json:"tcp_stubs"`
}

// HTTPStub represents a single HTTP stub endpoint
type HTTPStub struct {
	Name          string                 `yaml:"name" json:"name"`
	Method        string                 `yaml:"method" json:"method"`
	Path          string                 `yaml:"path" json:"path"`
	Headers       map[string]string      `yaml:"headers,omitempty" json:"headers,omitempty"`
	BodyContains  string                 `yaml:"body_contains,omitempty" json:"body_contains,omitempty"`   // Match if body contains this string
	BodyJSON      map[string]interface{} `yaml:"body_json,omitempty" json:"body_json,omitempty"`           // Match specific JSON fields
	RequireFields []string               `yaml:"require_fields,omitempty" json:"require_fields,omitempty"` // Match only if these dotted JSON paths are present, any value
	Response      HTTPResponse           `yaml:"response" json:"response"`
}

// HTTPResponse defines the HTTP stub response
type HTTPResponse struct {
	Status  int               `yaml:"status" json:"status"`
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	Body    string            `yaml:"body" json:"body"`
	Delay   int               `yaml:"delay,omitempty" json:"delay,omitempty"`
}

// TCPStub represents a TCP stub configuration
type TCPStub struct {
	Name            string               `yaml:"name" json:"name"`
	Port            int                  `yaml:"port" json:"port"`
	CloseAfter      bool                 `yaml:"close_after" json:"close_after"`
	Delay           int                  `yaml:"delay,omitempty" json:"delay,omitempty"`
	ValidateRequest bool                 `yaml:"validate_request" json:"validate_request"`
	MinLength       int                  `yaml:"min_length,omitempty" json:"min_length,omitempty"`
	MaxLength       int                  `yaml:"max_length,omitempty" json:"max_length,omitempty"`
	Fields          []TCPResponseField   `yaml:"fields,omitempty" json:"fields,omitempty"`       // Build the response from field specs — see TCPResponseField
	Length          int                  `yaml:"length,omitempty" json:"length,omitempty"`       // Total response byte length; required when Fields is set
	Responses       []TCPResponseVariant `yaml:"responses,omitempty" json:"responses,omitempty"` // Multiple possible outcomes (e.g. approved/declined), chosen at random per message
	Match           *TCPMatch            `yaml:"match,omitempty" json:"match,omitempty"`         // Selects this stub among others sharing the same Port — see TCPMatch
	Label           string               `yaml:"label,omitempty" json:"label,omitempty"`         // Human-readable message name for logs, e.g. "Plan 86 (Tiempo Aire)"; Name stays the stable key
	Meta            *TCPMeta             `yaml:"meta,omitempty" json:"meta,omitempty"`           // Portal-only display info; the server ignores it
}

// TCPMeta is display info for the portal's message list (served raw via the sim agent's
// GET /stubs). The server never reads it; it lives here so the config schema stays in one place.
type TCPMeta struct {
	ID               string `yaml:"id" json:"id"`
	Category         string `yaml:"category" json:"category"` // "spdh" or "iso8583"
	DisplayName      string `yaml:"display_name" json:"display_name"`
	Description      string `yaml:"description" json:"description"`
	SampleRequestHex string `yaml:"sample_request_hex" json:"sample_request_hex"`
}

// TCPMatch lets several message definitions share one port, the way a real BASE24-style
// switch multiplexes every transaction code over a single connection: each stub declares
// the byte range that identifies its own message (e.g. SPDH's Codigo_Transaccion) and the
// expected value there, and the first stub on that port whose Match is satisfied by the
// incoming bytes handles it. A stub with no Match always matches — an unconditional
// catch-all, and how a port with only one message type keeps working unchanged.
type TCPMatch struct {
	Offset int    `yaml:"offset" json:"offset"`
	Value  string `yaml:"value" json:"value"` // hex-encoded expected byte(s) at Offset
}

// TCPResponseVariant is one possible outcome for a TCP stub (e.g. an approved vs a
// declined reply). When a stub declares more than one, one is picked at random
// (weighted by Weight, default 1) for every message it responds to.
type TCPResponseVariant struct {
	Name   string             `yaml:"name" json:"name"`
	Weight int                `yaml:"weight,omitempty" json:"weight,omitempty"`
	Fields []TCPResponseField `yaml:"fields" json:"fields"` // Build this variant's response from field specs — see TCPResponseField
	Length int                `yaml:"length" json:"length"` // Total response byte length
}

// TCPResponseField declaratively builds one byte range of a response: every TCP stub
// response is assembled from scratch — allocate Length zero bytes (Length is set on the
// stub or variant), then write each field's value at [Offset:Offset+Length] in order.
// Fields can overlap; later entries win, which is how a captured proprietary blob (one
// big Source:"fixed" field) can sit underneath a few small Source:"request"/"random"
// overlays for the parts that vary per message.
type TCPResponseField struct {
	Name   string `yaml:"name,omitempty" json:"name,omitempty"`
	Offset int    `yaml:"offset" json:"offset"`
	Length int    `yaml:"length" json:"length"`
	// Source: "fixed" (default) uses Value; "request" copies Length bytes from the
	// incoming request at RequestOffset; "random" picks one of Values at random (each
	// must encode to exactly Length bytes), or — if Values is empty — fills with fresh
	// random ASCII digits, same as RandomField.
	Source        string   `yaml:"source,omitempty" json:"source,omitempty"`
	Encoding      string   `yaml:"encoding,omitempty" json:"encoding,omitempty"` // "ascii" (default), "hex", "ebcdic", or "bcd"; applies to Value/Values
	Value         string   `yaml:"value,omitempty" json:"value,omitempty"`
	RequestOffset int      `yaml:"request_offset,omitempty" json:"request_offset,omitempty"`
	Values        []string `yaml:"values,omitempty" json:"values,omitempty"`
	// Echo is shorthand for the common case of source:"request" with RequestOffset ==
	// Offset (the request and response share a wire layout there) — most real protocol
	// envelopes are byte-for-byte echoes at the same position, e.g. BASE24/SPDH fields the
	// dictionary itself calls out as "echoed". Set true instead of repeating the offset
	// twice; a field with a genuinely different RequestOffset still uses Source directly.
	Echo bool `yaml:"echo,omitempty" json:"echo,omitempty"`
}

// HTTPStubServer manages HTTP stub endpoints
type HTTPStubServer struct {
	stubs []HTTPStub
}

// NewHTTPStubServer creates a new HTTP stub server
func NewHTTPStubServer() *HTTPStubServer {
	return &HTTPStubServer{
		stubs: []HTTPStub{},
	}
}

// AddStub adds an HTTP stub programmatically
func (s *HTTPStubServer) AddStub(stub HTTPStub) {
	s.stubs = append(s.stubs, stub)
}

// matchRequest checks if a request matches a stub
func (s *HTTPStubServer) matchRequest(r *http.Request, stub HTTPStub) bool {
	// Match method
	if !strings.EqualFold(stub.Method, r.Method) {
		return false
	}

	// Match path (exact match for now, can be extended to patterns)
	if stub.Path != r.URL.Path {
		return false
	}

	// Match headers if specified
	for key, value := range stub.Headers {
		if r.Header.Get(key) != value {
			return false
		}
	}

	// Match body if specified
	if stub.BodyContains == "" && len(stub.BodyJSON) == 0 && len(stub.RequireFields) == 0 {
		return true
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes)) // restore for the handler

	if stub.BodyContains != "" && !bytes.Contains(bodyBytes, []byte(stub.BodyContains)) {
		return false
	}

	if len(stub.BodyJSON) > 0 || len(stub.RequireFields) > 0 {
		var requestJSON map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &requestJSON); err != nil {
			return false
		}
		for key, expectedValue := range stub.BodyJSON {
			if !jsonFieldMatches(requestJSON, key, expectedValue) {
				return false
			}
		}
		for _, path := range stub.RequireFields {
			if _, ok := jsonFieldAt(requestJSON, path); !ok {
				return false
			}
		}
	}

	return true
}

// jsonFieldAt walks a dotted path (e.g. "hdr.remitente.centro") into a decoded JSON
// object and returns the value found there, or ok=false if any segment is missing —
// the shared primitive behind both exact-value (jsonFieldMatches) and presence-only
// (require_fields) matching.
func jsonFieldAt(data map[string]interface{}, path string) (interface{}, bool) {
	var current interface{} = data
	for _, key := range strings.Split(path, ".") {
		m, ok := current.(map[string]interface{})
		if !ok {
			return nil, false
		}
		current, ok = m[key]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

// jsonFieldMatches checks if a JSON field matches expected value (supports nested paths with dots)
func jsonFieldMatches(data map[string]interface{}, path string, expectedValue interface{}) bool {
	current, ok := jsonFieldAt(data, path)
	return ok && fmt.Sprintf("%v", current) == fmt.Sprintf("%v", expectedValue)
}

// ServeHTTP handles incoming HTTP requests
func (s *HTTPStubServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("[HTTP %s] %s %s", time.Now().Format("15:04:05"), r.Method, r.URL.Path)

	for _, stub := range s.stubs {
		if s.matchRequest(r, stub) {
			log.Printf("[HTTP] Matched stub: %s", stub.Name)

			if stub.Response.Delay > 0 {
				time.Sleep(time.Duration(stub.Response.Delay) * time.Millisecond)
			}

			for key, value := range stub.Response.Headers {
				w.Header().Set(key, value)
			}

			w.WriteHeader(stub.Response.Status)
			w.Write([]byte(stub.Response.Body))
			return
		}
	}

	log.Printf("[HTTP] No stub matched for %s %s", r.Method, r.URL.Path)
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte(`{"error": "No stub matched"}`))
}

// TCPStubServer handles TCP connections. Several stubs can share one port — see TCPMatch —
// so each port maps to an ordered list, not a single stub.
type TCPStubServer struct {
	stubs map[int][]*TCPStub
}

// NewTCPStubServer creates a new TCP stub server
func NewTCPStubServer() *TCPStubServer {
	return &TCPStubServer{
		stubs: make(map[int][]*TCPStub),
	}
}

// AddStub adds a TCP stub, appending to whatever's already registered on its port.
func (s *TCPStubServer) AddStub(stub TCPStub) {
	s.stubs[stub.Port] = append(s.stubs[stub.Port], &stub)
}

// StubCount returns the total number of TCP stubs across every port, since s.stubs is now
// keyed by port (which can hold several stubs) rather than one-stub-per-port.
func (s *TCPStubServer) StubCount() int {
	total := 0
	for _, stubs := range s.stubs {
		total += len(stubs)
	}
	return total
}

// Start starts all TCP stub listeners, one per distinct port.
func (s *TCPStubServer) Start() error {
	for port, stubs := range s.stubs {
		go func(p int, st []*TCPStub) {
			if err := s.listenTCP(p, st); err != nil {
				// One line per stub, in the "[TCP] Stub <name> error: ..." shape
				// agent/sim_agent.py's _LISTEN_ERROR_RE parses to flag a failed bind.
				for _, stub := range st {
					log.Printf("[TCP] Stub %s error: %v", stub.Name, err)
				}
			}
		}(port, stubs)
	}
	return nil
}

// listenTCP starts a TCP listener shared by every stub registered on this port; which one
// handles a given connection (or message, for a persistent connection) is decided per
// message by matchStub, not fixed at listen time.
func (s *TCPStubServer) listenTCP(port int, stubs []*TCPStub) error {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("failed to listen on port %d: %w", port, err)
	}
	defer listener.Close()

	names := make([]string, len(stubs))
	for i, st := range stubs {
		names[i] = st.Name
	}
	log.Printf("[TCP] Listening on port %d with %d stub(s): %v", port, len(stubs), names)

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("[TCP] Error accepting connection: %v", err)
			continue
		}

		go s.handleConnection(conn, port, stubs)
	}
}

// matchStub picks the stub that should handle one received message: the first (in
// declaration order) whose Match condition is satisfied by data, or that has no Match at
// all (an unconditional catch-all). Returns nil if nothing on this port claims it.
func matchStub(stubs []*TCPStub, data []byte) *TCPStub {
	for _, stub := range stubs {
		if stub.Match == nil {
			return stub
		}
		want, err := hex.DecodeString(stub.Match.Value)
		if err != nil {
			log.Printf("[TCP:%s] Skipping match check: invalid match value %q: %v", stub.Name, stub.Match.Value, err)
			continue
		}
		if stub.Match.Offset < 0 || stub.Match.Offset+len(want) > len(data) {
			continue
		}
		if bytes.Equal(data[stub.Match.Offset:stub.Match.Offset+len(want)], want) {
			return stub
		}
	}
	return nil
}

// validateRequest validates the incoming TCP request (hex character length)
func (s *TCPStubServer) validateRequest(data []byte, stub *TCPStub) (bool, string) {
	hexData := hex.EncodeToString(data)
	hexLength := len(hexData) // Count hex characters, not bytes
	byteLength := len(data)

	log.Printf("[TCP:%s] Validating request: %d bytes (%d hex chars), hex: %s",
		stub.Name, byteLength, hexLength, hexData)

	// Check minimum length (in hex characters)
	if stub.MinLength > 0 && hexLength < stub.MinLength {
		return false, fmt.Sprintf("Request too short: %d hex chars (min: %d)", hexLength, stub.MinLength)
	}

	// Check maximum length (in hex characters)
	if stub.MaxLength > 0 && hexLength > stub.MaxLength {
		return false, fmt.Sprintf("Request too long: %d hex chars (max: %d)", hexLength, stub.MaxLength)
	}

	return true, "OK"
}

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

// handleConnection handles a single TCP connection. stubs is every message definition
// registered on this port; which one applies is decided per message (not once for the
// whole connection), since a real multiplexed connection can carry a different message
// type on every exchange.
//
// Every message on the wire opens with a 2-byte big-endian length prefix giving the byte
// count that follows (the "frame_prefix" field in STUBS.yaml, e.g. wire[0:1]) - reads pull
// exactly one frame via the prefix rather than a single Read() into a fixed buffer, since
// the OS is free to deliver one client write as more than one Read(), which previously
// made handleConnection treat a partial message as the whole thing and close early.
func (s *TCPStubServer) handleConnection(conn net.Conn, port int, stubs []*TCPStub) {
	defer conn.Close()

	clientAddr := conn.RemoteAddr().String()
	log.Printf("[TCP:%d %s] Connection from %s", port, time.Now().Format("15:04:05"), clientAddr)

	reader := bufio.NewReader(conn)

	for {
		header := make([]byte, 2)
		if _, err := io.ReadFull(reader, header); err != nil {
			return
		}
		payload := make([]byte, binary.BigEndian.Uint16(header))
		if _, err := io.ReadFull(reader, payload); err != nil {
			return
		}
		data := append(header, payload...)
		log.Printf("[TCP:%d] Received %d bytes: %s", port, len(data), hex.EncodeToString(data))

		stub := matchStub(stubs, data)
		if stub == nil {
			log.Printf("[TCP:%d] No stub's match condition fits this message - closing", port)
			return
		}
		why := "catch-all, no match set"
		if stub.Match != nil {
			why = fmt.Sprintf("match: offset %d == %s", stub.Match.Offset, stub.Match.Value)
		}
		who := fmt.Sprintf("stub %q", stub.Name)
		if stub.Label != "" {
			who = fmt.Sprintf("%s — stub %q", stub.Label, stub.Name)
		}
		log.Printf("[TCP:%d] ▶ Matched %s, %s", port, who, why)

		if stub.ValidateRequest {
			valid, reason := s.validateRequest(data, stub)
			if !valid {
				log.Printf("[TCP:%s] ❌ Validation failed: %s", stub.Name, reason)
				log.Printf("[TCP:%s] Simulating timeout (no response sent)", stub.Name)
				// Just close the connection without sending anything - simulates timeout
				return
			}
			log.Printf("[TCP:%s] ✅ Validation passed", stub.Name)
		}

		if stub.Delay > 0 {
			time.Sleep(time.Duration(stub.Delay) * time.Millisecond)
		}

		responseData, variantName, err := buildResponse(stub, data)
		if err != nil {
			log.Printf("[TCP:%s] Error building response: %v", stub.Name, err)
			return
		}

		if _, err := conn.Write(responseData); err != nil {
			log.Printf("[TCP:%s] Error writing response: %v", stub.Name, err)
			return
		}
		log.Printf("[TCP:%s] Sent %d bytes response (%s) to %s", stub.Name, len(responseData), variantName, clientAddr)

		if stub.CloseAfter {
			return
		}
		// Otherwise loop: keep the connection open and match the next message fresh,
		// since it may belong to a different stub than this one did.
	}
}

// applyOverrides applies the -port and -outcome flags to the loaded TCP stubs in place, so
// a frontend can start the same static config on a chosen port with a chosen result
// instead of generating a different YAML per run. port 0 / outcome "" leave things as is.
// -port needs every stub to share one port (it would silently merge separate listeners
// otherwise); -outcome (case-insensitive) keeps only the variant with that name in each
// stub that has one, and errors if no stub does so a typo isn't silently ignored.
func applyOverrides(stubs []TCPStub, port int, outcome string) error {
	if port != 0 {
		ports := map[int]bool{}
		for _, st := range stubs {
			ports[st.Port] = true
		}
		if len(ports) > 1 {
			return fmt.Errorf("-port %d given but the config's TCP stubs use %d different ports", port, len(ports))
		}
		for i := range stubs {
			stubs[i].Port = port
		}
	}
	if outcome == "" {
		return nil
	}
	pinned := false
	var available []string
	for i := range stubs {
		for _, v := range stubs[i].Responses {
			available = append(available, v.Name)
			if strings.EqualFold(v.Name, outcome) {
				stubs[i].Responses = []TCPResponseVariant{v}
				pinned = true
				break
			}
		}
	}
	if !pinned {
		return fmt.Errorf("-outcome %q matches no response variant (available: %v)", outcome, available)
	}
	return nil
}

// LoadConfig loads stubs from a YAML or JSON file
func LoadConfig(filename string) (*StubConfig, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	log.Printf("Read %d bytes from config file", len(data))

	var config StubConfig

	if strings.HasSuffix(filename, ".yaml") || strings.HasSuffix(filename, ".yml") {
		log.Println("Parsing as YAML...")
		err = yaml.Unmarshal(data, &config)
	} else {
		log.Println("Parsing as JSON...")
		err = json.Unmarshal(data, &config)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	log.Printf("Parsed config: %d HTTP stubs, %d TCP stubs", len(config.HTTPStubs), len(config.TCPStubs))

	return &config, nil
}

func main() {
	configFile := flag.String("config", "", "Path to config file (YAML or JSON)")
	httpPort := flag.Int("http-port", 8080, "HTTP port to listen on")
	tcpPort := flag.Int("port", 0, "Override the listen port of the config's TCP stubs (requires all of them to share one port)")
	outcome := flag.String("outcome", "", "Pin TCP stubs to the response variant with this name (e.g. APROBADA), instead of choosing at random")
	flag.Parse()

	if (*tcpPort != 0 || *outcome != "") && *configFile == "" {
		log.Fatal("-port and -outcome only apply to a -config file's TCP stubs")
	}

	httpServer := NewHTTPStubServer()
	tcpServer := NewTCPStubServer()

	if *configFile != "" {
		config, err := LoadConfig(*configFile)
		if err != nil {
			log.Fatalf("Error loading config: %v", err)
		}

		for _, stub := range config.HTTPStubs {
			httpServer.AddStub(stub)
		}
		log.Printf("Loaded %d HTTP stub(s) from %s", len(config.HTTPStubs), *configFile)

		if err := applyOverrides(config.TCPStubs, *tcpPort, *outcome); err != nil {
			log.Fatalf("Error applying -port/-outcome: %v", err)
		}
		for _, stub := range config.TCPStubs {
			tcpServer.AddStub(stub)
		}
		log.Printf("Loaded %d TCP stub(s) from %s", len(config.TCPStubs), *configFile)
	} else {
		log.Println("No config file provided, using hardcoded stubs")

		// Example HTTP stubs
		httpServer.AddStub(HTTPStub{
			Name:   "health-check",
			Method: "GET",
			Path:   "/health",
			Response: HTTPResponse{
				Status: 200,
				Headers: map[string]string{
					"Content-Type": "application/json",
				},
				Body: `{"status": "ok"}`,
			},
		})
	}

	// Start TCP servers
	if len(tcpServer.stubs) > 0 {
		log.Printf("Starting %d TCP stub server(s)...", len(tcpServer.stubs))
		if err := tcpServer.Start(); err != nil {
			log.Fatalf("Failed to start TCP servers: %v", err)
		}
	}

	// Start HTTP server
	httpAddr := fmt.Sprintf(":%d", *httpPort)
	log.Printf("Starting HTTP stub server on %s", httpAddr)
	log.Printf("Loaded %d HTTP stub(s) and %d TCP stub(s)", len(httpServer.stubs), tcpServer.StubCount())
	log.Println("Server ready to accept requests...")

	if err := http.ListenAndServe(httpAddr, httpServer); err != nil {
		log.Fatalf("HTTP server failed: %v", err)
	}
}
