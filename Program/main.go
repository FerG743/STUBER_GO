package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
)

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
