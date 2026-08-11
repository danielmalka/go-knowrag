# The embedding service

`server.py` is the only embedding runtime this system has. It holds BGE-M3 resident on the GPU and
answers `/health`, `/handshake`, `/tokenize` and `/embed` over plain HTTP on loopback.

It runs on the **operator's host**, not on the VPS. ADR-001 §0 moved it there on 2026-08-08: the VPS
runs Qdrant and nothing else of this system. Everything embeds against this one process — `knowrag
ingest` calls `/tokenize` at every chunk boundary and `/embed` per batch, `knowrag search` and the
MCP server embed one query each.

This file describes how to stand it up on a machine that does not have it yet. It is a record of
what already runs, not a plan: the unit below has been serving a real corpus since S06.

## What it needs

- An NVIDIA GPU with CUDA available to the OS running Python. Measured on an RTX 3060: 1176 MiB of
  VRAM at fp16 (ADR-001 §6.2), so the card is not the constraint — system RAM during load is.
- Python with `torch` and `FlagEmbedding` installed, in a virtualenv of its own.
- Roughly 2.2 GB of disk for the weights, downloaded once into the Hugging Face cache under the
  user's home. First start pays that download; every later start loads from cache in ~11 s.

## Install

1. **Create the virtualenv and install the two dependencies.** They are not in this repository's
   dependency set — this is a Python service beside a Go project — so the interpreter path is
   configuration, not a fixed location.

   ```sh
   python3 -m venv ~/.venvs/knowrag-embedder
   ~/.venvs/knowrag-embedder/bin/pip install torch FlagEmbedding
   ```

2. **Write the environment file** that carries every install-specific path. Copy
   `knowrag-embedder.env.example` to `~/.config/knowrag/embedder.env` and fill in the four values it
   documents: the virtualenv's interpreter, this checkout's path, and the host and port to bind.

   Keep the host on loopback. The service has no authentication of any kind, so an address something
   else can reach hands the GPU to whoever asks.

3. **Install the unit and start it.**

   ```sh
   cp knowrag-embedder.service ~/.config/systemd/user/
   systemctl --user daemon-reload
   systemctl --user enable --now knowrag-embedder
   loginctl enable-linger "$USER"   # without this it dies with the login session
   ```

4. **Point the CLI and the MCP server at it.** Two variables on this same host, each read by a
   different process: `EMBEDDER_ENDPOINT` for `knowrag`, `MCP_EMBEDDER_ENDPOINT` for the MCP server.
   Both are `http://<host>:<port>` with the values from step 2.

## Confirming it is really on the GPU

The startup line names the device it was **asked** for:

```sh
journalctl --user -u knowrag-embedder | grep loading
# loading BAAI/bge-m3@... on cuda:0 ...
```

That line is the request, not the confirmation, and the HTTP surface cannot supply the
confirmation either: `/handshake` reports the seven fields ADR-001 §4 pins — model, revisions,
dimension, normalization, pooling, precision, sparse shape — and **the device is not among them**. A
`precision` of `float16` is a hint, since fp16 on CPU is unusual, but it is not proof.

What proves it is the card:

```sh
nvidia-smi
```

The python process must appear in the process table holding on the order of 1.2 GB of VRAM. If it
does not, the model is on the CPU: the service will still answer every request, correctly, and an
ingestion that took seven minutes will take hours instead.

Confirm the service is up and answering at all with:

```sh
curl -s localhost:7999/health     # {"status":"ok","loaded":true}
curl -s localhost:7999/handshake  # the seven pinned fields
```

## The two memory numbers in the unit

`MemoryHigh=4G` and `MemoryMax=7G` are in `knowrag-embedder.service` because this is the largest
consumer on a WSL2 guest, where an unbounded cgroup holds every page it ever read — measured at
6.2 GB held against 2.5 GB of actual RSS, the difference being page cache of the weights. `MemoryHigh`
does the work by making the kernel reclaim that cache; `MemoryMax` is a backstop above the measured
load peak. The reasoning is written out at the lines themselves, and the numbers live there, not
here.

## When it dies

`Restart=always` with `RestartSec=5` brings it back, and `StartLimitBurst=5` in a 300 s window stops
a crash loop from thrashing the GPU. What the operator sees while it is down, and what to do about
it, is in the runbook's "the embedder is dead" section.
