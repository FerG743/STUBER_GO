package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

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
