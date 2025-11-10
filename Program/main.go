package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
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
	Name               string `yaml:"name" json:"name"`
	Port               int    `yaml:"port" json:"port"`
	ResponseMessage    string `yaml:"response_message" json:"response_message"`
	ResponseHex        string `yaml:"response_hex,omitempty" json:"response_hex,omitempty"` // Hex encoded response
	CloseAfter         bool   `yaml:"close_after" json:"close_after"`
	Delay              int    `yaml:"delay,omitempty" json:"delay,omitempty"`
	ValidateRequest    bool   `yaml:"validate_request" json:"validate_request"`
	ExpectedHexPattern string `yaml:"expected_hex_pattern,omitempty" json:"expected_hex_pattern,omitempty"` // Regex pattern for hex
	ExpectedPrefix     string `yaml:"expected_prefix,omitempty" json:"expected_prefix,omitempty"`           // Expected hex prefix
	MinLength          int    `yaml:"min_length,omitempty" json:"min_length,omitempty"`
	MaxLength          int    `yaml:"max_length,omitempty" json:"max_length,omitempty"`
	ErrorResponse      string `yaml:"error_response,omitempty" json:"error_response,omitempty"` // Response on validation failure
	ErrorResponseHex   string `yaml:"error_response_hex,omitempty" json:"error_response_hex,omitempty"`
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

	// Send stub response
	var responseData []byte
	if stub.ResponseHex != "" {
		// Decode hex response
		responseData, err = hex.DecodeString(stub.ResponseHex)
		if err != nil {
			log.Printf("[TCP:%s] Error decoding response hex: %v", stub.Name, err)
			return
		}
	} else {
		responseData = []byte(stub.ResponseMessage)
	}

	_, err = conn.Write(responseData)
	if err != nil {
		log.Printf("[TCP:%s] Error writing response: %v", stub.Name, err)
		return
	}

	log.Printf("[TCP:%s] Sent %d bytes response to %s", stub.Name, len(responseData), clientAddr)

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
