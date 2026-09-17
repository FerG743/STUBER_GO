package main

import (
	"bufio"
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
	Name         string                 `yaml:"name" json:"name"`
	Method       string                 `yaml:"method" json:"method"`
	Path         string                 `yaml:"path" json:"path"`
	Headers      map[string]string      `yaml:"headers,omitempty" json:"headers,omitempty"`
	BodyContains string                 `yaml:"body_contains,omitempty" json:"body_contains,omitempty"` // Match if body contains this string
	BodyJSON     map[string]interface{} `yaml:"body_json,omitempty" json:"body_json,omitempty"`         // Match specific JSON fields
	Response     HTTPResponse           `yaml:"response" json:"response"`
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
	Name               string               `yaml:"name" json:"name"`
	Port               int                  `yaml:"port" json:"port"`
	ResponseMessage    string               `yaml:"response_message" json:"response_message"`
	ResponseHex        string               `yaml:"response_hex,omitempty" json:"response_hex,omitempty"` // Hex encoded response
	CloseAfter         bool                 `yaml:"close_after" json:"close_after"`
	Delay              int                  `yaml:"delay,omitempty" json:"delay,omitempty"`
	ValidateRequest    bool                 `yaml:"validate_request" json:"validate_request"`
	ExpectedHexPattern string               `yaml:"expected_hex_pattern,omitempty" json:"expected_hex_pattern,omitempty"` // Regex pattern for hex
	ExpectedPrefix     string               `yaml:"expected_prefix,omitempty" json:"expected_prefix,omitempty"`           // Expected hex prefix
	MinLength          int                  `yaml:"min_length,omitempty" json:"min_length,omitempty"`
	MaxLength          int                  `yaml:"max_length,omitempty" json:"max_length,omitempty"`
	ErrorResponse      string               `yaml:"error_response,omitempty" json:"error_response,omitempty"` // Response on validation failure
	ErrorResponseHex   string               `yaml:"error_response_hex,omitempty" json:"error_response_hex,omitempty"`
	CopyFields         []FieldCopy          `yaml:"copy_fields,omitempty" json:"copy_fields,omitempty"`     // Splice bytes from the request into the response before sending
	RandomFields       []RandomField        `yaml:"random_fields,omitempty" json:"random_fields,omitempty"` // Randomize non-echoed digit fields before sending
	Fields             []TCPResponseField   `yaml:"fields,omitempty" json:"fields,omitempty"`               // Build the response from field specs instead of a captured template — see TCPResponseField
	Length             int                  `yaml:"length,omitempty" json:"length,omitempty"`               // Total response byte length; required when Fields is set
	Responses          []TCPResponseVariant `yaml:"responses,omitempty" json:"responses,omitempty"`         // Multiple possible outcomes (e.g. approved/declined), chosen at random per message
}

// FieldCopy copies a fixed-length byte range from the raw request into the raw response,
// so a canned response_hex can reflect fields (e.g. an MSISDN) that vary per request
// instead of always sending back the same recorded sample. Offsets are byte indexes into
// the exact bytes received/sent on the wire (including any length-prefix bytes).
type FieldCopy struct {
	Name           string `yaml:"name,omitempty" json:"name,omitempty"`
	RequestOffset  int    `yaml:"request_offset" json:"request_offset"`
	ResponseOffset int    `yaml:"response_offset" json:"response_offset"`
	Length         int    `yaml:"length" json:"length"`
}

// RandomField overwrites a fixed-length byte range in the response before it's sent, so
// fields that aren't tied to the request (auth codes, trace numbers) don't come back as
// the exact same recorded value on every single response. With Values set, one entry is
// picked uniformly at random (for fields like a Base24 approval/decline code, where only
// specific values are meaningful); otherwise Length random ASCII digits are generated.
type RandomField struct {
	Name           string   `yaml:"name,omitempty" json:"name,omitempty"`
	ResponseOffset int      `yaml:"response_offset" json:"response_offset"`
	Length         int      `yaml:"length" json:"length"`
	Values         []string `yaml:"values,omitempty" json:"values,omitempty"`
}

// TCPResponseVariant is one possible outcome for a TCP stub (e.g. an approved vs a
// declined reply). When a stub declares more than one, one is picked at random
// (weighted by Weight, default 1) for every message it responds to.
type TCPResponseVariant struct {
	Name            string             `yaml:"name" json:"name"`
	Weight          int                `yaml:"weight,omitempty" json:"weight,omitempty"`
	ResponseMessage string             `yaml:"response_message,omitempty" json:"response_message,omitempty"`
	ResponseHex     string             `yaml:"response_hex,omitempty" json:"response_hex,omitempty"`
	CopyFields      []FieldCopy        `yaml:"copy_fields,omitempty" json:"copy_fields,omitempty"`
	RandomFields    []RandomField      `yaml:"random_fields,omitempty" json:"random_fields,omitempty"`
	Fields          []TCPResponseField `yaml:"fields,omitempty" json:"fields,omitempty"` // Build this variant's response from field specs — see TCPResponseField
	Length          int                `yaml:"length,omitempty" json:"length,omitempty"` // Total response byte length; required when Fields is set
}

// TCPResponseField declaratively builds one byte range of a response — no starting
// captured hex template required. When a stub or variant sets Fields (and Length, the
// total response size), the response is assembled from scratch: allocate Length zero
// bytes, then write each field's value at [Offset:Offset+Length] in order. Fields can
// overlap and later entries win, so one big Source:"fixed" field spanning the whole
// message (equivalent to today's ResponseHex) plus a few small Source:"request"/
// "random" overlays reproduces exactly what CopyFields/RandomFields already do — the
// new mechanism is a superset, not a parallel one. Genuinely new message types can skip
// the base template entirely and declare every field independently instead.
type TCPResponseField struct {
	Name   string `yaml:"name,omitempty" json:"name,omitempty"`
	Offset int    `yaml:"offset" json:"offset"`
	Length int    `yaml:"length" json:"length"`
	// Source: "fixed" (default) uses Value; "request" copies Length bytes from the
	// incoming request at RequestOffset; "random" picks one of Values at random (each
	// must encode to exactly Length bytes), or — if Values is empty — fills with fresh
	// random ASCII digits, same as RandomField.
	Source        string   `yaml:"source,omitempty" json:"source,omitempty"`
	Encoding      string   `yaml:"encoding,omitempty" json:"encoding,omitempty"` // "ascii" (default) or "hex"; applies to Value/Values
	Value         string   `yaml:"value,omitempty" json:"value,omitempty"`
	RequestOffset int      `yaml:"request_offset,omitempty" json:"request_offset,omitempty"`
	Values        []string `yaml:"values,omitempty" json:"values,omitempty"`
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
	if stub.BodyContains != "" || len(stub.BodyJSON) > 0 {
		// Read body
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			return false
		}
		// Restore body for later reads
		r.Body = io.NopCloser(strings.NewReader(string(bodyBytes)))

		bodyStr := string(bodyBytes)

		// Check if body contains string
		if stub.BodyContains != "" && !strings.Contains(bodyStr, stub.BodyContains) {
			return false
		}

		// Check JSON fields match
		if len(stub.BodyJSON) > 0 {
			var requestJSON map[string]interface{}
			if err := json.Unmarshal(bodyBytes, &requestJSON); err != nil {
				return false
			}

			// Check if all specified fields match
			for key, expectedValue := range stub.BodyJSON {
				if !jsonFieldMatches(requestJSON, key, expectedValue) {
					return false
				}
			}
		}
	}

	return true
}

// jsonFieldMatches checks if a JSON field matches expected value (supports nested paths with dots)
func jsonFieldMatches(data map[string]interface{}, path string, expectedValue interface{}) bool {
	keys := strings.Split(path, ".")

	var current interface{} = data
	for _, key := range keys {
		if m, ok := current.(map[string]interface{}); ok {
			current = m[key]
		} else {
			return false
		}
	}

	// Compare values
	return fmt.Sprintf("%v", current) == fmt.Sprintf("%v", expectedValue)
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

// TCPStubServer handles TCP connections
type TCPStubServer struct {
	stubs map[int]*TCPStub
}

// NewTCPStubServer creates a new TCP stub server
func NewTCPStubServer() *TCPStubServer {
	return &TCPStubServer{
		stubs: make(map[int]*TCPStub),
	}
}

// AddStub adds a TCP stub
func (s *TCPStubServer) AddStub(stub TCPStub) {
	s.stubs[stub.Port] = &stub
}

// Start starts all TCP stub listeners
func (s *TCPStubServer) Start() error {
	for port, stub := range s.stubs {
		go func(p int, st *TCPStub) {
			if err := s.listenTCP(p, st); err != nil {
				log.Printf("[TCP] Stub %s error: %v", st.Name, err)
			}
		}(port, stub)
	}
	return nil
}

// listenTCP starts a TCP listener for a specific stub
func (s *TCPStubServer) listenTCP(port int, stub *TCPStub) error {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("failed to listen on port %d: %w", port, err)
	}
	defer listener.Close()

	log.Printf("[TCP] Stub '%s' listening on port %d", stub.Name, port)
	if stub.ValidateRequest {
		log.Printf("[TCP] Stub '%s' has request validation enabled", stub.Name)
	}

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("[TCP] Error accepting connection: %v", err)
			continue
		}

		go s.handleConnection(conn, stub)
	}
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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// applyFieldCopies splices byte ranges from the received request into the response,
// in place, so the response reflects request-specific values instead of always being
// the same recorded sample. Out-of-bounds fields are skipped and logged rather than
// causing a panic or corrupting the response.
func applyFieldCopies(stubName string, fields []FieldCopy, data []byte, responseData []byte) {
	for _, fc := range fields {
		if fc.Length <= 0 {
			continue
		}
		if fc.RequestOffset < 0 || fc.RequestOffset+fc.Length > len(data) {
			log.Printf("[TCP:%s] Skipping copy_field %q: request_offset %d + length %d exceeds received %d bytes",
				stubName, fc.Name, fc.RequestOffset, fc.Length, len(data))
			continue
		}
		if fc.ResponseOffset < 0 || fc.ResponseOffset+fc.Length > len(responseData) {
			log.Printf("[TCP:%s] Skipping copy_field %q: response_offset %d + length %d exceeds response %d bytes",
				stubName, fc.Name, fc.ResponseOffset, fc.Length, len(responseData))
			continue
		}
		copy(responseData[fc.ResponseOffset:fc.ResponseOffset+fc.Length], data[fc.RequestOffset:fc.RequestOffset+fc.Length])
		log.Printf("[TCP:%s] Copied field %q (%d bytes) from request[%d:%d] into response[%d:%d]: %s",
			stubName, fc.Name, fc.Length, fc.RequestOffset, fc.RequestOffset+fc.Length, fc.ResponseOffset, fc.ResponseOffset+fc.Length,
			hex.EncodeToString(responseData[fc.ResponseOffset:fc.ResponseOffset+fc.Length]))
	}
}

const randomFieldDigits = "0123456789"

// applyRandomFields overwrites byte ranges in the response, in place, so fields that
// aren't tied to the request (auth codes, trace numbers, Base24 approval/decline codes)
// vary from response to response instead of always being the exact same recorded value.
// A field with Values set picks one of those values at random (only entries matching
// Length are eligible); otherwise it fills with fresh random ASCII digits. Out-of-bounds
// or unusable fields are skipped and logged rather than corrupting the response.
func applyRandomFields(stubName string, fields []RandomField, responseData []byte) {
	for _, rf := range fields {
		if rf.Length <= 0 {
			continue
		}
		if rf.ResponseOffset < 0 || rf.ResponseOffset+rf.Length > len(responseData) {
			log.Printf("[TCP:%s] Skipping random_field %q: response_offset %d + length %d exceeds response %d bytes",
				stubName, rf.Name, rf.ResponseOffset, rf.Length, len(responseData))
			continue
		}
		if len(rf.Values) > 0 {
			var candidates []string
			for _, v := range rf.Values {
				if len(v) == rf.Length {
					candidates = append(candidates, v)
				} else {
					log.Printf("[TCP:%s] Ignoring random_field %q value %q: length %d != declared length %d",
						stubName, rf.Name, v, len(v), rf.Length)
				}
			}
			if len(candidates) == 0 {
				log.Printf("[TCP:%s] Skipping random_field %q: no values matching length %d", stubName, rf.Name, rf.Length)
				continue
			}
			copy(responseData[rf.ResponseOffset:rf.ResponseOffset+rf.Length], candidates[rand.Intn(len(candidates))])
		} else {
			for i := 0; i < rf.Length; i++ {
				responseData[rf.ResponseOffset+i] = randomFieldDigits[rand.Intn(len(randomFieldDigits))]
			}
		}
		log.Printf("[TCP:%s] Randomized field %q (%d bytes) at response[%d:%d]: %s",
			stubName, rf.Name, rf.Length, rf.ResponseOffset, rf.ResponseOffset+rf.Length,
			string(responseData[rf.ResponseOffset:rf.ResponseOffset+rf.Length]))
	}
}

// encodeFieldValue turns a config-declared value into the exact bytes to write into the
// response — "ascii" writes the string's own bytes, "hex" decodes a hex string — and
// requires the result to be exactly length bytes, matching how RandomField already
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
	default:
		return nil, fmt.Errorf("unsupported encoding %q (use \"ascii\" or \"hex\")", encoding)
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
// are skipped and logged rather than aborting the response, same policy as
// applyFieldCopies/applyRandomFields.
func buildFields(stubName string, fields []TCPResponseField, request []byte, response []byte) {
	for _, f := range fields {
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
				raw = make([]byte, f.Length)
				for i := range raw {
					raw[i] = randomFieldDigits[rand.Intn(len(randomFieldDigits))]
				}
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
// response variant (if the stub declares more than one outcome), then either builds the
// response from Fields (see TCPResponseField — the "on the fly" path, no captured
// template needed) or falls back to the original template + copy_fields + random_fields
// splicing. Returns the response bytes and the name of whichever variant/stub was used,
// for logging.
func buildResponse(stub *TCPStub, data []byte) ([]byte, string, error) {
	variant := pickResponseVariant(stub)

	responseHex := stub.ResponseHex
	responseMessage := stub.ResponseMessage
	copyFields := stub.CopyFields
	randomFields := stub.RandomFields
	fields := stub.Fields
	length := stub.Length
	name := stub.Name

	if variant != nil {
		responseHex = variant.ResponseHex
		responseMessage = variant.ResponseMessage
		copyFields = variant.CopyFields
		randomFields = variant.RandomFields
		fields = variant.Fields
		length = variant.Length
		name = fmt.Sprintf("%s/%s", stub.Name, variant.Name)
	}

	if len(fields) > 0 {
		if length <= 0 {
			return nil, name, fmt.Errorf("fields declared but length is not set (or <= 0)")
		}
		responseData := make([]byte, length)
		buildFields(name, fields, data, responseData)
		return responseData, name, nil
	}

	var responseData []byte
	var err error
	if responseHex != "" {
		responseData, err = hex.DecodeString(responseHex)
		if err != nil {
			return nil, name, fmt.Errorf("decoding response hex: %w", err)
		}
	} else {
		responseData = []byte(responseMessage)
	}

	applyFieldCopies(stub.Name, copyFields, data, responseData)
	applyRandomFields(stub.Name, randomFields, responseData)

	return responseData, name, nil
}

// handleConnection handles a single TCP connection
func (s *TCPStubServer) handleConnection(conn net.Conn, stub *TCPStub) {
	defer conn.Close()

	clientAddr := conn.RemoteAddr().String()
	log.Printf("[TCP:%s %s] Connection from %s", stub.Name, time.Now().Format("15:04:05"), clientAddr)

	// Read incoming data
	reader := bufio.NewReader(conn)

	// For binary protocols, read all available data or up to a buffer size
	buffer := make([]byte, 4096)
	n, err := reader.Read(buffer)
	if err != nil {
		log.Printf("[TCP:%s] Error reading data: %v", stub.Name, err)
		return
	}

	data := buffer[:n]
	hexData := hex.EncodeToString(data)
	log.Printf("[TCP:%s] Received %d bytes: %s", stub.Name, n, hexData)

	// Validate request if enabled
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

	// Apply delay if specified
	if stub.Delay > 0 {
		time.Sleep(time.Duration(stub.Delay) * time.Millisecond)
	}

	// Build and send the stub response
	responseData, variantName, err := buildResponse(stub, data)
	if err != nil {
		log.Printf("[TCP:%s] Error building response: %v", stub.Name, err)
		return
	}

	_, err = conn.Write(responseData)
	if err != nil {
		log.Printf("[TCP:%s] Error writing response: %v", stub.Name, err)
		return
	}

	log.Printf("[TCP:%s] Sent %d bytes response (%s) to %s", stub.Name, len(responseData), variantName, clientAddr)

	// Optionally keep connection open or close it
	if !stub.CloseAfter {
		// Keep connection open for more data
		for {
			n, err := reader.Read(buffer)
			if err != nil {
				break
			}
			data := buffer[:n]
			hexData := hex.EncodeToString(data)
			log.Printf("[TCP:%s] Received: %s", stub.Name, hexData)
			responseData, variantName, err := buildResponse(stub, data)
			if err != nil {
				log.Printf("[TCP:%s] Error building response: %v", stub.Name, err)
				continue
			}
			log.Printf("[TCP:%s] Sending %d bytes response (%s)", stub.Name, len(responseData), variantName)
			conn.Write(responseData)
		}
	}
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
	flag.Parse()

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
	log.Printf("Loaded %d HTTP stub(s) and %d TCP stub(s)", len(httpServer.stubs), len(tcpServer.stubs))
	log.Println("Server ready to accept requests...")

	if err := http.ListenAndServe(httpAddr, httpServer); err != nil {
		log.Fatalf("HTTP server failed: %v", err)
	}
}
