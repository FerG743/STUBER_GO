package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins (adjust for production)
	},
}

type Bridge struct {
	tcpHost string
	tcpPort int
}

func NewBridge(tcpHost string, tcpPort int) *Bridge {
	return &Bridge{
		tcpHost: tcpHost,
		tcpPort: tcpPort,
	}
}

// handleWebSocket handles WebSocket connections and bridges to TCP
func (b *Bridge) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	wsConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}
	defer wsConn.Close()

	clientAddr := r.RemoteAddr
	log.Printf("[WS] New connection from %s", clientAddr)

	// Connect to TCP backend
	tcpAddr := fmt.Sprintf("%s:%d", b.tcpHost, b.tcpPort)
	tcpConn, err := net.DialTimeout("tcp", tcpAddr, 5*time.Second)
	if err != nil {
		log.Printf("[WS] Failed to connect to TCP backend %s: %v", tcpAddr, err)
		wsConn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("ERROR: Cannot connect to TCP backend: %v", err)))
		return
	}
	defer tcpConn.Close()

	log.Printf("[WS] Connected to TCP backend %s", tcpAddr)

	errChan := make(chan error, 2)

	// WebSocket -> TCP
	go func() {
		for {
			messageType, message, err := wsConn.ReadMessage()
			if err != nil {
				errChan <- fmt.Errorf("WS read error: %v", err)
				return
			}

			var dataToSend []byte
			if messageType == websocket.BinaryMessage {
				dataToSend = message
				log.Printf("[WS->TCP] Sending %d bytes (binary)", len(dataToSend))
			} else {
				hexStr := string(message)
				dataToSend, err = hex.DecodeString(hexStr)
				if err != nil {
					log.Printf("[WS->TCP] Invalid hex: %v", err)
					wsConn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("ERROR: Invalid hex: %v", err)))
					continue
				}
				log.Printf("[WS->TCP] Sending %d bytes (decoded from hex: %s)", len(dataToSend), hexStr[:min(40, len(hexStr))])
			}

			_, err = tcpConn.Write(dataToSend)
			if err != nil {
				errChan <- fmt.Errorf("TCP write error: %v", err)
				return
			}
		}
	}()

	// TCP -> WebSocket
	go func() {
		buffer := make([]byte, 4096)
		for {
			tcpConn.SetReadDeadline(time.Now().Add(30 * time.Second))
			n, err := tcpConn.Read(buffer)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				errChan <- fmt.Errorf("TCP read error: %v", err)
				return
			}

			data := buffer[:n]
			hexData := hex.EncodeToString(data)
			log.Printf("[TCP->WS] Received %d bytes, sending as hex: %s", n, hexData[:min(40, len(hexData))])

			err = wsConn.WriteMessage(websocket.TextMessage, []byte(hexData))
			if err != nil {
				errChan <- fmt.Errorf("WS write error: %v", err)
				return
			}
		}
	}()

	err = <-errChan
	log.Printf("[WS] Connection closed: %v", err)
}

// Health endpoint
func (b *Bridge) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"🍆 toi bien 🍆","service":"websocket-tcp-bridge"}`))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func main() {
	wsPort := flag.Int("ws-port", 9090, "WebSocket server port")
	tcpHost := flag.String("tcp-host", "localhost", "TCP backend host")
	tcpPort := flag.Int("tcp-port", 8001, "TCP backend port")
	flag.Parse()

	bridge := NewBridge(*tcpHost, *tcpPort)

	http.HandleFunc("/ws", bridge.handleWebSocket)
	http.HandleFunc("/health", bridge.handleHealth)

	// Web test interface
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		html := `<!DOCTYPE html>
<html>
<head><title>WebSocket TCP Bridge</title></head>
<body>
<h1>Wasaaaaaaaaaaaaaaa probando tcp o k ase</h1>
<p>WebSocket endpoint: <code id="endpoint"></code></p>
<p>Health check: <a href="/health">/health</a></p>
<hr>
<h2>Test Client</h2>
<textarea id="hexInput" rows="4" cols="80" placeholder="Enter hex string (e.g., 00c4f0f8d3e5...)"></textarea><br>
<button onclick="sendHex()">Send Hex</button>
<button onclick="disconnect()">Disconnect</button>
<pre id="output"></pre>
<script>
let ws = null;

function connect() {
	if (ws) return;
	const protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
	const wsUrl = protocol + '//' + location.host + '/ws';
	document.getElementById('endpoint').textContent = wsUrl;
	console.log('Connecting to:', wsUrl);
	ws = new WebSocket(wsUrl);
	ws.onopen = () => log('✅ Connected to ' + wsUrl);
	ws.onmessage = (e) => log('📨 Received: ' + e.data);
	ws.onerror = (e) => log('❌ Error: ' + e);
	ws.onclose = () => { log('Connection closed'); ws = null; };
}

function sendHex() {
	if (!ws) connect();
	setTimeout(() => {
		const hex = document.getElementById('hexInput').value.trim();
		if (!hex) return log('⚠️ No data entered');
		log('📤 Sending: ' + hex);
		ws.send(hex);
	}, 100);
}

function disconnect() {
	if (ws) { ws.close(); ws = null; }
	log('🔌 Disconnected manually');
}

function log(msg) {
	document.getElementById('output').textContent += msg + '\n';
}
</script>
</body>
</html>`
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(html))
	})

	addr := fmt.Sprintf(":%d", *wsPort)
	log.Printf("🚀 WebSocket-to-TCP bridge starting on %s", addr)
	log.Printf("📡 WebSocket endpoint: ws://localhost:%d/ws", *wsPort)
	log.Printf("🎯 TCP backend: %s:%d", *tcpHost, *tcpPort)
	log.Printf("🌐 Web interface: http://localhost:%d/", *wsPort)

	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
