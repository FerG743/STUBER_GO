package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

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
