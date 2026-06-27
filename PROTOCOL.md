# SqueezePlay Server Protocols Reference

## Overview

SqueezePlay communicates with a server (LMS / Logitech Media Server / SlimServer)
over multiple independent TCP/UDP connections. Each connection carries a distinct
protocol. The protocols are:

| Protocol     | Transport | Port | Direction         |
|--------------|-----------|------|-------------------|
| UDP Discovery| UDP       | 3483 | Server ← Client   |
| SlimProto    | TCP       | 3483 | Server ↔ Client   |
| CometD       | HTTP/TCP  | 9000 | Server ↔ Client   |
| JSON-RPC     | HTTP/TCP  | 9000 | Server ↔ Client   |
| CLI          | TCP       | 9090 | Server ← Client   |

---

## 1. UDP Discovery Protocol (port 3483)

The client discovers servers on the local network by sending UDP broadcast/
multicast requests. The server responds directly to the requester's IP+port.

### 1.1 TLV Request ("e")

First byte: ASCII `e` (0x65)
Followed by a sequence of TLV entries (Tag + Length + Value):

    4 bytes: Tag (e.g. "IPAD", "NAME", "JSON", "VERS", "UUID", "CLIP")
    1 byte:  Value length N
    N bytes: Value

If the request payload is empty (beyond the `e` prefix), the server should
respond with all available tags. Otherwise, the server responds only with the
requested tags.

### 1.2 TLV Response ("E")

First byte: ASCII `E` (0x45)
Followed by the same TLV entries with values filled in.

A typical minimal response for a request with tags
`[IPAD NAME JSON VERS UUID]` would be:

    E  IPAD \x09 192.168.1.8  NAME \x0b GoSlimServer  JSON \x04 9000
      VERS \x03 9.0           UUID \x16 goslimserver-uuid-1234

### 1.3 Legacy Request ("d")

First byte: ASCII `d` (0x64)
No additional payload. The server responds with a 17-byte legacy packet.

### 1.4 Legacy Response ("D")

    1 byte:  ASCII 'D' (0x44)
    16 bytes: Server IP address string, NUL-padded

### 1.5 Observed SqueezePlay Behavior

SqueezePlay sends TLV requests every ~2 seconds while searching for servers.
It requests tags: `IPAD`, `NAME`, `JSON`, `VERS`, `UUID`, `JVID`.

---

## 2. SlimProto Protocol (TCP port 3483)

This is the primary control+audio streaming protocol. Each player establishes
ONE persistent TCP connection to the server.

### 2.1 Packet Format (Server-side)

    [2 bytes: total length (big-endian)]
    [4 bytes: opcode (ASCII, space-padded)]
    [N bytes: payload]

### 2.2 Packet Format (Client-side)

The Lua client sends a different wire format (2-byte length prefix):

    [2 bytes: length (big-endian)]
    [N bytes: payload where payload[1:4] = opcode, payload[5:] = data]

This is handled transparently by the Lua `SlimProto.lua` module.

### 2.3 Connection Flow

#### Client → Server: HELO

The client initiates the connection by sending a HELO packet.

Payload (binary packed little-endian):

    [1]   Device ID      (e.g. 0x07 = receiver, 0x12 = squeezebox)
    [1]   Revision       (e.g. 0x00)
    [6]   MAC address    (6 bytes, e.g. c4:ef:bb:c6:c4:a5)
    [16]  UUID           (16 bytes)
    [2]   WLAN list      (bit flags: bit 14 = reconnect)
    [4]   bytesReceivedH (high 32 bits of received bytes)
    [4]   bytesReceivedL (low 32 bits)
    [2]   Language code  (ISO 639-1, uppercase, e.g. "DE")
    [*]   Capabilities   (comma-separated string, e.g. "Model=squeezeplay,...")

#### Server → Client: vers

Server sends its version string.

    Opcode: "vers"
    Payload: version string (e.g. "9.0")

#### Server → Client: setd (name)

Server sends the server's display name.

    Opcode: "setd"
    Payload: [1 byte: flags] [name string]

#### Server → Client: strm (stream/status)

Server sends stream metadata or status.

    [1]   Command byte
    [4]   Track number (big-endian)
    [1]   Title length
    [*]   Title
    [1]   Artist length
    [*]   Artist
    [1]   Album length
    [*]   Album
    [4]   Duration (big-endian)
    [2]   Unknown flags (0xAC, 0x44)
    [2]   Unknown (0x0000)
    [2]   Unknown (0x0018)

The original LMS sends `strm #28` roughly every 4 seconds as a keepalive.

#### Client → Server: STAT

Client acknowledges every server packet with a STATUS response.

    Opcode: "STAT"
    Payload includes: 4-byte event code, timestamps, buffer status counters.

The event code mirrors the opcode being acknowledged (e.g. `"vers"`, `"setd"`,
`"STMf"`, `"STMc"`).

#### Client → Server: DSCO

Client disconnects.

    [1] Reason code (1 = normal disconnect)

#### Client → Server: BYE!

Client bids farewell (alternative to DSCO).

#### Client → Server: aude

Audio decode enable/disable.

#### Server → Client: audg

Audio gain settings (master volume, left/right balance).

#### Server → Client: strm (keepalive)

The original LMS sends `strm #28` periodically (~every 4 seconds) as a
keepalive. The client responds with STAT for each.

### 2.4 State Machine

The client tracks a simple state: UNCONNECTED → CONNECTING → CONNECTED.
When it receives any packet, it sets state to CONNECTED and clears any
connection failure flag. If the read pump times out (35s), it disconnects.

---

## 3. CometD / Bayeux Protocol (HTTP port 9000, path /cometd)

This is the web API protocol. The client sends JSON over HTTP POST to
`/cometd`. The server responds with JSON. The client uses TWO separate
SocketHttp instances:

- `chttp` ("Chunked") — used for handshake, connect, subscribe, and
  server-pushed events (streaming mode). All long-lived connections.
- `rhttp` ("Request") — used for one-shot request-response calls
  (e.g. `/slim/request`, `/slim/subscribe`). Each call may use a
  fresh connection or a pooled connection.

### 3.1 Request Format

Content-Type: `text/json`

Body is ALWAYS a JSON array, even for a single request:

```json
[{"channel": "...", "clientId": "...", ...}]
```

### 3.2 Response Format

Server responds with HTTP 200 and Content-Type `application/json`.
Body is a JSON array with one element per request:

```json
[{"channel": "...", "successful": true, "clientId": "...", ...}]
```

### 3.3 Handshake (`/meta/handshake`)

**Client → Server:**

```json
[{
  "channel": "/meta/handshake",
  "version": "1.0",
  "supportedConnectionTypes": ["streaming"],
  "ext": {
    "rev": "<jive_version>",
    "uuid": "<client_uuid>",
    "mac": "<client_mac>"
  }
}]
```

**Server → Client (success):**

```json
[{
  "channel": "/meta/handshake",
  "successful": true,
  "version": "1.0",
  "minimumVersion": "1.0",
  "supportedConnectionTypes": ["long-polling", "callback-polling"],
  "clientId": "<server-assigned-id>",
  "advice": {
    "reconnect": "retry",
    "interval": 0,
    "timeout": 60000
  }
}]
```

Key fields:
- `clientId` — server-assigned, opaque string used in all subsequent requests
- `advice.timeout` — NOT 0; the original LMS sends 60000 (60s). This tells
  the client how long to wait before retrying the connect.
- `supportedConnectionTypes` — the original LMS sends `["long-polling",
  "callback-polling"]`, NOT `["streaming"]`.

### 3.4 Connect (`/meta/connect`)

Sent as a BATCH with `/meta/subscribe` in a single POST:

```json
[{
  "channel": "/meta/connect",
  "clientId": "<clientId>",
  "connectionType": "streaming"
},
{
  "channel": "/meta/subscribe",
  "clientId": "<clientId>",
  "subscription": "/<clientId>/**"
}]
```

**Server → Client:**

```json
[{
  "channel": "/meta/connect",
  "successful": true,
  "clientId": "<clientId>",
  "advice": {
    "reconnect": "retry",
    "interval": 0,
    "timeout": 0
  }
},
{
  "channel": "/meta/subscribe",
  "successful": true,
  "clientId": "<clientId>
}]
```

After receiving the connect response, the client transitions to CONNECTED state
and calls the `cometConnected` callback.

### 3.5 Subscribe (`/slim/subscribe`)

Used to subscribe to server events. May be sent via `rhttp` (not `chttp`).

```json
[{
  "channel": "/slim/subscribe",
  "id": <number>,
  "data": {
    "request": ["", ["serverstatus", 0, 50, "subscribe:60"]],
    "response": "/<clientId>/slim/serverstatus"
  }
}]
```

### 3.6 Request (`/slim/request`)

Used to send arbitrary requests to the server. May be sent via `rhttp`.

```json
[{
  "channel": "/slim/request",
  "id": <number>,
  "data": {
    "request": ["", ["artworkspec", "add", "40x40_m", "squeezeplayskin"]],
    "response": "/<clientId>/slim/request"
  }
}]
```

### 3.7 Server Status (`/slim/serverstatus`)

The server responds to a `/slim/subscribe` for `serverstatus` with events:

```json
[{
  "channel": "/<clientId>/slim/serverstatus",
  "id": <number>,
  "data": {
    "version": "9.0",
    "uuid": "...",
    "name": "...",
    "ip": "...",
    "port": "9000",
    "cliport": "9090",
    "compatible": true,
    ...
  }
}]
```

### 3.8 HTTP Response Headers

The original LMS sets:

    Content-Type: application/json
    Connection: close   (on the handshake response)

Setting `Connection: close` on the handshake response forces the client to
close the TCP connection after receiving the handshake. The follow-up connect
request then opens a fresh TCP connection, avoiding state-machine races in
the Lua SocketHttp implementation.

### 3.9 SocketHttp State Machine (Lua Client)

The client's `chttp` and `rhttp` each have their own state machine:

**Send states:**
- `t_sendDequeue` → pop next request from send queue
  - If connected: `t_sendRequest`
  - If not connected: `t_sendResolve` → `t_sendConnect` → `t_sendRequest`
- `t_sendRequest` → write HTTP request to TCP socket
- `t_sendComplete` → move request from send-request to recv-queue
  → if recv-state is `t_recvDequeue`, re-enter it

**Receive states:**
- `t_recvDequeue` → pop next request from recv queue
  - If request found: `t_rcvHeaders`
  - If no request: install idle-close read handler (closes socket on ANY data arrival)
- `t_rcvHeaders` → parse HTTP response headers
- `t_rcvResponse` → read response body via pump/sink
- `t_recvComplete` → `self:socketInactive()`, back to `t_recvDequeue`

**Critical race condition:**

After `t_recvDequeue` installs the idle handler (recv queue empty), if data
arrives on the socket before the next queued request's `t_sendComplete` moves
it into the recv queue, the idle handler closes the socket ("idle close").

This race is why `Connection: close` on the handshake response matters:
it closes the socket, so `t_recvDequeue` checks `self:connected()` (false)
and skips installing the idle handler. The next fetch opens a fresh TCP
connection.

### 3.10 Streaming vs Long-Polling

The client requests `"connectionType": "streaming"` in the connect request,
regardless of what the server advertises as supported. The server responds
immediately (timeout=0) and keeps the TCP connection open. Subsequent server
events (e.g. serverstatus) are delivered as new JSON arrays written to the
same HTTP response stream.

Key points:
- The client always uses `"connectionType": "streaming"` — this is hardcoded
  in the Lua `Comet.lua` module, not negotiated.
- The server's `supportedConnectionTypes` in the handshake response does not
  affect the connection type the client uses.
- The connect response `advice.timeout` is ignored when using streaming mode.
- Server-pushed events arrive on the same TCP connection (`chttp`) as
  additional complete JSON arrays, one per event.

### 3.11 CometRequest Sink-Mode Selection

`CometRequest` (a subclass of `RequestHttp`) selects the response data
delivery mode in `t_getResponseSinkMode()`:

- If the server response has a `Transfer-Encoding` header (typically
  `chunked`), the client uses `'jive-by-chunk'` mode — each chunk of the
  streaming response is delivered individually to the JSON decoder sink.
- Otherwise, the client uses `'jive-concat'` mode — the full response body
  is concatenated and delivered as a single block.

In either mode, the raw JSON string passes through `jsonfilters.decode`
before reaching the user-provided sink callback.

### 3.12 advice Field Reference

The `advice` object in CometD responses controls client behavior:

| Field | Handshake | Connect | Description |
|-------|-----------|---------|-------------|
| `reconnect` | `"retry"` | `"retry"` | Client should retry on disconnect |
| `interval` | `0` | `0` | Delay (ms) before reconnect attempt |
| `timeout` | `60000` | `0` | Handshake: suggested long-poll timeout for subsequent connects. Connect: how long server will hold response (0 = respond immediately) |

### 3.13 Successful LMS Connection Timeline

```
 0.000  Client creates Comet object, subscribes to /slim/serverstatus
 0.001  connectToServer() called
 0.001  SlimProto: disconnect (previous), connect, HELO sent
 0.002  SlimProto: vers/setd/strm received, STAT sent
 0.046  setEndpoint(state=UNCONNECTED, 192.168.1.8, 9000, /cometd)
 0.047  connect(state=UNCONNECTED)
 0.047  _handshake() → HTTP POST to /cometd
 0.047  state → CONNECTING
 0.048  _handshake OK, clientId: "e318f3d0"
 0.048  _connect() → HTTP POST to /cometd (connect + subscribe batch)
 0.048  _response, /meta/connect id= OK
 0.048  Sending pending request(s): /slim/subscribe serverstatus
 0.048  state → CONNECTED
 0.048  request artworkspec (queued via pending_reqs)
 0.049  Sending pending request(s): /slim/request
 0.049  ***serverConnected SlimServer {traube3}
 0.049  _response, /meta/subscribe id= OK
 0.052  _response, /slim/subscribe id=1 OK
 0.089  _response, /e318f3d0/slim/serverstatus id=1 OK
 0.089  notify callbacks for /slim/serverstatus
```

The entire connection sequence from handshake to serverstatus callback
completes in ~42ms against a real LMS on the same LAN.

---

## 4. JSON-RPC Protocol (HTTP port 9000, path /jsonrpc)

Used for simpler request-response calls (e.g. fetching settings). Less
commonly used than CometD.

### 4.1 Request

```json
{
  "id": <number>,
  "method": "<method_name>",
  "params": [...]
}
```

### 4.2 Response

```json
{
  "id": <number>,
  "result": { ... }
}
```

---

## 5. CLI Protocol (TCP port 9090)

Text-based telnet-style protocol for server administration. Not used during
client connection flow.

---

## 6. Ports Summary

| Port | Protocol | Purpose |
|------|----------|---------|
| 3483 | UDP      | Server discovery (broadcast/multicast) |
| 3483 | TCP      | SlimProto control + audio streaming |
| 9000 | HTTP/TCP | CometD web API |
| 9000 | HTTP/TCP | JSON-RPC (same port, different path) |
| 9090 | TCP      | CLI administration (not used by SqueezePlay) |

---

## 7. Lua Client Networking Architecture

The SqueezePlay Lua client uses a cooperative multitasking networking layer
built around a single NetworkThread event loop. All network I/O is
non-blocking and driven by `socket.select()`.

### 7.1 Socket Class Hierarchy

```
Socket (jive.net.Socket)
  └─ SocketTcp (jive.net.SocketTcp)
       └─ SocketHttp (jive.net.SocketHttp)
            └─ SocketHttpQueue (jive.net.SocketHttpQueue, used only by HttpPool)
```

| Class | File | Purpose |
|-------|------|---------|
| `Socket` | `Socket.lua` | Abstract base: read/write handler registration, lifecycle, activity tracking |
| `SocketTcp` | `SocketTcp.lua` | TCP connection: connect, disconnect, connected-state tracking |
| `SocketHttp` | `SocketHttp.lua` | HTTP client: send queue, recv queue, state machine (940 lines) |
| `SocketHttpQueue` | `SocketHttpQueue.lua` | Wraps SocketHttp with a dequeue callback for HttpPool |

### 7.2 Socket (base class)

**File:** `jive/net/Socket.lua`

Core read/write handler management via the **pump pattern**:

- **`t_addRead(pump, timeout)`**: Creates a Task wrapping `readPump`, registers
  with NetworkThread via `jnt:t_addRead(sock, task, timeout)`. The task loop
  calls `readPump(networkErr)` repeatedly:
  - If pump returns `true` → pump is done, yield via `Task:yield(false)`
  - If pump returns `false` → more data to process, loop synchronously
  - On error → calls `self:close("task error")`
- **`t_addWrite(pump, timeout)`**: Identical pattern for write side.
- **`t_removeRead()` / `t_removeWrite()`**: Unregister from NetworkThread.
- **`close()`**: Removes both handlers, closes raw socket, notifies
  NetworkThread of inactivity.
- **`socketActive()` / `socketInactive()`**: Track network activity state
  (used for power management).

### 7.3 SocketTcp

**File:** `jive/net/SocketTcp.lua`

TCP-specific connection management:

- **`t_connect()`**: Creates a non-blocking LuaSocket TCP socket
  (`settimeout(0)`) and initiates `connect()`. Returns 1 on success or
  in-progress, nil on failure.
- **`connected()`**: Main-thread-safe boolean check. Updated via `t_setConnected()`.
- **Connection detection**: The `t_addRead` / `t_addWrite` overrides wrap the
  pump in a closure that calls `t_setConnected(true)` on the first successful
  I/O before calling the original pump.
- **`close()`**: Sets `connected = false` before closing the raw socket.

### 7.4 SocketHttp State Machine

**File:** `jive/net/SocketHttp.lua` (940 lines)

Each `SocketHttp` instance (used for both `chttp` and `rhttp`) maintains
dual state machines: one for sending requests and one for receiving responses.

**Send states:**

```
t_sendDequeue → pop next request from send queue
  ├─ If connected() → t_sendRequest
  └─ If not connected() → t_sendResolve → t_sendConnect → t_sendRequest

t_sendRequest → write HTTP request line + headers + body to TCP socket
t_sendComplete → move request from send-request to recv-queue
  └─ if recv-state is t_recvDequeue → re-enter t_recvDequeue
```

**Receive states:**

```
t_recvDequeue → pop next request from recv queue
  ├─ If request found → t_rcvHeaders
  └─ If no request → install idle-close read handler

t_rcvHeaders → parse HTTP response headers (status line + header lines)
  └─ On completion → t_rcvResponse

t_rcvResponse → read response body via pump/sink
  └─ On completion → t_recvComplete

t_recvComplete → self:socketInactive()
  └─ → t_recvDequeue (loop back)
```

**Critical race condition:**

After `t_recvDequeue` installs the idle-close handler (when recv queue is
empty), if data arrives on the socket before the next queued request's
`t_sendComplete` moves it into the recv queue, the idle handler closes the
socket ("idle close"). The race windows are:

1. `t_sendDequeue` pops a request but has not yet written to the socket
   → recv queue still empty → idle handler installed on recv side
2. Server responds quickly — data arrives on socket BEFORE
   `t_sendComplete` is reached to move request into recv queue
3. Idle handler fires → socket closed → response lost

**Why `Connection: close` on handshake fixes this:**

When the server sends `Connection: close`, the client receives the complete
response body and the TCP connection closes (EOF). `t_recvDequeue` checks
`self:connected()` — since the socket is closed, it returns false, and the
idle handler is NOT installed. The next `fetch()` call opens a fresh TCP
connection in `t_sendResolve`.

### 7.5 HttpPool (rhttp Pool)

**File:** `jive/net/HttpPool.lua`

Manages a pool of persistent HTTP connections for request-response calls
(`rhttp`). The pool distributes queued requests across multiple
`SocketHttpQueue` instances.

**Key behavior:**
- Maintains `reqQueue` (FIFO array of `RequestHttp` instances)
- Uses `quantity` (default 1) `SocketHttpQueue` instances, each wrapping a
  `SocketHttp`
- `queue(request)`: Appends to `reqQueue`, kicks all connections via
  `t_sendDequeueIfIdle()`
- `t_dequeue()`: Removes next request from queue. If queue is empty, starts a
  one-shot timer (`KEEPALIVE_TIMEOUT = 60000ms`) to close idle connections.
- Each `SocketHttpQueue` calls back into the pool's `t_dequeue()` when it is
  ready for the next request.

**Key difference from raw SocketHttp:**

| Aspect | HttpPool (rhttp) | Raw SocketHttp (chttp) |
|--------|-----------------|----------------------|
| Connection count | Multiple (pooled) | Single |
| Request queue | Internal FIFO | Per-instance send queue |
| Idle timeout | 60s auto-close | None |
| Dequeue callback | `SocketHttpQueue` → pool `t_dequeue()` | N/A |

### 7.6 NetworkThread Event Loop

**File:** `jive/net/NetworkThread.lua`

A dedicated Lua coroutine (thread) for ALL network I/O, using LuaSocket's
`socket.select()` for event-driven multiplexing.

**Event loop (`_run`):**

```
loop:
  timeoutSecs = compute_timeout()
  socket.select(t_readSocks, t_writeSocks, timeoutSecs)
  for each readable/writable socket:
    update lastSeen timestamp
    task:addTask(pump_callback)   // enqueue pump in Task system
  for each socket with timeout > 0 and (now - lastSeen > timeout):
    task:addTask("inactivity timeout")
  Task:yield(true)                // yield to main thread
end loop
```

**Key details:**
- Single-threaded cooperative model — no mutexes needed for socket state.
- Communication with the main (UI) thread via mutexed function queues.
- Task priority: each socket has a `priority` field that influences Task
  scheduling — higher priority sockets run first.
- Timeout: each socket can have a per-activity timeout. On expiry, an
  `"inactivity timeout"` event is enqueued, which the receiving pump handles
  by calling `close()`.
- Activity tracking: `networkActive()` / `networkInactive()` calls are
  reference-counted for power management (e.g. prevent sleep during playback).

### 7.7 RequestHttp — Request/Response Handling

**File:** `jive/net/RequestHttp.lua`

Encapsulates a single HTTP request: method, URI, headers, body source, and
the response sink. Used as the base class for `CometRequest`.

**Request construction:**
- Parses URI via `socket.url.parse(uri, defaults)`
- Builds request string: `GET /path?query` or `POST /path`
- Merges headers from options, always sets `Host` header

**Response header parsing (`t_setResponseHeaders`):**
- Lowercases ALL header keys into a `mappedHeaders` table
  (e.g. `Content-Type` → `content-type`)
- Header lookup is case-insensitive via `t_getResponseHeader(key)`:
  `return mappedHeaders[string.lower(key)]`

**Response body dispatch (`t_setResponseBody`):**
- On HTTP 200: calls user sink with data
- On redirect (301, 302, 307): follows `Location` via a new SocketHttp
  (max 5 redirects)
- On error: calls sink with error

### 7.8 CometRequest — CometD-specific Request

**File:** `jive/net/CometRequest.lua`

Subclass of `RequestHttp` for CometD JSON POST requests.

**Body source (`_getBodySource`):**
- Creates an ltn12 source chain: `jsonfilters.encode` → function returning
  the POST body once
- Content-Type: `text/json`

**Sink-mode selection:**
```lua
function t_getResponseSinkMode(self)
    if self:t_getResponseHeader("Transfer-Encoding") then
        return 'jive-by-chunk'   -- streaming mode
    else
        return 'jive-concat'     -- concatenate all data
    end
end
```

**JSON decoding (`t_setResponseBody`):**
- Only processes body on HTTP 200
- Wraps the user sink in an ltn12 filter chain: `jsonfilters.decode → sink`
- `jsonfilters.decode` converts raw JSON string → Lua table before passing
  to the original sink callback
- On non-200, propagates error through the sink

---

## 8. Go Server Implementation Notes

### 8.1 Expected CometD Response Format

The server must respond to CometD requests with these exact JSON structures.

**Handshake response (top-level fields, not nested in `data`):**

```json
{
  "channel": "/meta/handshake",
  "successful": true,
  "version": "1.0",
  "minimumVersion": "1.0",
  "supportedConnectionTypes": ["long-polling", "callback-polling"],
  "clientId": "<unique-id>",
  "advice": {
    "reconnect": "retry",
    "interval": 0,
    "timeout": 60000
  }
}
```

**Connect response:**

```json
{
  "channel": "/meta/connect",
  "successful": true,
  "clientId": "<clientId>",
  "advice": {
    "reconnect": "retry",
    "interval": 0,
    "timeout": 0
  }
}
```

**HTTP headers:**
- Handshake response MUST include `Connection: close` to avoid the
  SocketHttp race condition.
- Connect response and subsequent responses should NOT include
  `Connection: close` so the TCP connection stays open for pushed events.
- Content-Type is always `application/json`.

### 8.2 Connection Flow Summary

```
1. UDP Discovery    (port 3483, optional — client may already know server IP)
2. SlimProto HAND   (TCP 3483) — client sends HELO, server responds vers/setd/strm
3. CometD Handshake (HTTP 9000 /cometd) — Connection: close
4. CometD Connect   (HTTP 9000 /cometd, new TCP) — keep-alive for streaming
5. CometD Subscribe (HTTP 9000 /cometd, via rhttp)
6. CometD Events    (HTTP 9000 /cometd, pushed via chttp streaming)
```
