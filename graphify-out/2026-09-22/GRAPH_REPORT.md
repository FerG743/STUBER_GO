# Graph Report - STUBER_GO  (2026-09-17)

## Corpus Check
- 9 files · ~7,448 words
- Verdict: corpus is large enough that graph structure adds value.

## Summary
- 72 nodes · 135 edges · 12 communities (6 shown, 2 thin omitted)
- Extraction: 99% EXTRACTED · 1% INFERRED · 0% AMBIGUOUS · INFERRED: 2 edges (avg confidence: 0.85)
- Token cost: 0 input · 0 output

## Graph Freshness
- Built from commit: `2712e1a4`
- Run `git rev-parse HEAD` and compare to check if the graph is stale.
- Run `graphify update .` after code changes (no API cost).

## Community Hubs (Navigation)
- sim_agent.py
- TCPStub
- Program/main.go
- HTTPStub
- Handler
- wsbridge/main.go
- README.md
- stubserver

## God Nodes (most connected - your core abstractions)
1. `TCPStub` - 13 edges
2. `TCPStubServer` - 8 edges
3. `Handler` - 8 edges
4. `buildResponse()` - 7 edges
5. `start()` - 7 edges
6. `HTTPStub` - 6 edges
7. `TCPResponseVariant` - 6 edges
8. `HTTPStubServer` - 6 edges
9. `main()` - 6 edges
10. `buildFields()` - 5 edges

## Surprising Connections (you probably didn't know these)
- `StubConfig` --references--> `HTTPStub`  [EXTRACTED]
  Program/main.go → Program/main.go  _Bridges community 1 → community 3_
- `buildResponse()` --references--> `TCPStub`  [EXTRACTED]
  Program/main.go → Program/main.go  _Bridges community 1 → community 2_

## Import Cycles
- None detected.

## Communities (12 total, 2 thin omitted)

### Community 0 - "sim_agent.py"
Cohesion: 0.29
Nodes (12): _append_log(), _detect_local_ip(), _expire(), main(), Caller must hold _LOCK for the whole call. Blocks other requests for up to ~5s…, SPDH Sim Agent — stdlib-only HTTP control plane for STUBER_GO. Runs on this…, _reader_thread(), _run_slot() (+4 more)

### Community 1 - "TCPStub"
Cohesion: 0.28
Nodes (8): net.Conn, LoadConfig(), main(), NewHTTPStubServer(), NewTCPStubServer(), StubConfig, TCPStub, TCPStubServer

### Community 2 - "Program/main.go"
Cohesion: 0.33
Nodes (11): FieldCopy, applyFieldCopies(), applyRandomFields(), buildFields(), buildResponse(), encodeFieldValue(), orDefault(), pickResponseVariant() (+3 more)

### Community 3 - "HTTPStub"
Cohesion: 0.29
Nodes (6): net/http.Request, net/http.ResponseWriter, HTTPResponse, HTTPStub, HTTPStubServer, jsonFieldMatches()

### Community 4 - "Handler"
Cohesion: 0.29
Nodes (4): Handler, logs_since(), send_test_request(), BaseHTTPRequestHandler

### Community 5 - "wsbridge/main.go"
Cohesion: 0.60
Nodes (3): main(), NewBridge(), Bridge

## Knowledge Gaps
- **2 isolated node(s):** `stubserver`, `STUBER_GO`
  These have ≤1 connection - possible missing edges or undocumented components. (Counts symbols only; 14 node(s) total have ≤1 connection when file, concept and rationale nodes are included.)
- **2 thin communities (<3 nodes) omitted from report** — run `graphify query` to explore isolated nodes.

## Suggested Questions
_Questions this graph is uniquely positioned to answer:_

- **Why does `HTTPStubServer` connect `HTTPStub` to `TCPStub`, `Program/main.go`?**
  _High betweenness centrality (0.082) - this node is a cross-community bridge._
- **Why does `Bridge` connect `wsbridge/main.go` to `HTTPStub`?**
  _High betweenness centrality (0.060) - this node is a cross-community bridge._
- **Why does `TCPStub` connect `TCPStub` to `Program/main.go`?**
  _High betweenness centrality (0.040) - this node is a cross-community bridge._
- **Are the 2 inferred relationships involving `start()` (e.g. with `_expire()` and `_reader_thread()`) actually correct?**
  _`start()` has 2 INFERRED edges - model-reasoned connections that need verification._
- **What connects `stubserver`, `STUBER_GO` to the rest of the system?**
  _2 weakly-connected nodes found - possible documentation gaps or missing edges._