package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"time"
)

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

// Bound on how long handleConnection will block on a single read - both while idle waiting for
// the next message's header and mid-frame waiting for a payload that never fully arrives (a
// length prefix that overstates what the client actually sends). Without this, io.ReadFull
// blocks forever on either read, leaking one goroutine and one socket per hung client with no
// way to notice or reclaim it. 10s is generous for anything on a LAN; short enough that a burst
// of malformed/truncated frames can't accumulate hung connections.
const connReadTimeout = 10 * time.Second

// malformedMessageFrame is what a connReadTimeout write back to the client: framed the same way
// as every other message on the wire (2-byte big-endian length prefix + payload), so a client
// that's actually parsing frames gets a coherent one back instead of a bare unprefixed string -
// "malformed message" as ASCII, 17 bytes.
var malformedMessageFrame = append([]byte{0x00, 0x11}, []byte("malformed message")...)

// handleReadTimeout distinguishes a connReadTimeout expiring from an ordinary close/EOF (the
// latter is the normal way every other connection already ends, and logging it would just be
// noise) - only a genuine deadline trip gets a log line and a response, since that's the
// "otherwise silent hang" case connReadTimeout exists to make visible instead of leaving the
// client to guess why the connection died.
func handleReadTimeout(conn net.Conn, port int, clientAddr string, err error) {
	netErr, ok := err.(net.Error)
	if !ok || !netErr.Timeout() {
		return
	}
	log.Printf("[TCP:%d] %s sent nothing (or an incomplete frame) for %s - closing hung connection", port, clientAddr, connReadTimeout)
	// Best-effort: the whole point is we're about to close anyway, so a failed write here
	// (client already gone) isn't itself an error worth acting on.
	_, _ = conn.Write(malformedMessageFrame)
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
		// Reset per message: covers both an idle wait for the next header and a stalled
		// mid-frame payload read under one bound, so neither can hang past connReadTimeout.
		if err := conn.SetReadDeadline(time.Now().Add(connReadTimeout)); err != nil {
			log.Printf("[TCP:%d] Couldn't set read deadline for %s, closing: %v", port, clientAddr, err)
			return
		}

		header := make([]byte, 2)
		if _, err := io.ReadFull(reader, header); err != nil {
			handleReadTimeout(conn, port, clientAddr, err)
			return
		}
		payload := make([]byte, binary.BigEndian.Uint16(header))
		if _, err := io.ReadFull(reader, payload); err != nil {
			handleReadTimeout(conn, port, clientAddr, err)
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
