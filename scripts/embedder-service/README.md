# The embedding service

`server.py` is the only embedding runtime this system has. It serves **EmbeddingGemma 2**
(`google/embeddinggemma-2`, text only, dense 768) over plain HTTP on loopback, **on the GPU only**:
no CPU fallback, no sparse vector, no float16.

It runs on the **operator's host**, not on the VPS (the VPS runs Qdrant and nothing else of this
system). `knowrag ingest` calls `/tokenize` at every chunk boundary and `/embed` per batch;
`knowrag search` and the MCP server embed one query each. Ingest and query are two *callers* of one
model.

## How it behaves (owner's decision, 2026-11-15)

- **The process is always up; the model is not.** Starting the service touches no GPU memory. The
  first call that needs the model (`/embed`, `/tokenize`, `/load`) loads it, and after
  `KNOWRAG_EMBEDDER_IDLE_UNLOAD` seconds without one (default 600) it is unloaded and the VRAM comes
  back. `/health` and `/handshake` never load it and never count as use.
- **GPU only.** Without a CUDA device the service refuses to start (exit status **78**, one line in
  the journal) and `/handshake` answers 503. "No CPU fallback" is about the *device*: bf16 (default)
  and fp32 both run on the GPU. **fp16 is refused** at startup, because the model card says not to
  use it and the failure is silent (vectors of the right shape that are wrong).
- **One call on the GPU at a time**, with an interactive query (`kind: "query"`) admitted ahead of
  queued batches (`gpu_gate.py`).
- **Loading, unloading and calls never overlap** (`model_host.py`): a call during a load waits for it,
  a call during an unload waits and then loads again, and a model in use is never unloaded.
- **Ingestion is scheduled** (`knowrag-ingest.timer`, fixed times) and needs no start/stop window.

## Endpoints

All JSON. The Go client is `internal/embed/http.go`.

| Method and path | Request | Response |
|---|---|---|
| `GET /health` | | `{"status":"ok","loaded":bool,"cuda":bool}` |
| `GET /handshake` | | the pinned configuration (below); 503 without a GPU |
| `POST /load` | `{}` | `{"loaded":true}` after loading the model |
| `POST /tokenize` | `{"inputs":[str]}` | `{"counts":[int]}` — the text alone, special tokens included |
| `POST /embed` | `{"inputs":[str],"kind":"query"\|"passage","titles":[str]?}` | `{"dense":[[float]],"tokens":[int]}` |

- **`kind` is required** and must be exactly `query` or `passage`: the task prefix depends on it, and
  a missing or wrong prefix does not fail, it silently degrades search. Anything else is a 400.
- **Prefixes are applied here, never by the caller** (`prompts.py`): a query reads
  `task: search result | query: <text>`, a document `title: <title or "none"> | text: <text>`.
  `titles` (documents only, one per input, `""` means `none`) is what the Go ingest sends. The scheme
  is versioned: `/handshake` reports `prompt_version: "gemma-search-v1"`, and it is part of
  `point_hash`, so changing a prefix string without changing that version makes stored points and
  new queries disagree without an error.
- **`/embed` refuses what it would have to truncate.** The count is of the *prompted* text (prefix and
  title included), special tokens included, against `KNOWRAG_EMBEDDER_MAX_INPUT_TOKENS`; over it is a
  400 naming the index, the tokens and the limit. `tokens` in the response is that same count.
  `/tokenize` counts the text alone, which is what the chunker sizes; the Go profile keeps 128 tokens
  in reserve for the prefix and a title (the prefix is 7 tokens; a 300-character title adds ~70), and
  ingest cuts the title it sends to 96 tokens (`ModelProfile.TitleMaxTokens`), so a chunk filling its
  window plus its prefix always fits; the 400 below is the service's backstop, not the normal path.
- **`/handshake` reports the pinned configuration without loading the model**, because the MCP server
  verifies it at start-up with a short deadline and nothing is searching yet. The load is what makes
  the report true: `st_adapter.load_verified_model` refuses to return a model whose width, dtype,
  pooling or vector norm differ from it.

```
GET /handshake
{"model_id":"google/embeddinggemma-2",
 "model_revision":"f9972ac22e696a105c482202646979e33e4b5a1e",
 "tokenizer_revision":"f9972ac22e696a105c482202646979e33e4b5a1e",
 "dense_dim":768,"normalized":true,"pooling":"mean","precision":"bfloat16",
 "prompt_version":"gemma-search-v1","max_input_tokens":8192}
```

`precision` is `bfloat16` or `float32` (the Go client maps them to `bf16`/`fp32`); there is no
`sparse` key. The revision is the head of the Hugging Face repository as read on 2026-11-15 (last
commit 2026-10-28), the same value as `schema.ModelProfile` on the Go side.

## Settings

Flags win over environment variables, which win over the defaults. The systemd unit reads the
environment from `~/.config/knowrag/embedder.env` (`knowrag-embedder.env.example`).

| Variable | Flag | Default | Meaning |
|---|---|---|---|
| `KNOWRAG_EMBEDDER_HOST` / `_PORT` | `--host` / `--port` | `127.0.0.1` / `7999` | Bind address, **loopback only** (`127.0.0.1`, `::1`; `localhost` is accepted and bound as `127.0.0.1`); any other value is refused (below). |
| `KNOWRAG_EMBEDDER_DEVICE` | `--device` | `cuda:0` | A CUDA device. `cpu` is refused. |
| `KNOWRAG_EMBEDDER_PRECISION` | `--precision` | `bf16` | `bf16` or `fp32`. `fp16` is refused. |
| `KNOWRAG_EMBEDDER_IDLE_UNLOAD` | `--idle-unload` | `600` | Seconds idle before unloading; `0` never unloads. |
| `KNOWRAG_EMBEDDER_BATCH_SIZE` | `--batch-size` | `4` | Texts per forward pass. |
| `KNOWRAG_EMBEDDER_MAX_INPUT_TOKENS` | `--max-input-tokens` | `8192` | Longest input `/embed` accepts, prefix included. Up to 8192, the model's window. |

**Loopback only, by design.** The service has no authentication of any kind, and `/load`, `/embed` and
`/tokenize` run arbitrary work on the GPU for whoever can reach them. It is acceptable only because it
listens on loopback, so that is enforced and not left to a default. The bind always receives a literal
(`localhost` is normalized to `127.0.0.1` first, since a name is resolved by the system at bind time and
could map elsewhere; `::ffff:127.0.0.1` is refused). Any other `--host` (`0.0.0.0`, a LAN address, a name) ends the process with status 78 and a message naming the value. There is no
opt-in to a network address; exposing the service would first need authentication, as its own
decision. A machine that must reach it remotely goes through something that authenticates (an SSH
tunnel, the private network's own access control), not through the bind address.

Refusals (fp16, a CPU device, a non-loopback host, a bad number, no CUDA device) end the process with status 78 and a
message naming the value; the unit lists 78 in `RestartPreventExitStatus` so it is one clear failure
and not a restart loop.

### Batch and token ceiling

Activation memory grows **linearly** with batch × tokens (measured: 4.4× the tokens gave 4.4× the
activations). Measured on 2026-11-15 on an RTX 3060 (12 GB), bf16, torch allocation at the peak of
one `/embed` call, weights included (517 MiB):

| Input length | Batch 4 | Batch 8 | Batch 16 |
|---|---|---|---|
| ~890 tokens | 0.9 GB | 1.4 GB | 2.2 GB |
| ~3,900 tokens (the longest chunk of the trial) | 2.4 GB | 4.3 GB | 7.9 GB |
| ~8,000 tokens (the most `/embed` accepts) | 4.3 GB | 8.1 GB | — |

The trial's 5.2 GB figure was batch 16 over a *mix* of lengths with a few chunks near 3,900 tokens;
the 7.9 GB above is batch 16 with *every* text at 3,900, the worst case for that batch.

Throughput barely depends on the batch for the sizes that matter: 128 chunks of ~370 tokens (the
corpus median) took 2.15 s at batch 4 (60 texts/s) and 1.71 s at batch 32 (75 texts/s). That is why
the default is **4**: the worst input the service accepts peaks at 4.3 GB and typical ingestion
loses about 15%. Raise it only with the table in hand and the card's other users in mind (a desktop
takes about 1 GB). Lowering `MAX_INPUT_TOKENS` also lowers the peak, but **the chunker may emit
chunks up to the profile's window** (8,064 tokens of text, flagged `oversize` above its ceiling), so
a service ceiling below that makes ingestion fail on those chunks; lower the two together or not at
all.

### Memory (host RAM)

`MemoryHigh=3G` and `MemoryMax=5G` in the unit bound **system RAM**, not VRAM. Measured: the process
peaks at about **1.95 GiB** resident after loading and encoding (torch and the CUDA libraries are
most of it) and does not give it back on unload (1.8 GiB after). `MemoryHigh` is what does the work
(above it the kernel reclaims page cache first, which throttles rather than kills); `MemoryMax` is
the backstop above the measured peak.

### What an unload gives back

Measured on the same card with the service's own process (idle unload set to 8 s for the test),
`nvidia-smi` total used:

| State | VRAM used (whole card) |
|---|---|
| before the service started | 759 MiB |
| service up, model not loaded | 759 MiB |
| model loaded (one query served) | 1,436 MiB |
| after the idle unload | 895 MiB |

The process keeps its **CUDA context** (about 136 MiB here) until it exits; everything else, the
517 MiB of weights and the allocator cache, is returned. If that residue ever matters, the plan's
alternative is systemd socket activation, which ends the process and frees everything at the cost of
a slower first search.

Load time: **5.9 s** for the first load in a fresh process, **3.0 s** for a reload (page cache warm);
a first-ever load also downloads about 0.5 GB of weights. The first search after an idle period pays
that, so the MCP client's timeout has to be above it.

## Install

Requires an NVIDIA GPU with CUDA visible to the OS that runs Python (on WSL2, the Windows driver
provides it; `nvidia-smi` must work) and about 6 GB of disk for the virtualenv (the CUDA libraries
are most of it) plus 0.5 GB of weights in the Hugging Face cache.

1. **Create the virtualenv from the pinned requirements** (versions are exact; see the header of
   `requirements.txt` for why each direct dependency is there):

   ```sh
   python3 -m venv ~/.venvs/knowrag-embedder
   ~/.venvs/knowrag-embedder/bin/pip install -r scripts/embedder-service/requirements.txt
   ```

2. **Write the environment file**: copy `knowrag-embedder.env.example` to
   `~/.config/knowrag/embedder.env` and fill in the interpreter, this checkout and the bind address.
3. **Install the units** and the ingest wrapper (the project is shut down; do not enable anything
   until the owner reopens it):

   ```sh
   mkdir -p ~/.config/systemd/user ~/.config/knowrag
   cp scripts/embedder-service/knowrag-embedder.service \
      scripts/embedder-service/knowrag-ingest.service \
      scripts/embedder-service/knowrag-ingest.timer ~/.config/systemd/user/
   cp scripts/embedder-service/scheduled-ingest.sh scripts/embedder-service/ensure-embedder.sh \
      ~/.config/knowrag/
   cp scripts/embedder-service/ingest.env.example ~/.config/knowrag/ingest.env   # then edit it
   systemctl --user daemon-reload
   ```

4. **Point the CLI and the MCP server at it**: `EMBEDDER_ENDPOINT` for `knowrag`,
   `MCP_EMBEDDER_ENDPOINT` for the MCP server, both `http://<host>:<port>`.

When it is time to run, `systemctl --user enable --now knowrag-embedder knowrag-ingest.timer` and
`loginctl enable-linger "$USER"` (without it user units die with the login session).

## Schedule

`knowrag-ingest.timer` runs `scheduled-ingest.sh` at 09:00, 15:00 and 21:00 **Brasilia time** (the
owner's decision of 2026-11-15). The zone is part of the `OnCalendar` expression
(`*-*-* 09,15,21:00:00 America/Sao_Paulo`), so the times do not move with the clock of the host; check
what it resolves to with `systemd-analyze calendar --iterations=4 '<the expression>'`.
`Persistent=true`, so a missed run happens once when the machine is back. The wrapper calls `ensure-embedder.sh` (start the service if needed,
check `/health` reports a CUDA device, `POST /load` and wait for it) and then
`knowrag ingest --vault both --prune --yes`. Nothing is stopped afterwards: the idle unload returns the VRAM.

The scheduled run **always prunes** (the owner's decision of 2026-11-15): the vault is the only source of
truth, so the points of a note deleted from it leave Qdrant on the next run, and the vault's git history
remembers the note. `--yes` is what authorizes that without a prompt; run by hand, `knowrag ingest --prune`
still asks. The wrapper prints one log line with the exit status and the report's own `orphans:` line
(how many notes were pruned and how many points that removed): `scheduled-ingest: exit=0; orphans: 2
note(s) deleted from the vault, 7 point(s), all removed`. The CLI refuses to prune, and exits non-zero, when
it could not read the index, when the run was interrupted, for a note whose file is still on disk, and
for a vault whose scan found **no notes at all** (an unmounted drive looks exactly like a vault whose every
note was deleted), when it would delete more than the **ceiling** (the smaller of 20 notes and 5% of the
vault's indexed notes, at least 3; `KNOWRAG_PRUNE_MAX_NOTES` / `KNOWRAG_PRUNE_MAX_PERCENT` move it), and when the
vault **root is not the one the last successful run used** (canonical path plus the git root commit, kept in
`vault-roots.json` under `KNOWRAG_STATE_DIR` or `$XDG_STATE_HOME/knowrag`). A refusal exits non-zero, deletes
nothing and puts its reason in the `scheduled-ingest:` log line. See the README's "O que fazer quando a poda é
recusada": `knowrag ingest --dry-run`, then a manual `knowrag ingest --prune` (it asks), or
`knowrag ingest --accept-vault-root` after a deliberate move.

## Confirming it is really on the GPU

`/health` reports `"cuda": true` when torch sees a device, and the startup line names the device the
service was configured for. What proves the model is *in* VRAM is the card:

```sh
nvidia-smi        # total used rises by ~0.7 GB after the first call and falls after the idle unload
journalctl --user -u knowrag-embedder | grep -E "loaded|unloaded|reserved"
# model loaded in 5.9s
# cuda memory reserved by this process: 540 MiB -> 0 MiB
# model unloaded: idle for 600s (limit 600s)
```

## When it dies

`Restart=on-failure` with `RestartSec=5` brings it back after a crash, and `StartLimitBurst=5` in a
300 s window stops a crash loop from thrashing the GPU. A refusal to start (status 78) is not
retried: read the journal line, fix the setting or the driver, start it again. What the operator sees
while it is down is in the runbook's "the embedder is dead" section.

## Tests

```sh
make test-embedder                                    # fake model, needs only pytest, no GPU
KNOWRAG_GPU_TESTS=1 <venv>/bin/python -m pytest -q test_gpu_real.py   # real model, in scripts/embedder-service
```

The first needs only `pytest` (`requirements-test.txt`): the service's modules import torch lazily.
The second loads the real model and checks what a fake cannot: that the service's prefixes equal the
model's own `query` and `document` prompts, that an unload returns the VRAM and the next call loads
again, that bf16 and fp32 agree (cosine above 0.98, so allowing both is one vector space), and that
the 128-token reserve covers the prefix with a long title. CI runs neither (it has no Python
toolchain for this and no GPU); run them before changing the service or any pinned version.
