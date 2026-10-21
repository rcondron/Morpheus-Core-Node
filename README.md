# Morpheus Core Node

[![CI](https://github.com/rcondron/Morpheus-Core-Node/actions/workflows/ci.yml/badge.svg)](https://github.com/rcondron/Morpheus-Core-Node/actions/workflows/ci.yml)

`morcore` is a terminal-only compute routing node for the [Morpheus](https://mor.org) decentralized AI
marketplace on BASE. It gives your AI agents one OpenAI-compatible endpoint, backed by independent
compute providers and paid for in MOR. It can also sell your own model server's compute to the network.

It is a lightweight alternative to the reference
[Morpheus-Lumerin-Node](https://github.com/MorpheusAIs/Morpheus-Lumerin-Node) proxy-router and speaks
the same wire protocol and contracts.

- **Fast and light.** One static ~12 MB binary with no runtime dependencies and no database. It uses about 20 MB
  of memory under load, and CLI commands start in milliseconds.
- **Windows, macOS and Linux** on amd64 and arm64, with checksum-verified one-line installers.
- **Seamless onboarding.** A guided setup creates or imports a wallet, picks a network and RPC endpoint, and
  issues your first agent API key in about a minute. Afterwards, `morcore` opens a live dashboard.
- **Built for agents.** `/v1/chat/completions` (streaming and non-streaming), `/v1/embeddings` and `/v1/models`
  work with any OpenAI SDK or agent framework. Point `OPENAI_BASE_URL` at the node and you're done.
- **API keys for every agent.** Each key can have model allowlists, requests-per-minute limits, concurrency
  limits, daily token quotas, expiry and scopes. Keys are stored only as hashes, and changes apply live.
- **Multi-session concurrency.** Each model gets a pool of marketplace sessions:
  - Requests run in parallel across providers, and the pool scales up automatically under load.
  - If a provider fails before responding, the request fails over to another provider.
  - Sessions are renewed before they expire and closed when idle, and unused MOR comes back automatically.
- **Provider mode.** Sell inference from vLLM, llama.cpp, Ollama, SGLang, LiteLLM or any OpenAI-compatible server.
- **Wire-compatible.** Verified in both directions against the reference implementation's own code (see
  [Interop](#interop-with-the-reference-node)).

**Contents:** [Install](#install) · [Quick start](#quick-start) · [Agent API](#agent-api) ·
[API keys](#api-keys) · [How it works](#how-it-works) · [Running a provider](#running-a-provider) ·
[Commands](#commands) · [Configuration](#configuration) · [Running as a service](#running-as-a-service) ·
[Security](#security) · [Architecture](#architecture) · [Development](#development) ·
[Limitations](#limitations)

## Install

Pre-built binaries are attached to each [GitHub release](https://github.com/rcondron/Morpheus-Core-Node/releases).

**macOS / Linux**

```sh
curl -fsSL https://raw.githubusercontent.com/rcondron/Morpheus-Core-Node/main/scripts/install.sh | sh
```

**Windows (PowerShell)**

```powershell
irm https://raw.githubusercontent.com/rcondron/Morpheus-Core-Node/main/scripts/install.ps1 | iex
```

Both installers detect your OS and CPU, verify the SHA-256 checksum, and put `morcore` on your PATH. To pin a
release or choose the directory, set `MORCORE_VERSION` or `MORCORE_INSTALL_DIR`:

```sh
curl -fsSL https://raw.githubusercontent.com/rcondron/Morpheus-Core-Node/main/scripts/install.sh | MORCORE_VERSION=v0.1.0 sh
```

**From source** (Go 1.26+)

```sh
go install github.com/rcondron/Morpheus-Core-Node/cmd/morcore@latest
# or: git clone … && make build   →  bin/morcore
```

**Docker:** see [Docker](#docker).

## Quick start

```sh
morcore
```

The first run starts the setup wizard:

1. **Role.** *Agent gateway* (use models), *Provider* (sell compute), or both.
2. **Network.** BASE mainnet, or BASE Sepolia testnet to try things out.
3. **RPC endpoint.** The public BASE endpoints work out of the box, with failover between them. Your own
   endpoint (Alchemy, Infura, QuickNode…) is faster and isn't rate-limited.
4. **Wallet.** Create a new one (with a 12-word recovery phrase) or import a recovery phrase or private key.
   When importing a phrase, you pick the account from a list showing each account's balances.
5. **Password.** Encrypts the wallet keystore. Optionally, the node can store it so it starts unattended.
6. **Sessions.** Choose the session length, parallel sessions per model and an optional MOR spending cap.
7. **First API key.** Shown once; copy it.

Fund the wallet address the wizard prints with:

- **ETH on BASE** for gas. A few dollars lasts a long time.
- **MOR** for session stakes. MOR is escrowed while a session is open and comes back when it closes.

Then:

```sh
morcore            # start the node with a live dashboard (press q to stop)
morcore chat       # talk to a marketplace model right in the terminal
```

Point any OpenAI-compatible agent at it:

```sh
export OPENAI_BASE_URL=http://127.0.0.1:8082/v1
export OPENAI_API_KEY=sk-mor-...
```

For scripts and containers, `morcore init --yes` does the same setup without prompts (see `morcore help init`):

```sh
MORCORE_PASSWORD='a-strong-password' morcore init --yes --network testnet \
  --password-env MORCORE_PASSWORD --api-key my-agent
```

## Agent API

The node serves an OpenAI-compatible API, by default on `http://127.0.0.1:8082`.

| Method | Path                   | Purpose                                                 |
|--------|------------------------|---------------------------------------------------------|
| GET    | `/v1/models`           | Models with active provider offers (that your key may use) |
| GET    | `/v1/models/{model}`   | One model, by name or `0x…` id                          |
| POST   | `/v1/chat/completions` | Chat completions, streaming (SSE) or not                |
| POST   | `/v1/embeddings`       | Embeddings                                              |
| GET    | `/healthz`             | Liveness (no auth)                                      |

Authenticate with `Authorization: Bearer sk-mor-…` (or `X-Api-Key: sk-mor-…`). Requests and responses use the
standard OpenAI formats, so existing clients work unchanged.

**curl**

```sh
curl http://127.0.0.1:8082/v1/chat/completions \
  -H "Authorization: Bearer $OPENAI_API_KEY" -H "Content-Type: application/json" \
  -d '{"model": "llama-3.3-70b", "messages": [{"role": "user", "content": "Hello!"}], "stream": true}'
```

**Python** (`pip install openai`)

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:8082/v1")  # api_key from $OPENAI_API_KEY

reply = client.chat.completions.create(
    model="llama-3.3-70b",
    messages=[{"role": "user", "content": "Explain MOR staking in one sentence."}],
)
print(reply.choices[0].message.content)

stream = client.chat.completions.create(
    model="llama-3.3-70b",
    messages=[{"role": "user", "content": "Write a haiku about routers."}],
    stream=True,
)
for chunk in stream:
    if chunk.choices:
        print(chunk.choices[0].delta.content or "", end="", flush=True)
```

**TypeScript** (`npm install openai`)

```ts
import OpenAI from "openai";

const client = new OpenAI(); // reads OPENAI_BASE_URL and OPENAI_API_KEY

const stream = await client.chat.completions.create({
  model: "llama-3.3-70b",
  messages: [{ role: "user", content: "Hello from TypeScript" }],
  stream: true,
});
for await (const chunk of stream) {
  process.stdout.write(chunk.choices[0]?.delta?.content ?? "");
}
```

LangChain, LlamaIndex, the Vercel AI SDK, CrewAI, AutoGen and other OpenAI-compatible tools work the same way:
set the base URL and API key.

**Model names.** Use the names from `morcore models` or `GET /v1/models`. If several marketplace listings share
a name, the one with the most offers keeps it and the others get an id suffix (`name@1a2b3c4d`). A model's
`0x…` id always works.

**Routing headers.** Every response carries `X-Morpheus-Session` and `X-Morpheus-Provider`, which tell you
which session and provider served it, plus `X-Request-Id`. To pin a request to a specific session (one opened
with `morcore sessions open`), send its id in a `session_id` header (the reference node's header) or in
`X-Morpheus-Session`.

**Errors** are OpenAI-shaped (`{"error": {"message", "type", "code"}}`), so SDK retry logic works:

| Status | `code`                                         | Meaning                                                    |
|--------|------------------------------------------------|------------------------------------------------------------|
| 401    | `missing_api_key`, `invalid_api_key`           | No key, or a revoked, expired or unknown key               |
| 403    | `insufficient_scope`, `model_not_allowed`      | The key may not use this endpoint or model                  |
| 404    | `model_not_found`, `session_not_found`         | Unknown model, or the pinned session is gone                |
| 429    | `rate_limit_exceeded`, `insufficient_quota`    | Per-key RPM, concurrency or daily token limit (`Retry-After` is set) |
| 503    | `queue_timeout`, `no_capacity`, `no_provider`  | All sessions busy (`Retry-After: 5`), session caps reached, or no provider reachable |
| 502    | `provider_error`, `session_error`              | Providers failed after failover, or a session could not be opened |

A provider's own model-server error (for example, its backend's 429) is passed through with its original status.

## API keys

Every agent should get its own key. Secrets look like `sk-mor-…`. They are shown once, and only a SHA-256 hash
is stored.

```sh
morcore keys create research-agent \
    --models llama-3.3-70b,qwen3-235b \
    --rpm 60 --concurrency 4 --daily-tokens 2000000 --expires 30d
morcore keys list                       # usage, limits, last use
morcore keys show research-agent
morcore keys update research-agent --rpm 120 --models ""   # "" = all models
morcore keys rotate research-agent      # new secret; same settings and usage
morcore keys revoke research-agent
```

| Option           | Effect                                                                    |
|------------------|---------------------------------------------------------------------------|
| `--models`       | Allowlist of model names or ids (default: all)                            |
| `--rpm`          | Requests per minute (0 = unlimited)                                       |
| `--concurrency`  | Requests in flight at once (0 = unlimited)                                |
| `--daily-tokens` | Prompt + completion tokens per UTC day (0 = unlimited)                    |
| `--expires`      | Lifetime: `12h`, `30d`, … (default: never)                                |
| `--scopes`       | `inference` (default) · `sessions` (view/open/close sessions) · `admin` (manage keys) |

Key commands work whether or not the node is running, and a running node picks up changes within a second.

To manage keys from code, use the admin API on the same port. It accepts the token in
`<home>/admin.token` or any key with the `admin` scope:

```sh
curl -X POST http://127.0.0.1:8082/admin/keys -H "Authorization: Bearer $(cat ~/.config/morcore/admin.token)" \
  -H "Content-Type: application/json" \
  -d '{"name": "worker-7", "models": ["llama-3.3-70b"], "rpm": 30, "expiresIn": "7d"}'
```

| Admin endpoint | Scope |
|---|---|
| `GET /admin/keys` · `POST /admin/keys` · `PATCH /admin/keys/{name\|id}` · `DELETE /admin/keys/{name\|id}` · `POST /admin/keys/{name\|id}/rotate` | admin |
| `GET /admin/status` · `GET /admin/events` · `GET /admin/models` · `GET /admin/models/{id}/bids` | sessions |
| `GET /admin/sessions` · `POST /admin/sessions` · `DELETE /admin/sessions/{id}` · `POST /admin/stakes/withdraw` | sessions |

## How it works

**The marketplace.**
- Morpheus runs a compute marketplace on BASE. Providers register an endpoint and post **bids**, each a price
  per second for serving a model.
- To use a model, a consumer opens a **session** with a provider by staking MOR in the marketplace contract.
- Before opening a session, the provider signs an approval. While the session runs, every request and response
  is signed. Responses are encrypted to the consumer's key.

**Paying for sessions.**
- By default, sessions use Morpheus' stake-for-compute model. Your MOR is escrowed, not spent, and providers are
  paid from the protocol's compute budget.
- When a session closes, your stake is released. Any part placed on hold becomes withdrawable after the next
  UTC day, and the node withdraws it automatically (`auto_withdraw`).
- Set `direct_payment = true` to pay providers directly from your stake instead.

`morcore bids <model>` shows the offers for a model, ranked, with the MOR each one would escrow.

**Choosing a provider.** The node rates bids with the network's default scorer. The score combines throughput,
time to first token, total time served, success rate and provider stake, divided by price. The node then pings
the top eight candidates in parallel with a signed health check. `sessions.health_policy` controls how health affects the
choice:

- `permissive`: use any reachable provider.
- `preferred`: try healthy providers first.
- `strict`: use only healthy providers.

You can also pin or exclude providers with `provider_allowlist` and `provider_denylist`.

**Concurrency.** A provider session serves one request at a time, so the node keeps a pool of sessions per
model:

1. A request takes a free session. If every session is busy for 2 seconds, the node opens another, up to
   `max_per_model`, `max_total` and the MOR cap `max_stake`.
2. A request that waits longer than `queue_timeout` gets `503 queue_timeout`.
3. If a provider fails before sending any output, the request retries on a different provider; your agent
   never sees the failure.
4. Busy sessions are renewed before they end (`renew_before`). Idle ones close after `idle_timeout`. Failing
   ones are retired.

Chain writes go through a single wallet lock, so concurrent opens and closes never collide on nonces.

**Shutdown and restart.**
- On stop (`q`, Ctrl+C or SIGTERM), the node closes its sessions so unused stake returns to the wallet. It uses
  the provider's signed usage receipt when available.
- On the next start, it reconciles with the chain: sessions that are still open are adopted, and expired ones
  are closed.

## Running a provider

1. Run an OpenAI-compatible model server, for example vLLM
   (`vllm serve meta-llama/Llama-3.3-70B-Instruct`), llama.cpp `llama-server`, Ollama, SGLang or LiteLLM.
2. Run `morcore init` and choose **Provider** (or both). The wizard asks for:
   - your server's chat-completions URL and model name;
   - the public `host:3333` that consumers will reach;
   - the marketplace model listing you serve. Joining an existing listing beats creating a duplicate.
3. Open TCP port **3333** in your firewall. This is the only port that should be public.
4. Register and post a bid:

   ```sh
   morcore provider register                          # stakes the provider minimum
   morcore provider bid llama-3.3-70b --price 0.5     # MOR per hour
   morcore start
   ```

5. Check on things with `morcore provider status`. Earnings are paid when consumers close sessions. For
   sessions that ended without a close, collect them with `morcore provider claim`.

The node relays requests and streams to your backend, signs every response and encrypts it to the consumer. It
also caps concurrent sessions per model (`max_sessions`), probes backend health (reported in pings), and returns
signed usage receipts. Several models can be served from one node:

```toml
[[provider.models]]
  model_id = "0x…"                                  # marketplace model id (morcore models --all)
  name = "llama-3.3-70b"
  api_url = "http://localhost:8000/v1/chat/completions"
  backend_model = "meta-llama/Llama-3.3-70B-Instruct"
  api_key = ""                                       # if your server needs one
  max_sessions = 8                                   # 0 = unlimited
```

`morcore provider model-register <name>` creates a new listing. `unbid` and `deregister` withdraw bids and
stake.

## Commands

| Command | |
|---|---|
| `morcore` | First run: setup wizard. Afterwards: start the node with the dashboard |
| `morcore init` | Re-run setup (`--yes` for non-interactive; `morcore help init`) |
| `morcore start [--headless]` | Start the node; `--headless` for plain logs (services, containers) |
| `morcore status` | Node, wallet, balances, sessions and traffic |
| `morcore chat [model]` | Interactive streaming chat in the terminal |
| `morcore models [--all]` | Marketplace models with offers (`--all`: every listing) |
| `morcore bids <model>` | Ranked provider offers with score, price and stake per session |
| `morcore keys list\|create\|show\|update\|rotate\|revoke` | Agent API keys |
| `morcore sessions list\|open\|close` | Inspect sessions; open one now; close one or `all` |
| `morcore wallet show\|withdraw\|export` | Balances, withdraw released stake, print the private key |
| `morcore provider status\|register\|bid\|unbid\|claim\|model-register\|deregister` | Provider operations |
| `morcore doctor` | Check configuration, wallet, RPC, contracts, balances and ports |
| `morcore config [show]` | Where configuration lives, or print it |
| `morcore version` | Version, commit, build date |

Global flags work anywhere on the command line: `--json` (machine-readable output), `--home <dir>`,
`--no-color`. Commands that need the node talk to the running instance when there is one; otherwise they work
directly against the chain.

## Configuration

Everything lives in one directory. Override it with `--home` or `MORCORE_HOME`.

| OS      | Default home                                   |
|---------|------------------------------------------------|
| Linux   | `~/.config/morcore`                            |
| macOS   | `~/Library/Application Support/morcore`        |
| Windows | `%AppData%\morcore`                            |

| File | Contents |
|---|---|
| `config.toml` | Settings, written by the wizard and safe to edit (`morcore config show`) |
| `keystore.json` | Wallet, Web3 Secret Storage v3 (scrypt), compatible with geth and other Ethereum tools |
| `apikeys.json` | API key metadata, hashes and usage |
| `admin.token` | Token for the local admin API |
| `sessions.json` · `provider_state.json` | Session state for restarts |
| `wallet.password` | Only if you chose unattended start |

All files are created with owner-only permissions. The defaults are:

```toml
network = "mainnet"            # mainnet | testnet
role = "consumer"              # consumer | provider | both

[chain]
  rpc_urls = ["https://mainnet.base.org", "https://base-rpc.publicnode.com", …]  # in order, with failover
  legacy_tx = false                          # true for RPCs without EIP-1559

[gateway]
  enabled = true
  listen = "127.0.0.1:8082"    # keep private
  cors_origins = []            # browser origins allowed to call the API

[sessions]
  duration = "1h0m0s"          # length of new sessions (min 5m)
  max_per_model = 2            # parallel sessions (= parallel requests) per model
  max_total = 6
  max_stake = "0"              # cap on MOR escrowed at once ("0" = no cap)
  idle_timeout = "30m0s"
  renew_before = "2m0s"
  close_on_shutdown = true
  direct_payment = false
  provider_timeout = "2m0s"    # max wait for a provider's next chunk
  queue_timeout = "2m0s"       # max wait for a free session
  health_policy = "permissive" # permissive | preferred | strict
  provider_allowlist = []
  provider_denylist = []
  auto_withdraw = true

[provider]
  enabled = false
  listen = "0.0.0.0:3333"
  public_endpoint = ""         # host:port registered on-chain

[log]
  level = "info"               # debug | info | warn | error
  format = "auto"              # auto | pretty | json
```

| Environment variable | |
|---|---|
| `MORCORE_HOME` | Node home directory |
| `MORCORE_PASSWORD` | Wallet password (unattended start) |
| `MORCORE_PRIVATE_KEY` | Use this key instead of the keystore (containers, secret managers) |
| `MORCORE_RPC_URLS` | Comma-separated RPC URLs, overriding `chain.rpc_urls` |
| `MORCORE_GATEWAY_LISTEN` | Overrides `gateway.listen` |
| `MORCORE_LOG_LEVEL` | Overrides `log.level` |
| `MORCORE_THEME` | `light` or `dark`, if your terminal's background isn't detected |
| `NO_COLOR` | Disable colors |

## Running as a service

The node needs the wallet password without a terminal. Use one of:

- `MORCORE_PASSWORD`;
- the unattended-start option in the wizard (or `init --auto-unlock`);
- `MORCORE_PRIVATE_KEY`.

Give it time to close sessions on stop. Shutdown is bounded at about 2 minutes, and anything left open is
reconciled on the next start.

### systemd

```ini
# /etc/systemd/system/morcore.service
[Unit]
Description=Morpheus Core Node
After=network-online.target
Wants=network-online.target

[Service]
User=morcore
Environment=MORCORE_HOME=/var/lib/morcore
EnvironmentFile=/etc/morcore/env        # MORCORE_PASSWORD=...
ExecStart=/usr/local/bin/morcore start --headless
Restart=on-failure
TimeoutStopSec=150

[Install]
WantedBy=multi-user.target
```

### Docker

```sh
docker build -t morcore .
docker run --rm -it -v morcore:/data -e MORCORE_PASSWORD=... morcore init --yes \
    --network mainnet --listen 0.0.0.0:8082 --api-key my-agent --password-env MORCORE_PASSWORD
docker run -d --name morcore --stop-timeout 150 -v morcore:/data -e MORCORE_PASSWORD=... \
    -p 127.0.0.1:8082:8082 morcore
```

The image is about 18 MB: distroless, non-root, with state in the `/data` volume. Inside the container the API
listens on `0.0.0.0`, so publish it only on `127.0.0.1` or a private network. For provider nodes, also publish
`-p 3333:3333`.

## Security

- **Keep the agent API private.** Port 8082 controls a funded wallet. It binds to `127.0.0.1` by default. If
  agents run elsewhere, use a private network, VPN or SSH tunnel, or a TLS reverse proxy. Only the provider port
  3333 is meant to be public, and only on provider nodes.
- **Prompts are visible to providers.** Prompts reach the provider signed but not encrypted, because the
  provider has to read them to run the model. Responses come back encrypted to your node. Choose providers you
  trust for sensitive data (`provider_allowlist`).
- **Limit what a leaked key can do.** Give each agent its own key with the smallest model list and limits that
  work. Use `max_stake` to cap how much MOR can be escrowed at once.
- **Back up your recovery phrase.** The keystore is encrypted with your password. `wallet.password` (unattended
  start) and `morcore wallet export` trade safety for convenience. Protect the host accordingly.
- Wallet keys are never sent anywhere. The node only signs locally and sends transactions to your RPC.

## Architecture

```
  Consumer role                                      Provider role

  AI agents                                          other nodes (consumers)
     │  OpenAI API · HTTP :8082                         │  MOR-RPC · TCP :3333
     ▼                                                  ▼
  gateway ── API keys, limits, quotas                provider ── verify, sign, encrypt, meter
     │                                                  │
     ▼                                                  ▼
  session manager ── pools, failover, renewal        your model server (OpenAI-compatible)
     │  MOR-RPC · TCP
     ▼
  provider nodes ──▶ models

  Shared: marketplace (catalog, bid rating) · chain client (BASE JSON-RPC with failover,
  Multicall3, transactions signed behind one wallet lock) · wallet · event bus → dashboard
```

| Package | |
|---|---|
| `internal/morrpc` | MOR-RPC messages, signing (secp256k1/keccak), ECIES, TCP transport |
| `internal/chain` | JSON-RPC client with failover, typed contract reads, Multicall3 batching, EIP-1559 transactions |
| `internal/marketplace` | Model catalog, bid rating, stake math |
| `internal/consumer` | Session pools, open/close, prompt streaming, reconciliation |
| `internal/gateway` | OpenAI-compatible API and admin API |
| `internal/apikeys` | Key store, limits, quotas, usage |
| `internal/provider` | Provider handlers, backend relay, health, receipts, claims |
| `internal/wallet` | BIP-39/32/44 derivation, keystore, signing |
| `internal/tui`, `internal/cli` | Wizard, dashboard, commands |

Dependencies are kept small: a handful of go-ethereum crypto/ABI packages, Charm's TUI libraries, a TOML parser
and BIP-39. There is no database, no web UI, and no Node.js or Electron.

## Development

```sh
make build        # bin/morcore
make test         # unit, protocol and end-to-end tests
make race         # with the race detector
make lint         # go vet + gofmt
make release      # cross-compile 6 targets into dist/ with checksums
```

### Local devnet

`morcore-devnet` simulates the whole marketplace locally: a BASE-like chain with the Diamond, MOR and
Multicall3 contracts, two providers, and an echo model (or your own backend with `--backend`). No real funds
are involved.

```sh
make devnet                        # terminal 1

# terminal 2 (the devnet prints these lines; `make install` puts morcore on your PATH)
export MORCORE_HOME=$(mktemp -d) MORCORE_PASSWORD=devnet-password
export DEVNET_KEY=0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80   # Hardhat's public test key
morcore init --yes --network local --key-env DEVNET_KEY --password-env MORCORE_PASSWORD --api-key demo
morcore start
```

### Interop with the reference node

`internal/morrpc/testdata/reference_vectors.json` was generated by the reference proxy-router's own code, and
morcore's tests verify every message type against it. The other direction is checked too: the reference's
validators, signature checks, ECIES decryption and OpenAI parsing all accept morcore's messages. To re-run both
directions against the latest reference code:

```sh
scripts/interop/run.sh            # REF_BRANCH=main by default
```

### Releasing

Push a tag like `v0.1.0`. GitHub Actions runs the tests, builds Linux, macOS and Windows for amd64 and arm64,
and publishes the archives and `checksums.txt` to the release. The install scripts read those archives.

## Limitations

- Chat completions and embeddings only. The reference node's audio (speech, transcription), agent-tool and
  chat-history endpoints, and its image-generation backends, are not implemented.
- TEE attestation is not verified locally. Providers that report `tee_unverified` health are skipped.
- No chat history storage. The gateway is stateless; agents keep their own context.
- The protocol is verified against the reference implementation, and the full flow is tested on the local
  devnet. Try BASE Sepolia (`--network testnet`) before committing real MOR.
