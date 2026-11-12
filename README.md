# meshcore-http-gateway 🌉📡

**Your MeshCore device, but with an HTTP API.**

meshcore-http-gateway is a **thin HTTP gateway** to a
[MeshCore](https://meshcore.io) LoRa device: everything the radio receives is
exposed over `GET /get`, and anything you post to `POST /send` goes out over
the mesh. No bot commands, no Telegram, no plugins — just a clean bridge
between HTTP clients and the radio.

It's the layer you want when *another* program needs to sit on the mesh:
an LLM agent, a home-automation script, a custom dashboard, a notification
daemon — anything that can speak HTTP can now speak LoRa.

*[🇷🇺 Русская версия](README_RU.md)*

---

## Why you'll want it

- 🔌 **Two-way API.** Poll `/get` (or the cursor-based `/messages`) for new
  messages, POST to `/send` to talk back, list `/contacts` and `/channels`
  as a directory, check the link via `/status`, and reach the device itself
  via `POST /cli`. No SDK, no websocket, no ceremony.
- 🗂️ **Device management over HTTP.** The device's contacts and channels are
  writable too: add/remove contacts, create or join channels, free slots —
  the same admin surface the official apps have, as plain REST.
- 📨 **Push or pull.** Prefer being notified? The gateway can POST every new
  message to your own webhook endpoint the moment it lands — no polling.
- 📥 **Nothing gets lost.** Messages that arrived while the server was down
  are synced from the device's storage on startup and served as unread.
- 📤 **Honest sends.** `/send` blocks until the device confirms the message
  actually went out — and tells you which kind of failure happened, with the
  right HTTP status code.
- 🔐 **Optional bearer-token auth.** One config key; leave it empty for
  friction-free local use.
- 🛰️ **Engineered for bad radio.** Same send machinery as the full bot:
  paced retries, saved/flood path handling, multi-transmission channel sends
  with adaptive early-stop.
- 💤 **Self-healing link.** Periodic stats keep the BLE connection warm and
  trigger automatic reconnects when the device drops.

---

## Running

```sh
go run .
# or build it:
go build -o meshcore-http-gateway . && ./meshcore-http-gateway
```

The binary reads a `config.yaml` from its working directory — the `Server`
and `Connection` sections documented below. It's a subset of the shared
family format, so a combined `config.yaml` kept alongside the bot or the
voice bridge keeps working here unchanged.

### Configuration

All options live in the `Server` section of `config.yaml`:

```yaml
Server:
  listen: ":8080"        # bind address (default :8080)
  token: "s3cret"        # optional; empty = no auth

Connection:
  type: "bt"             # "bt" (BLE, default), "serial" or "tcp" (Wi-Fi/Ethernet)
  serial_port: "/dev/ttyUSB0"    # when type: "serial"
  serial_baud: 115200
  # tcp_address: "192.168.1.50"   # when type: "tcp" — host or host:port (default 5000)
  # device_name: "MeshCore-AB12"  # when type: "bt" — pick one device by advertised name
  # device_address: "AA:BB:CC:DD:EE:FF"  # when type: "bt" — or by platform address
  channel_transmissions: 3   # max transmissions per channel message (default 3)

Push:
  enabled: false         # when true, every new message is POSTed to endpoint_url
  endpoint_url: "http://example.local:9000/hook"  # required when enabled
  token: ""              # optional; sent to the endpoint as "Authorization: Bearer <token>"
  timeout_seconds: 10    # per-POST timeout
```

| Key | Default | Meaning |
|---|---|---|
| `Server.listen` | `:8080` | `host:port` for the HTTP server |
| `Server.token` | *(empty)* | When set, every request must send `Authorization: Bearer <token>` |
| `Connection.*` | — | Transport to the device — BLE scan (with optional name/address selection), USB serial or TCP |
| `Push.*` | disabled | Webhook delivery of new messages — see [Push mode](#push-mode--webhook-delivery) |

---

## API

### `GET /get` — read new (unread) messages

Returns everything received since your **last successful `/get`** — reading
marks messages as read and drains the queue. Poll as often as you like; an
empty inbox returns `{"messages":[]}`, never an error.

```sh
curl -H "Authorization: Bearer s3cret" localhost:8080/get
```

```json
{
  "messages": [
    {
      "id": 42,
      "type": "dm",
      "sender": "Короед",
      "sender_key": "a1b2c3d4e5f6…",
      "channel": "",
      "channel_index": -1,
      "text": "ping, anyone there?",
      "timestamp": "2026-10-25T12:00:00Z",
      "received_at": "2026-10-25T12:00:03Z"
    },
    {
      "id": 43,
      "type": "channel",
      "sender": "Wire",
      "sender_key": "",
      "channel": "public",
      "channel_index": 0,
      "text": "weather looking grim",
      "timestamp": "2026-10-25T12:00:10Z",
      "received_at": "2026-10-25T12:00:11Z"
    }
  ]
}
```

**Field notes**

| Field | `type: "dm"` | `type: "channel"` |
|---|---|---|
| `sender` | contact name, or `?<6-hex>` if the key is unknown | name the sender signed the message with |
| `sender_key` | sender's full hex public key | `""` |
| `channel` / `channel_index` | `""` / `-1` | channel name / index |
| `timestamp` | sender-side mesh timestamp | sender-side mesh timestamp |
| `received_at` | when *this server* got it | when *this server* got it |
| `id` | sequential server-assigned ID — use it to dedupe across reconnects | same |

Both DMs and channel messages arrive on the same queue. Live messages
interleave in arrival order; messages synced from device storage at startup
arrive grouped — stored DMs first, then stored channel messages.

### `GET /messages?after=<id>` — cursor-based read (non-destructive)

The drain-on-read semantics of `/get` mean its callers share one mailbox.
When several consumers poll the same server, use the cursor variant instead:
it returns everything newer than `after` **without** removing it.

```sh
curl -H "Authorization: Bearer s3cret" "localhost:8080/messages?after=42"
```

```json
{"messages": [ …same shape as /get… ], "last_id": 57}
```

Keep the highest `id` you have processed (or the returned `last_id`) and pass
it back as `after` next time. `last_id` is always safe as a cursor.

### `GET /contacts` / `GET /channels` — directory

Read-only listings to resolve recipients before `POST /send`:

```json
{"contacts": [{"name": "Короед", "key": "a1b2c3…", "last_seen": "2026-10-25T12:00:00Z"}]}
{"channels": [{"index": 0, "name": "public"}]}
```

`/contacts` is sorted by name; `/channels` by index and skips empty slots.
Both come from the gateway's live cache — no radio traffic at request time.

Add **`?full`** for the admin view instead: a fresh listing straight from the
device — `/contacts?full` with every stored field, `/channels?full` with every
slot *including empty ones* and the key material:

```json
{"contacts": [{"name": "Yorick 💀", "key": "a1b2…", "type_name": "chat", "type": 1,
  "flags": ["favorite"], "path_length": 2, "path_hex": "0405", "lat": 55.75, "lon": 37.61,
  "last_seen": "2026-10-25T12:00:00Z", "last_message_at": "2026-10-25T12:01:00Z",
  "is_active": true, "last_mod": 1700000000}]}
{"channels": [{"index": 2, "name": "team", "psk_hex": "9cd8fcf2…", "hash_byte": 156, "is_empty": false}]}
```

(`?full=false` / `?full=0` explicitly keep the cached listing; `path_length`
is the firmware's raw out-path byte — hop count in the low 6 bits, bytes per
hop in the top 2, `0xFF` = flood/unknown; `hash_byte` is how group packets
are addressed on air, the way to correlate a slot with raw `0x88` frames.)

A `?full` listing is a device round trip — it queues behind sends like any
admin call below. **`psk_hex` is the channel's secret** — the same value a QR
code `meshcore://channel/add?name=…&secret=<hex>` carries; the bearer token
is the only thing between it and the world.

### Contact administration — `POST /contacts`, `GET /contacts/{key}`, `DELETE /contacts/{key}`

Write contacts into the device's storage (not just the gateway's cache) and
read one back for verification. Every call is a device round trip.

Add or update — `POST /contacts` is an upsert: the firmware keys the entry on
the full 32-byte public key, so sending an existing key rewrites it.

```sh
curl -X POST localhost:8080/contacts \
  -H "Authorization: Bearer s3cret" \
  -H "Content-Type: application/json" \
  -d '{"key": "a1b2c3d4e5f6…(64 hex chars)", "name": "Короед", "type": "chat", "flags": ["favorite"], "lat": 55.75, "lon": 37.61}'
```

```json
{"status": "saved", "key": "a1b2c3d4e5f6…"}
```

| Field | Required | Meaning |
|---|---|---|
| `key` | ✔ | full 64-hex-character public key — from a `meshcore://contact/add?…` QR, `GET /contacts/{key}`, or `GET /contacts?full` |
| `name` | ✔ | up to 31 bytes (the on-device field size; longer is a `400`, not a silent truncation) |
| `type` | — | `chat` (default) / `repeater` / `room` / `sensor` |
| `flags` | — | array of `favorite` / `tele_base` / `tele_loc` / `tele_env` |
| `lat`, `lon` | — | location; must be given together |

The path is deliberately not writable: a manually added contact starts
flood-routed and the device learns a real path from adverts and traffic.

`GET /contacts/{key}` reads the contact back from the device — the
"verify what landed" call (the device's answer also refreshes the gateway's
cache). `DELETE /contacts/{key}` removes it from the device's storage.

### Channel administration — `POST /channels`, `DELETE /channels/{index}`

There is no "join" command on the wire: entering a channel *is* writing it
into a free slot. `POST /channels` dispatches on `action` the way `/send`
dispatches on `type`.

```sh
curl -X POST localhost:8080/channels \
  -H "Authorization: Bearer s3cret" \
  -H "Content-Type: application/json" \
  -d '{"action": "create_private", "name": "Team"}'
```

```json
{"index": 2, "name": "team", "psk_hex": "9cd8fcf2…", "hash_byte": 156, "is_empty": false}
```

| Action | Fields | Meaning |
|---|---|---|
| `create_private` | `name` ✔ | generate a random key and enter a private channel; the response carries `psk_hex` — share it (QR) with participants |
| `add` | `name` ✔, `psk` ✔ | join an existing private channel by its 32-hex-character key (from a QR code or another participant) |
| `join_public` | `name` — | the standard MeshCore public channel (fixed firmware key); empty name defaults to `"Public"` |
| `join_hashtag` | `name` ✔ | hashtag channel: the key is derived as `SHA256("#name")[:16]`, compatible with the official apps |
| `set` | `index` ✔ (0–255), `name` ✔, `psk` ✔ | overwrite one specific slot — the point-edit primitive the actions above are built on |

`409` means the device has no free slot left — `DELETE /channels/{index}`
frees one. Deleting a channel overwrites the slot with an empty name and a
zero key, the protocol's way of deleting: a named-but-zero-key slot would
keep catching null-key traffic, so clearing the name alone is not removal.

### Admin status codes

All admin endpoints (`POST/DELETE /contacts…`, `POST/DELETE /channels…`,
`?full` listings) block on the shared device path like `/send` — each waits
its turn behind an in-flight send (the device protocol has no sequence
numbers, so device commands must never interleave). Give your HTTP client the
same generous timeout, and note that a queued admin call is not cancelled if
its client disconnects. Status codes:

| Status | When |
|---|---|
| `400` | malformed request — bad hex key/PSK, unknown `type`/`flags`/`action`, missing fields, name over 31 bytes, channel index outside 0–255, `lat`/`lon` out of range, or a field the chosen `action` does not take |
| `404` | the device reports `not_found` — no such contact, or channel index out of range |
| `409` | the device's storage is full (`table_full`) or it has no free channel slot |
| `502` | the device rejected the command, or the transport failed |
| `504` | the device did not answer in time |

On success the driver's cache is updated too, so the cached directory and
`/send` recipient resolution reflect the change immediately.

### `GET /status` — device and link snapshot

A cached view of the device: a `/status` call generates **no radio traffic**
and never queues behind a send. Identity and radio config come from the
connect handshake, statistics from the runtime's 30-second keepalive polls,
and the device clock is sampled once at startup.

```sh
curl -H "Authorization: Bearer s3cret" localhost:8080/status
```

```json
{
  "connected": true,
  "transport": "bt",
  "device": {
    "name": "Bot-AB12",
    "model": "Heltec V3",
    "firmware": "1.8.0",
    "freq_hz": 869618000,
    "bw_hz": 250000,
    "sf": 9,
    "cr": 5,
    "tx_power": 14,
    "max_tx_power": 22,
    "lat": 55.75,
    "lon": 37.61
  },
  "stats": {
    "battery_mv": 3980,
    "uptime_secs": 12345,
    "errors": 0,
    "queue_len": 0,
    "tx_air_secs": 512,
    "rx_air_secs": 1024,
    "noise_floor_dbm": -112,
    "last_rssi_dbm": -98,
    "last_snr_db": 7.5,
    "packets_recv": 9021,
    "packets_sent": 1032,
    "packets_recv_errors": 3,
    "age_secs": 12
  },
  "device_time": "2026-11-09T12:00:00Z",
  "device_time_sampled_at": "2026-11-09T12:00:01Z",
  "started_at": "2026-11-09T11:58:20Z"
}
```

**Field notes**

| Field | Meaning |
|---|---|
| `connected` | live link to the device (drops are reconnected in the background) |
| `transport` | configured connection type: `bt`, `serial` or `tcp` |
| `device` | identity and radio config from the handshake; omitted until the handshake completes. `lat`/`lon` are omitted when the node has no location set (0,0 counts as unset) |
| `stats` | battery/airtime/packet counters, refreshed every 30 s by the keepalive; `age_secs` is how old the reading is. Omitted until the first poll lands (≤30 s after start) |
| `device_time` / `device_time_sampled_at` | the device's RTC read once at startup, and when it was read — compare with your own clock to see drift; omitted if the startup sample failed. The firmware syncs its RTC from the host at connect, so a visible drift means that sync failed |
| `started_at` | when the HTTP server was built (after device connect and the startup sync) |

Note that `/status` reports your node's configured location — the bearer
token grants it, same as everything else on this API.

### `POST /send` — send a message over the mesh

```sh
curl -X POST localhost:8080/send \
  -H "Authorization: Bearer s3cret" \
  -H "Content-Type: application/json" \
  -d '{"type": "dm", "to": "Короед", "text": "copy that"}'
```

Request body:

| Field | Required | Meaning |
|---|---|---|
| `type` | ✔ | `"dm"` or `"channel"` |
| `to` | ✔ | DM: contact **name** or hex **public-key prefix**. Channel: channel **name** or numeric **index** |
| `text` | ✔ | message text (non-empty) |
| `ack` | — | `true` = wait for *delivery* confirmation (end-to-end, slower) — **DMs only; ignored for `type: "channel"`**, where success means the device queued the transmissions; default `false` = *sent* confirmation |

Responses:

| Status | Body | When |
|---|---|---|
| `200` | `{"status":"sent"}` | transmitted and confirmed |
| `400` | `{"error":"…"}` | invalid JSON, missing `to`/`text`, bad `type` |
| `401` | `{"error":"…"}` | missing/wrong bearer token (only if `Server.token` is set) |
| `404` | `{"error":"contact not found: …"}` / `{"error":"channel not found: …"}` | destination didn't resolve |
| `502` | `{"error":"…"}` | device rejected the send, or transport failure |
| `504` | `{"error":"…"}` | every attempt timed out with no confirmation |

```json
{"status": "sent"}
```

### `POST /cli` — run a device CLI command (advanced)

Runs one ASCII CLI command **on the device itself** (companion firmware v14+)
and returns the device's verbatim reply: `ver`, `get tx`,
`set radio 869.618,250,9,5`, `reboot` — everything in the firmware's
`docs/cli_commands.md`. This is the escape hatch for device management from
HTTP; messages over the mesh still go through `POST /send`.

```sh
curl -X POST localhost:8080/cli \
  -H "Authorization: Bearer s3cret" \
  -H "Content-Type: application/json" \
  -d '{"command": "get tx"}'
```

```json
{"reply": "> 869.618"}
```

Request body:

| Field | Required | Meaning |
|---|---|---|
| `command` | ✔ | the CLI command text, at most 160 bytes (the firmware's line buffer) |

The `reply` is the device's own text, not a parsed result: `get` commands
answer `"> <value>"`, a failed command answers `Error, ...` — those are **200**
responses (the command ran; the device just said no).

Responses:

| Status | When |
|---|---|
| `200` | command ran; body is the device's reply |
| `400` | invalid JSON, missing/empty `command`, or length outside the 2–160 byte range |
| `401` | missing/wrong bearer token (only if `Server.token` is set) |
| `502` | device rejected the command — e.g. `unsupported_cmd` on firmware older than v14 — or transport failure |
| `504` | the device did not answer in time |

**Handle with care.** This exposes the device's *full* CLI — `reboot`,
`factory reset`, radio reconfiguration included. Behind `Server.token` that is
as safe as your token; without one it is safe only on a trusted network.

Like `/send`, the command runs on the shared device path: a `/cli` issued
while a send is in flight waits for it to finish (tens of seconds worst case),
so give your HTTP client the same generous timeout as `/send`. And like
`/send`, a queued command is **not cancelled** if its client times out or
disconnects — no reply does not mean nothing happened.

---

## Push mode — webhook delivery

Polling not your thing? Set `Push.enabled: true` and the gateway POSTs every
new message to your HTTP endpoint the moment it lands:

```yaml
Push:
  enabled: true
  endpoint_url: "http://example.local:9000/hook"  # required
  token: ""            # optional; sent as "Authorization: Bearer <token>"
  timeout_seconds: 10  # per-POST timeout (default 10)
```

Each message is its own POST — the exact same JSON object `/get` returns per
message. A direct message:

```json
{"id": 41, "type": "dm", "sender": "Короед", "sender_key": "a1b2c3d4e5f6…",
 "channel": "", "channel_index": -1, "text": "ping, anyone there?",
 "timestamp": "2026-10-25T12:00:00Z", "received_at": "2026-10-25T12:00:03Z"}
```

and a channel message:

```json
{"id": 42, "type": "channel", "sender": "Wire", "sender_key": "",
 "channel": "public", "channel_index": 0, "text": "weather looking grim",
 "timestamp": "2026-10-25T12:00:10Z", "received_at": "2026-10-25T12:00:11Z"}
```

Headers: `Content-Type: application/json`, plus `Authorization: Bearer <token>`
when `Push.token` is set. Respond with **any 2xx** to accept a message.

**Delivery semantics**

- Any **2xx** = delivered. Any other **4xx** = the endpoint doesn't want that
  message — it's dropped (logged) and the next one is sent. Network errors,
  timeouts, **429 and 5xx** are retried with exponential backoff (2s, doubling,
  capped at 60s).
- Messages arrive **in order**, one POST at a time. While the endpoint is down
  they queue in memory (capped at 1000, oldest dropped first) and replay when
  it recovers.
- Push is **live-only**: messages already in the inbox at startup (the device
  stored-message sync) are not pushed, and the queue does not survive a
  gateway restart. For guaranteed, replayable consumption use
  `GET /messages?after=<id>`.
- Push and pull coexist: a `/get` drain by a pull client never removes
  anything from the push queue, and vice versa.

---

## Behavior you should know about

### `/send` blocks — by design

LoRa is slow. A send with retries (device confirmations, inter-transmission
pacing, path resets) legitimately takes **10–30 seconds**; `"ack": true` can
take longer. The HTTP request stays open the whole time so the response
reflects the real outcome.

- Set your HTTP client timeout to **90s+**.
- Concurrent `/send`s don't collide: the device path serializes them, and each
  request waits its turn.
- A `504` means *maybe sent, unconfirmed* — the device may still deliver the
  message late. Retrying can duplicate it; prefer idempotent texts or wait.

### Unread semantics

- For `/get`, unread = **received since your last `/get`**. There is no
  per-client state: the first caller to poll drains the queue. Use one poller
  process, or accept that pollers share a single mailbox.
- Multiple readers should use the cursor endpoint instead:
  `GET /messages?after=<id>` never removes anything, so every consumer keeps
  its own cursor.
- The inbox is in-memory and capped at **1000 messages**; on overflow the
  oldest are dropped first. If nobody polls for a very long time, early
  messages are lost.
- On restart the device sync restores messages **stored on the device** into
  the unread queue — the mesh's own retention, not the server's.

### Startup

The device connection (BLE scan or serial) and the stored-message sync happen
**before** the HTTP server starts listening. If the device isn't reachable,
the process exits — there's nothing useful to serve. The server is then
available for the process's lifetime; BLE drops are reconnected in the
background (messages received while disconnected are simply not seen — that's
radio).

---

## Example: a minimal poll-answer loop

```sh
#!/bin/sh
while true; do
  # read everything new, answer each message with a canned reply
  resp=$(curl -sf -H "Authorization: Bearer s3cret" localhost:8080/get)
  echo "$resp" | jq -c '.messages[] | select(.type=="dm") | {type, to: .sender, text: ("echo: " + .text)}' |
  while IFS= read -r body; do
    curl -s -X POST localhost:8080/send \
      -H "Authorization: Bearer s3cret" \
      -H "Content-Type: application/json" \
      -d "$body" > /dev/null
  done
  sleep 5
done
```

(Python/Go clients work the same way: poll `/get`, POST `/send`.)

---

## How it relates to the rest of the family

| | [meshcore-go-bot](https://github.com/stempit/meshcore-go-bot) | this gateway | [meshcore-mcp-gateway](https://github.com/stempit/meshcore-mcp-gateway) |
|---|---|---|---|
| Purpose | autonomous chat bot on the mesh | HTTP gateway for *your* code | XiaoZhi voice-assistant bridge |
| Command processing (`.ping`, games, LLM…) | ✔ | ✘ | ✘ |
| Telegram / MQTT / alerts bridges | ✔ | ✘ | ✘ |
| Inbox | dispatcher pipeline | `GET /get` JSON | voice reader (`mesh_get_new_messages`) |
| Sends | replies, paced by dispatcher | `POST /send`, same radio pacing | `mesh_send_message`, same radio pacing |
| Device connection | own | own | own |
| Config | `config.yaml` | `Connection` + `Server` | `Connection` + `MCP` |

Each app opens its **own** device connection — they are independent
features. Don't point two of them at the **same physical device** at the same
time: a MeshCore companion serves one BLE/serial connection.

Source map: `main.go` (composition root + config), `server.go` (handlers,
auth, error mapping), `push.go` (webhook delivery). The device runtime
(connect, startup sync, unread inbox, keepalive) comes from
[meshcore-kit](https://github.com/stempit/meshcore-kit); the
protocol driver from [meshcore-go](https://github.com/stempit/meshcore-go).

## License

MIT — see [LICENSE](LICENSE).
