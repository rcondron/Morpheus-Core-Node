# Marketplace storage (IPFS)

morcore lets storage providers sell IPFS storage on the Morpheus marketplace and get paid in MOR, and lets
users and their agents keep files with them. It needs no new contracts: storage is bought with the same
sessions that buy inference.

- **Listings.** A storage listing is an ordinary ModelRegistry entry tagged `storage`, `ipfs` and
  `quota:<size>`, e.g. `quota:100GiB`. One session on the listing buys that much storage.
- **Bids.** A provider's bid on a listing prices that quota per second, like any compute bid.
- **Leases.** A lease is a session on a storage bid, opened with **direct payment**, so the provider is paid
  from the user's stake for the lease's whole duration. The rest of the stake comes back when the lease closes.
- **Files.** The provider pins the user's files on its own Kubo node while leases cover them, plus a grace
  period. Users audit providers with Merkle proofs of randomly chosen blocks.

```
 user / agent ──Files API──▶ morcore (consumer)                      morcore (provider) ──▶ Kubo
                              │ encrypt to wallet, compute CID           ▲  re-import, require CID,
                              │ open lease (session, direct payment) ────┘  dag/import (pinned)
                              │ storage.put.* · get · prove · delete over MOR-RPC (signed both ways)
                              ▼
                        BASE: SessionRouter pays the provider from the lease stake
```

## Using storage

The node has to be running: it renews leases in the background.

```sh
morcore storage providers                 # offers, cheapest per byte first (works offline from the node)
morcore storage put report.pdf            # encrypted to your wallet; --public stores it as is
morcore storage ls
morcore storage get file-3f9c… ./report.pdf
morcore storage status                    # usage, leases, stake
morcore storage audit                     # challenge providers now
morcore storage rm file-3f9c…
morcore storage recover                   # new machine: rebuild the file list from the chain and providers
```

Agents use the OpenAI-style Files API with a key that has the `storage` scope
(`morcore keys create archivist --scopes inference,storage`):

```sh
curl http://127.0.0.1:8082/v1/files -H "Authorization: Bearer $KEY" -F purpose=assistants -F file=@notes.md
curl http://127.0.0.1:8082/v1/files -H "Authorization: Bearer $KEY"
curl http://127.0.0.1:8082/v1/files/file-3f9c…/content -H "Authorization: Bearer $KEY" -o notes.md
curl -X DELETE http://127.0.0.1:8082/v1/files/file-3f9c… -H "Authorization: Bearer $KEY"
```

```python
from openai import OpenAI
client = OpenAI(base_url="http://127.0.0.1:8082/v1", api_key=KEY)
f = client.files.create(file=open("notes.md", "rb"), purpose="assistants")
data = client.files.content(f.id).read()
```

File objects carry the usual fields (`id`, `bytes`, `filename`, `purpose`, `created_at`) plus `cid`,
`encrypted`, `stored_bytes`, `copies` and `providers`. Each API key sees only the files it stored; the operator
(admin token) sees all. Add `?public=true` (or a `public=true` form field) to store a file unencrypted.

### What the node does for you

- **Encryption.** By default each file is sealed before it leaves the machine. The file name, type and a
  random AES-256 key go in a header encrypted to the wallet's public key (ECIES); the content is AES-256-GCM in
  64 KiB segments, authenticated in order, so a reordered or truncated file is rejected. Providers see only
  ciphertext and its size. Any machine with the wallet can decrypt.
- **Content addressing.** The node computes the file's CID itself, exactly as
  `ipfs add --cid-version=1 --raw-leaves --chunker=size-262144` would (balanced DAG, 174 links per node,
  sha2-256). The CID is checked against real Kubo output in the tests. Every download is re-hashed against that
  CID before a byte reaches you.
- **Leases.** To store a file, the node uses room under a lease it already holds. Otherwise it buys a lease
  on the cheapest offer whose quota fits (`storage.providers` can pin the choice to certain providers).
- **Renewal.** Before leases run out (`renew_before`), the node buys new ones, enough to cover what each
  provider holds. When a provider stops selling storage, its files move elsewhere.
- **Closing leases.** Leases at providers that no longer hold any of your files are closed early, and ended
  leases are closed, so stake comes back.
- **Audits.** Every `audit_interval` the node asks each provider for the blocks on the path from the root to
  a random byte, with a fresh nonce, and checks them against the CID and size it recorded. A copy that fails
  twice in a row is replaced: the node fetches the file from a healthy copy (or from the failing provider if it
  still serves it intact), stores it with another provider and deletes the bad copy.
- **Recovery.** The manifest (`storage.json`) is the only local state. If it is lost, `morcore storage recover`
  finds your storage leases among the wallet's recent sessions and lists what each provider holds for you. It
  restores names and types from the encrypted headers.

### Costs

A lease buys a listing's full quota for its duration, whatever you use. Example: a provider bids
0.2 MOR per GiB-month on a 100 GiB listing.

| | |
|---|---|
| Price per second | 0.2 × 100 / 2,592,000 ≈ 0.0000077 MOR |
| One 24 h lease | ≈ 0.67 MOR, paid to the provider from the lease's stake |
| Stake escrowed per lease | What the marketplace requires for a 24 h session at that price (`morcore storage providers` shows the lease price; the stake follows the protocol's stake-to-stipend rate). The rest returns when the lease closes. |

`storage.max_stake` caps the MOR escrowed in storage leases at once.

### Settings

```toml
[storage]
  lease = "24h0m0s"            # length of each lease (capped by the contract's maxSessionDuration)
  renew_before = "30m0s"       # buy the next lease this long before cover runs out
  replicas = 1                 # providers that keep each file
  encrypt = true               # encrypt new files to the wallet key (per file: --public)
  audit_interval = "6h0m0s"
  max_stake = "0"              # cap on MOR escrowed in leases ("0" = none)
  providers = []               # only use these provider addresses
```

## Selling storage

1. Run [Kubo](https://docs.ipfs.tech/install/command-line/) (`ipfs daemon`) on the provider host. Keep its RPC
   API (port 5001) private. If it isn't local, protect it with `API.Authorizations` and pass the header with
   `--ipfs-auth`.
2. Point the node at it and check it:

   ```sh
   morcore provider storage --ipfs-api http://127.0.0.1:5001
   ```

3. Choose how much one lease buys. `storage-listing` joins an existing listing with that quota, or registers a
   new one (`--new`):

   ```sh
   morcore provider storage-listing 100GiB
   ```

4. Post a price and restart the node:

   ```sh
   morcore provider bid <listing> --price-gib-month 0.2
   morcore start
   ```

`morcore doctor` checks Kubo, the listings and whether your capacity covers them. The status screen and
dashboard show users, files and stored bytes.

**Getting paid.** A lease pays you price × duration. When the user closes it, the session settles. Ended
leases the user never closed are claimed automatically every hour, or with `morcore provider claim`. The
marketplace's usual rules apply, including the cap on earnings relative to your provider stake.

**What the node enforces.**
- **Attaching a lease.** The session must belong to the requesting wallet, be live, and be paid directly
  (unless `accept_stipend`). It must also be bound to one of your bids on a storage listing.
- **Uploads.** Uploads must fit the user's live quota (all live leases added up) and `max_file_size`. The node
  re-imports every upload and refuses it unless it hashes to the CID the user announced. It then streams the
  DAG into Kubo with `dag/import`, which pins it. Your Kubo settings never change a user's CIDs.
- **Capacity.** The quota sold across live leases stays under `max_total`, which defaults to Kubo's
  `Datastore.StorageMax`.
- **Retention.** Files are kept while leases cover the user, and for `grace` (default 72 h) after the last one
  ends, then unpinned. If a user stays over their live quota for longer than the grace period, their newest
  files are unpinned until they fit.
- **Reads and proofs.** These come from local blocks only (`offline=true`), so the node never fetches from the
  network what it claims to hold.

```toml
[provider.storage]
  enabled = true
  ipfs_api = "http://127.0.0.1:5001"
  ipfs_auth = ""               # e.g. "Bearer …" when API.Authorizations is set
  listings = ["0x…"]           # storage listings you serve
  max_file_size = "4GiB"
  max_total = "0"              # quota sold across live leases ("0" = Kubo's StorageMax)
  grace = "72h0m0s"
  accept_stipend = false       # also accept leases paid from the compute budget
  gateway = ""                 # public gateway to advertise (optional)
```

## Protocol

Storage adds `storage.*` methods to MOR-RPC. They use the same TCP transport and JSON envelope as the rest of
the protocol.

- **Requests** are signed by the user's wallet, the same way as other MOR-RPC params. They are accepted within
  10 minutes of their timestamp.
- **Results** come back as `{result, timestamp, signature}`, signed by the provider. The user checks that the
  signer may act for the provider: the provider address itself, the owner of a provider contract, or a
  delegate.

| Method | Params | Result |
|---|---|---|
| `storage.info` | `nonce` | listings and their quotas, `maxFileBytes`, `graceSeconds`, `payment` (`direct`/`any`), `chunkBytes`, the nonce |
| `storage.attach` | `session` | the user's quota, used bytes and `coveredUntil` |
| `storage.put.start` | `session` (optional), `cid`, `size`, `label` | `upload` id and `chunkBytes` (no id when the user already stores that CID) |
| `storage.put.chunk` | `upload`, `offset`, `data` (base64, ≤ 8 MiB) | bytes received |
| `storage.put.end` | `upload` | receipt: `cid`, `size`, `owner`, `storedAt` |
| `storage.list` | – | quota, usage, cover and the user's files |
| `storage.get` | `cid`, `offset`, `length` (≤ 4 MiB) | `data` (base64) |
| `storage.prove` | `cid`, `offset`, `nonce` | the DAG blocks from the root to the leaf holding `offset`, and the nonce |
| `storage.delete` | `cid` | bytes freed |

**Proofs.** The user knows the root CID and the file size. It checks that each block hashes to the CID its
parent links to, starting from the root, and that each step follows the child whose UnixFS `blocksizes` range
covers the offset. The last block must be the raw leaf holding that byte.

**Encrypted container (`MCE1`).**
- **Layout:** `"MCE1"`, the header length (uint32, big-endian), the header, then the segments.
- **Header:** ECIES-encrypted to the owner's public key, holding the name, type, size, created time, AES key and
  nonce prefix (JSON).
- **Segments:** AES-256-GCM over 64 KiB of plaintext each. The nonce is the 8-byte prefix plus a 4-byte counter.
  The additional data is one byte marking the final segment.

## Trust model and limits

- **What audits prove.** Audits show that a provider could produce a random block when asked. They are not
  continuous proofs of replication. A provider that fails them keeps the current lease's payment, but the node
  stops renewing with it and moves your files elsewhere. Use `replicas = 2` or more for files you cannot lose.
- **What providers see.** They see your wallet address, file sizes, CIDs and access times. With encryption on,
  they do not see names or contents. MOR-RPC is plain TCP, so the same is true for anyone on the network path.
  Public files and their labels are readable by anyone with the CID.
- **Downloads** are buffered in a temporary file so they can be checked against the CID before they are
  returned.
- **Other marketplace software.** Storage listings are marketplace models, so other marketplace front ends list
  them as models. The reference Lumerin node does not implement `storage.*`, so storage providers must run
  morcore.
- **Files are lost for good** if the node stays offline past a provider's grace period and leases are not
  renewed, unless you hold copies elsewhere.
