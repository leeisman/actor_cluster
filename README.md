# actor_cluster

A high-concurrency Actor Cluster framework written in pure Go, designed around low-allocation message flow, deterministic routing, and event-sourced state recovery.

This project is intentionally opinionated:

- Pure Go runtime with thin infrastructure adapters
- Actor-style single-threaded state mutation
- Deterministic `(tenant_id, uid)` routing via discovery table
- Cassandra-backed append-only event store
- High-throughput stream + batch transport between client and node
- Spec-driven development with architecture docs checked into the repo

## Why This Exists

General-purpose actor frameworks are convenient, but they often hide costs that matter in high-throughput systems: lock contention, scheduler overhead, memory churn, and opaque runtime behavior.

`actor_cluster` is built for the opposite trade-off:

- explicit control over concurrency behavior
- predictable ownership and routing
- low GC pressure on hot paths
- infrastructure boundaries kept thin and replaceable

The target use case is a stateful, distributed system where a `(tenant_id, uid)` pair owns a single logical actor, while nodes scale horizontally and recover state from persisted events.

## Core Ideas

### 1. Thin actor runtime

`pkg/actor` is deliberately small. It serializes messages through a single goroutine per actor and pushes results into an injected sink. Business logic stays outside the runtime.

### 2. Deterministic routing

`pkg/discovery` owns the only routing algorithm. Both node and client resolve ownership through the same table semantics, so there is one source of truth for slot assignment.

### 3. Event-sourced state

`internal/node` rehydrates actor state from:

- `wallet_snapshots`
- `wallet_events`

Events are append-only. State is rebuilt by loading the latest snapshot and replaying later deltas.

### 4. Stream + batch transport

`pkg/remote` exposes a bidirectional gRPC stream. Clients batch envelopes by destination node, and nodes batch responses back over the same stream to amortize transport overhead.

### 5. Performance-first engineering

The repo follows strict Go performance rules documented in the Memory Bank:

- avoid hot-path `defer`
- prefer fixed ownership over shared mutation
- avoid unnecessary allocations
- preallocate slices and maps where possible
- isolate I/O and synchronization from actor core logic

## Repository Layout

```text
cmd/
  client/        Load generator / gateway-like batch sender
  node/          Actor node entrypoint

internal/
  node/          Business runtime, actor lifecycle, rehydration, batching
  mock/          Test doubles

pkg/
  actor/         Thin actor runtime
  discovery/     Routing table + etcd-based topology resolver
  persistence/   Thin Cassandra adapter
  remote/        gRPC protobuf + transport adapter

deploy/
  infra/         etcd, Cassandra, Kind manifests
  build/         Dockerfile
  *.yaml         Node and client deployment manifests

docs/design/
  Deep design specs for each subsystem

ai/
  Memory Bank, engineering rules, and active context for AI-assisted development
```

## Current Architecture

High-level request flow:

```text
Client/Gateway
  -> discovery.GetNodeIP(tenant_id, uid)
  -> batch by target node
  -> gRPC stream send

Node
  -> validate envelope
  -> ownership check
  -> create/lookup actor
  -> actor handles message in single goroutine
  -> persist event
  -> emit result
  -> batch response over original stream
```

## Quick Start

### Local development

Requirements:

- Go 1.24+
- Docker
- Kind
- kubectl

Start infrastructure:

```bash
make kind-up
make deploy-infra
make init-db
```

Run a node locally against port-forwarded infra:

```bash
make port-forward
go run ./cmd/node
```

### Build images

```bash
make images
```

Load images into Kind and verify:

```bash
make docker-build
```

### Deploy nodes

```bash
make deploy-node
```

After code changes, force a fresh rollout:

```bash
make refresh-nodes
```

### Run the load generator

```bash
make load-test
kubectl logs -l app=load-generator -f
```

## Specs and Design Docs

Detailed subsystem specs live in [`docs/design/`](docs/design).

The current spec set includes:

- discovery and topology resolver
- persistence adapter
- actor runtime
- remote transport
- internal node runtime
- deployment model
- client/load-generator behavior

These docs are not decorative; they are intended to stay in sync with the implementation.

## Development Notes

This repository uses a Memory Bank workflow under [`ai/`](ai), including:

- project vision
- technical decisions
- system design
- Go performance rules
- active context

That means implementation decisions in this repo are expected to align with documented invariants, especially around:

- routing ownership
- graceful shutdown ordering
- actor isolation
- batching behavior
- event rehydration semantics

## Status

This repository already contains:

- a working actor runtime primitive
- deterministic discovery/routing logic
- Cassandra persistence adapter
- gRPC transport contracts
- internal node business runtime
- load generator client
- deployment manifests for local Kind-based testing

The project is still evolving, especially around hardening behavior under failure, transport recovery, and large-scale load validation.

## Philosophy

The design goal is not “generic actor framework first.”

The goal is:

- correctness under ownership rules
- explicit boundaries between runtime and business logic
- low-latency hot paths
- operational clarity when things fail

If you want a small, understandable, performance-conscious distributed actor foundation in Go, this repo is aiming squarely at that space.
