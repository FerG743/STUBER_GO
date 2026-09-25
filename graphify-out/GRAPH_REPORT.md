# Graph Report - STUBER_GO  (2026-09-25)

## Corpus Check
- 10 files · ~10,608 words
- Verdict: corpus is large enough that graph structure adds value.

## Summary
- 72 nodes · 131 edges · 8 communities (6 shown, 2 thin omitted)
- Extraction: 93% EXTRACTED · 7% INFERRED · 0% AMBIGUOUS · INFERRED: 9 edges (avg confidence: 0.85)
- Token cost: 0 input · 0 output

## Graph Freshness
- Built from commit: `4127fdd3`
- Run `git rev-parse HEAD` and compare to check if the graph is stale.
- Run `graphify update .` after code changes (no API cost).

## Community Hubs (Navigation)
- sim_agent.py
- TCPStubServer
- TCPStub
- HTTPStub
- Handler
- encoding.go
- README.md
- stubserver

## God Nodes (most connected - your core abstractions)
1. `TCPStub` - 15 edges
2. `TCPStubServer` - 9 edges
3. `Handler` - 8 edges
4. `start()` - 7 edges
5. `HTTPStub` - 6 edges
6. `HTTPStubServer` - 6 edges
7. `buildFields()` - 5 edges
8. `buildResponse()` - 5 edges
9. `main()` - 5 edges
10. `_stop_locked()` - 5 edges

## Surprising Connections (you probably didn't know these)
- `main()` --calls--> `applyOverrides()`  [INFERRED]
  Program/main.go → Program/config.go
- `TestApplyOverrides()` --calls--> `applyOverrides()`  [INFERRED]
  Program/overrides_test.go → Program/config.go
- `main()` --calls--> `LoadConfig()`  [INFERRED]
  Program/main.go → Program/config.go
- `main()` --calls--> `NewHTTPStubServer()`  [INFERRED]
  Program/main.go → Program/httpserver.go
- `buildResponse()` --references--> `TCPStub`  [EXTRACTED]
  Program/encoding.go → Program/config.go

## Import Cycles
- None detected.

## Communities (8 total, 2 thin omitted)

### Community 0 - "sim_agent.py"
Cohesion: 0.29
Nodes (12): _append_log(), _detect_local_ip(), _expire(), main(), Caller must hold _LOCK for the whole call. Blocks other requests for up to ~5s…, SPDH Sim Agent — stdlib-only HTTP control plane for STUBER_GO. Runs on this…, _reader_thread(), _run_slot() (+4 more)

### Community 1 - "TCPStubServer"
Cohesion: 0.22
Nodes (6): net.Conn, main(), handleReadTimeout(), matchStub(), NewTCPStubServer(), TCPStubServer

### Community 2 - "TCPStub"
Cohesion: 0.44
Nodes (8): applyOverrides(), LoadConfig(), StubConfig, TCPMatch, TCPMeta, TCPResponseField, TCPResponseVariant, TCPStub

### Community 3 - "HTTPStub"
Cohesion: 0.29
Nodes (8): net/http.Request, net/http.ResponseWriter, HTTPResponse, jsonFieldAt(), jsonFieldMatches(), NewHTTPStubServer(), HTTPStub, HTTPStubServer

### Community 4 - "Handler"
Cohesion: 0.29
Nodes (4): Handler, logs_since(), send_test_request(), BaseHTTPRequestHandler

### Community 5 - "encoding.go"
Cohesion: 0.27
Nodes (8): testing.T, buildFields(), buildResponse(), encodeFieldValue(), orDefault(), pickResponseVariant(), TestEncodeFieldValue(), TestApplyOverrides()

## Knowledge Gaps
- **2 isolated node(s):** `stubserver`, `STUBER_GO`
  These have ≤1 connection - possible missing edges or undocumented components. (Counts symbols only; 12 node(s) total have ≤1 connection when file, concept and rationale nodes are included.)
- **2 thin communities (<3 nodes) omitted from report** — run `graphify query` to explore isolated nodes.

## Suggested Questions
_Questions this graph is uniquely positioned to answer:_

- **Why does `TCPStub` connect `TCPStub` to `TCPStubServer`, `encoding.go`?**
  _High betweenness centrality (0.156) - this node is a cross-community bridge._
- **Why does `HTTPStub` connect `HTTPStub` to `TCPStub`?**
  _High betweenness centrality (0.102) - this node is a cross-community bridge._
- **Why does `applyOverrides()` connect `TCPStub` to `TCPStubServer`, `encoding.go`?**
  _High betweenness centrality (0.072) - this node is a cross-community bridge._
- **Are the 2 inferred relationships involving `start()` (e.g. with `_expire()` and `_reader_thread()`) actually correct?**
  _`start()` has 2 INFERRED edges - model-reasoned connections that need verification._
- **What connects `stubserver`, `STUBER_GO` to the rest of the system?**
  _2 weakly-connected nodes found - possible documentation gaps or missing edges._