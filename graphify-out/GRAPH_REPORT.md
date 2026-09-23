# Graph Report - STUBER_GO  (2026-09-22)

## Corpus Check
- 7 files · ~9,607 words
- Verdict: corpus is large enough that graph structure adds value.

## Summary
- 73 nodes · 142 edges · 9 communities (7 shown, 2 thin omitted)
- Extraction: 97% EXTRACTED · 3% INFERRED · 0% AMBIGUOUS · INFERRED: 4 edges (avg confidence: 0.85)
- Token cost: 0 input · 0 output

## Graph Freshness
- Built from commit: `0c0a6f26`
- Run `git rev-parse HEAD` and compare to check if the graph is stale.
- Run `graphify update .` after code changes (no API cost).

## Community Hubs (Navigation)
- sim_agent.py
- TCPStubServer
- Program/main.go
- HTTPStub
- Handler
- TestEncodeFieldValue
- README.md
- _stop_locked
- stubserver

## God Nodes (most connected - your core abstractions)
1. `TCPStub` - 15 edges
2. `TCPStubServer` - 9 edges
3. `main()` - 8 edges
4. `Handler` - 8 edges
5. `start()` - 7 edges
6. `HTTPStub` - 6 edges
7. `HTTPStubServer` - 6 edges
8. `buildFields()` - 5 edges
9. `buildResponse()` - 5 edges
10. `_stop_locked()` - 5 edges

## Surprising Connections (you probably didn't know these)
- `TestEncodeFieldValue()` --calls--> `encodeFieldValue()`  [INFERRED]
  Program/encoding_test.go → Program/main.go
- `TestApplyOverrides()` --calls--> `applyOverrides()`  [INFERRED]
  Program/overrides_test.go → Program/main.go

## Import Cycles
- None detected.

## Communities (9 total, 2 thin omitted)

### Community 0 - "sim_agent.py"
Cohesion: 0.39
Nodes (8): _append_log(), _detect_local_ip(), _expire(), main(), SPDH Sim Agent — stdlib-only HTTP control plane for STUBER_GO. Runs on this…, _reader_thread(), _run_slot(), start()

### Community 1 - "TCPStubServer"
Cohesion: 0.33
Nodes (4): net.Conn, main(), NewTCPStubServer(), TCPStubServer

### Community 2 - "Program/main.go"
Cohesion: 0.30
Nodes (14): applyOverrides(), buildFields(), buildResponse(), encodeFieldValue(), LoadConfig(), matchStub(), orDefault(), pickResponseVariant() (+6 more)

### Community 3 - "HTTPStub"
Cohesion: 0.19
Nodes (11): net/http.Request, net/http.ResponseWriter, HTTPResponse, HTTPStub, HTTPStubServer, jsonFieldAt(), jsonFieldMatches(), NewHTTPStubServer() (+3 more)

### Community 4 - "Handler"
Cohesion: 0.29
Nodes (4): Handler, logs_since(), send_test_request(), BaseHTTPRequestHandler

### Community 5 - "TestEncodeFieldValue"
Cohesion: 0.40
Nodes (3): testing.T, TestEncodeFieldValue(), TestApplyOverrides()

### Community 7 - "_stop_locked"
Cohesion: 0.50
Nodes (4): Caller must hold _LOCK for the whole call. Blocks other requests for up to ~5s…, status(), stop(), _stop_locked()

## Knowledge Gaps
- **2 isolated node(s):** `stubserver`, `STUBER_GO`
  These have ≤1 connection - possible missing edges or undocumented components. (Counts symbols only; 10 node(s) total have ≤1 connection when file, concept and rationale nodes are included.)
- **2 thin communities (<3 nodes) omitted from report** — run `graphify query` to explore isolated nodes.

## Suggested Questions
_Questions this graph is uniquely positioned to answer:_

- **Why does `HTTPStubServer` connect `HTTPStub` to `Program/main.go`?**
  _High betweenness centrality (0.079) - this node is a cross-community bridge._
- **Why does `TCPStub` connect `Program/main.go` to `TCPStubServer`?**
  _High betweenness centrality (0.054) - this node is a cross-community bridge._
- **Are the 2 inferred relationships involving `start()` (e.g. with `_expire()` and `_reader_thread()`) actually correct?**
  _`start()` has 2 INFERRED edges - model-reasoned connections that need verification._
- **What connects `stubserver`, `STUBER_GO` to the rest of the system?**
  _2 weakly-connected nodes found - possible documentation gaps or missing edges._