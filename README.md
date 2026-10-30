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
  as a directory. No SDK, no websocket, no ceremony.
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
```

| Key | Default | Meaning |
|---|---|---|
| `Server.listen` | `:8080` | `host:port` for the HTTP server |
| `Server.token` | *(empty)* | When set, every request must send `Authorization: Bearer <token>` |
| `Connection.*` | — | Transport to the device — BLE scan (with optional name/address selection), USB serial or TCP |

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

Both DMs and channel messages arrive on the same queue, interleaved in
arrival order.

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
| `ack` | — | `true` = wait for *delivery* confirmation (end-to-end, slower); default `false` = *sent* confirmation |

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
auth, error mapping). The device runtime (connect, startup sync, unread
inbox, keepalive) comes from
[meshcore-kit](https://github.com/stempit/meshcore-kit); the
protocol driver from [meshcore-go](https://github.com/stempit/meshcore-go).

## License

MIT — see [LICENSE](LICENSE).
