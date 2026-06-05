# WsDriver Protocol Specification

**Version:** 1.0.0
**Status:** Reference (extracted from the gemini-testing/selenoid server and gemini-testing/testplane client reference implementations)
**Category:** Browser Automation Transport
**Sub-protocol identifier:** `wsdriver` (WebDriver over WebSockets)

---

## Table of Contents

1. [Introduction & Design Goals](#1-introduction--design-goals)
2. [Conformance & Terminology](#2-conformance--terminology)
3. [Session Lifecycle & Capability Negotiation](#3-session-lifecycle--capability-negotiation)
4. [Wire Protocol & Binary Framing](#4-wire-protocol--binary-framing)
5. [Message Schemas & Payload Mapping](#5-message-schemas--payload-mapping)
6. [Error Model](#6-error-model)
7. [Reference Implementation Analysis](#7-reference-implementation-analysis)
8. [Appendix A — Constants & Tunables](#appendix-a--constants--tunables)
9. [Appendix B — Canonical Wire Examples](#appendix-b--canonical-wire-examples)

---

## 1. Introduction & Design Goals

### 1.1. Abstract

**WsDriver** is a binary, request/response, multiplexed sub-protocol over
WebSocket framing that transports W3C WebDriver Classic commands between a
WebDriver client and a WebDriver-compatible hub (Selenoid). The protocol
preserves the request/response *semantics* of WebDriver Classic (path,
method, JSON body, HTTP-style status code) while replacing its HTTP/1.1
*transport* with a single, long-lived, optionally-compressed WebSocket
connection per session.

The protocol is currently produced as **version 1** of the wire format.
The single-octet version preamble is reserved to enable in-place evolution
of the framing structure. Forward-tolerance is **asymmetric**, however: a
v1 *server* is **not** forward-tolerant — it rejects any frame whose
`Version ≠ 1` by closing the connection (§4.3). A v1 *client* can send v1
requests and read v1 responses, and on the read side it tolerates frames
of an unknown version by silently discarding them (§7.2.4) rather than
failing; it simply cannot *understand* a future v2 message. Genuine
cross-version interop therefore depends on a future version negotiating
down to v1, not on a v1 server accepting v2 frames.

### 1.2. Protocol Motivation

WebDriver Classic mandates one outbound HTTP request per command. In typical
testing workloads — where a single test session may issue thousands of
short commands ("find element", "click", "execute script", "get text") —
the per-command overhead of HTTP/1.1 dominates the on-wire cost:

* **Connection setup amortization is poor.** Even with `keep-alive`, the
  ALPN/TLS handshake amortizes only across the lifetime of one TCP
  connection; intermediate proxies, NATs, and idle timeouts frequently
  drop the connection between commands, forcing a fresh TCP+TLS round.
* **Header bloat.** Each WebDriver request carries a full HTTP request
  line, `Host`, `User-Agent`, `Content-Type`, `Content-Length`,
  `Accept`, etc., plus a full HTTP response status line and headers. For
  a typical `{"value":null}` response (14 bytes) the headers can exceed
  the payload by an order of magnitude.
* **No application-level compression by default.** WebDriver responses
  carrying serialized DOM snapshots or large element trees are sent
  uncompressed unless a `Content-Encoding` is negotiated per request.
* **Polling latency.** Each command pays at least one full RTT before
  the first response byte; pipelining is forbidden by RFC 7230 §6.3.2
  in any practical sense.

WsDriver replaces this with a single WebSocket upgrade followed by a
stream of binary frames. The benefits observed in the reference
implementations are:

| Property                | HTTP/1.1 WebDriver         | WsDriver v1                                  |
| ----------------------- | -------------------------- | -------------------------------------------- |
| Connection setup        | per-command (worst case)   | once per session                             |
| Per-message framing     | full HTTP request/response | 9-byte fixed header + null-terminated path   |
| Body serialization      | JSON text                  | JSON text, optionally `gzip` or `zstd`       |
| Compression negotiation | per-request HTTP header    | once, at WebSocket handshake                 |
| Concurrent commands     | one in-flight per conn.    | multiplexed via `RequestID` correlation      |
| Half-duplex polling     | yes                        | no (full-duplex; server may push errors)     |

### 1.3. Key Performance Benefits

The reference implementations are designed around three measurable wins:

1. **Network packet reduction.** A WsDriver command for `GET /status`
   collapses to a single 15-byte binary frame: `01 02 00 00 00 01 00 00 73
   74 61 74 75 73 00`. The equivalent HTTP/1.1 GET typically exceeds 200
   bytes once host, user-agent, accept, and connection headers are
   serialized.
2. **Native binary serialization.** Numeric fields (`Version`,
   `RequestID`, `RequestMethod`, `ResponseStatus`) use fixed-width
   big-endian integers rather than ASCII representations.
3. **Opportunistic, threshold-gated compression.** The server compresses
   the response body only when (a) its uncompressed length exceeds
   `1024` bytes (the WsDriver compression threshold, kept below a typical
   Ethernet MTU of 1500), and (b) the client advertised support for at
   least one compression codec during the WebSocket upgrade. This avoids
   paying compression overhead on small "ack-style" responses while
   still capturing the substantial wins on bulky payloads such as
   element trees or page sources.

---

## 2. Conformance & Terminology

The key words *MUST*, *MUST NOT*, *REQUIRED*, *SHALL*, *SHALL NOT*,
*SHOULD*, *SHOULD NOT*, *RECOMMENDED*, *MAY*, and *OPTIONAL* in this
document are to be interpreted as described in RFC 2119.

* **Server**: The hub that brokers WebDriver sessions and terminates
  the WsDriver WebSocket (reference: Selenoid).
* **Client**: The WebDriver client that issues commands (reference:
  Testplane).
* **Upstream WebDriver**: The backend HTTP WebDriver implementation
  (e.g. `chromedriver`, `geckodriver`) that the server proxies to.
* **Frame**: A single WebSocket *binary* application message (i.e. a
  fully-reassembled `BinaryMessage` per RFC 6455 §5.6), independent of
  the underlying TCP segmentation.
* **Command Message**: A frame whose `MessageType` is `Request`.
* **Response Message**: A frame whose `MessageType` is `Response`.

---

## 3. Session Lifecycle & Capability Negotiation

### 3.1. Phase 1 — Standard WebDriver Session Creation

The client *MUST* initiate every session via the standard W3C WebDriver
`POST /session` (or `POST /wd/hub/session`) over HTTP. WsDriver does
*not* define a session-creation handshake; it only defines a transport
that *replaces HTTP for the remainder of the session’s lifetime*.

### 3.2. Phase 2 — Capability Injection

WsDriver capability injection requires a **W3C-shape** session response —
one carrying a `value.capabilities` object. The reference server only
injects the capabilities into that object; a legacy JSONWP response
(e.g. Chrome < 75, with `sessionId` at the top level and no
`value.capabilities`) receives no WsDriver capabilities, so WsDriver is
silently unavailable for such sessions and the client falls back to plain
HTTP WebDriver.

When the server constructs the W3C-shape response to `POST /session`, it
*MUST* inject the following two capabilities into the `value.capabilities`
object, on top of whatever the upstream WebDriver returned:

```json
{
  "value": {
    "sessionId": "<session-uuid>",
    "capabilities": {
      "se:wsdriver":        "ws://<hub-host>/wsdriver/<session-uuid>/",
      "se:wsdriverVersion": "1"
    }
  }
}
```

Field semantics:

| Capability             | Type   | Required | Meaning                                                                 |
| ---------------------- | ------ | -------- | ----------------------------------------------------------------------- |
| `se:wsdriver`          | string | yes      | Absolute WebSocket URL (`ws://` or `wss://`) at which the server accepts the wsdriver upgrade for this session. |
| `se:wsdriverVersion`   | string | yes      | Comma-and-space-separated (`", "`) list of supported wsdriver wire-protocol versions as ASCII decimal integers. The reference server advertises `"1"`. |

The client *MUST* tolerate the absence of these capabilities and fall
back to plain HTTP WebDriver in that case. The reference client throws
`WSDriverError("Couldn't determine wsdriver endpoint")` if `se:wsdriver`
is missing and `WSDriverError("Couldn't determine wsdriver supported
versions")` if `se:wsdriverVersion` is missing — both are *fatal at
agent construction*, **not** at session creation, so the rest of the
HTTP-based session continues to function.

The reference client parses `se:wsdriverVersion` as:

```ts
caps["se:wsdriverVersion"].split(", ").map(Number).filter(Boolean)
```

The client *MUST* select a version that appears in this list; the
reference client implicitly selects version `1`.

### 3.3. Phase 3 — Protocol Upgrade & Compression Negotiation

Upon detecting valid WsDriver capabilities, the client *MUST*:

1. Open a WebSocket connection to the `se:wsdriver` URL.
2. During the HTTP `Upgrade: websocket` handshake request, include the
   `wsdriver-accept-encoding` header listing the compression codecs the
   client can *decode*, in **preference order**, separated by `", "`
   (comma-space). Recognized tokens are `zstd` and `gzip`.

   The reference client computes this list as:

   ```ts
   const clientSupportedCompressionTypes =
       ["zstd" in process.versions ? "zstd" : null, "gzip"].filter(Boolean);
   ```

   so a Node.js ≥ 22 client sends `wsdriver-accept-encoding: zstd, gzip`
   and an older client sends `wsdriver-accept-encoding: gzip`.

The server, upon a successful WebSocket upgrade, *MUST* include the
following header in the `101 Switching Protocols` response, advertising
the codecs it can *encode*:

```
WsDriver-Accept-Encoding: zstd, gzip
```

> **Note on header case.** WebSocket header names are case-insensitive
> per RFC 7230 §3.2; the server emits the canonical form
> `WsDriver-Accept-Encoding` while the client emits
> `wsdriver-accept-encoding`. Implementations *MUST* match
> case-insensitively.

The negotiated compression codec, used *by the client when sending
request bodies*, is determined by intersecting the client’s preference
list with the server-advertised list, in **client preference order**.
The reference client logic is:

```
serverAcceptEncodings = server "WsDriver-Accept-Encoding" header, split by ", "
for codec in clientSupportedCompressionTypes:
    if codec in serverAcceptEncodings:
        return codec   # 'zstd' wins over 'gzip' if both are mutually supported
return None
```

The negotiated codec, used *by the server when sending response bodies*,
is symmetric: the server prefers `zstd` over `gzip` if the client’s
`WsDriver-Accept-Encoding` header advertises both.

The server *MUST NOT* require the client to advertise any compression
codec; if the client sends no `wsdriver-accept-encoding` header (or an
empty one), the server simply emits all response bodies uncompressed.

### 3.4. Phase 4 — Fallback & Error Conditions During Upgrade

The client *SHOULD* classify upgrade failures as retryable or
non-retryable as follows. The protocol does **not** mandate any specific
retry count, backoff schedule, or interval — those are entirely
implementation-defined. A client *MAY* retry a retryable failure as many
(or as few) times as it likes; the reference client retries with
exponential backoff using the values in Appendix A
(`WSD_CONNECTION_RETRIES`, `WSD_CONNECTION_RETRY_BASE_DELAY`).

| Condition                                             | Retryable? |
| ----------------------------------------------------- | ---------- |
| Network reset, DNS failure, connection refused        | Yes        |
| HTTP `4xx` response to the WebSocket Upgrade request  | No         |
| HTTP `5xx` response to the WebSocket Upgrade request  | Yes        |
| Upgrade exceeds the per-attempt connection timeout    | Yes        |

The reference client is organized in two layers, each with its own error
hierarchy:

* the **transport layer** (`WsConnection`), whose errors are named
  `WsConnection*` — e.g. `WsConnectionEstablishmentError`,
  `WsConnectionTerminatedError`, `WsConnectionTimeoutError`,
  `WsConnectionBreakError`; and
* the **application layer** (`WSDriverRequestAgent`), whose errors are
  named `WSDriver*` (see §6.3) and which wrap the transport-layer errors.

The conditions in the table above are raised at the transport layer. The
4xx-versus-5xx retryability rule is encoded in
`WsConnectionEstablishmentError.isRetryable()`:

```ts
isRetryable(): boolean {
  if (this.code && this.code >= 400 && this.code < 500) return false;
  return true;
}
```

The reference client does *not* automatically fall back to plain HTTP if
the WebSocket upgrade ultimately fails after retries; the agent throws
and the test fails. Implementations *MAY* choose to fall back; the
protocol does not preclude it because the original HTTP WebDriver
endpoint remains live for the session.

### 3.5. Session Termination

The WsDriver connection is bound to the lifetime of the underlying
WebDriver session. The client *MUST* close the WebSocket cleanly when it
issues `DELETE /session/<id>`. The server *MUST* tolerate either
ordering (DELETE-then-close or close-then-server-side timeout).

There are two distinct "session gone" cases, handled differently:

1. **Session unknown at upgrade time.** If the session referenced by the
   upgrade URL (`/wsdriver/<sessionId>/`) is not found when the WebSocket
   upgrade is attempted, the server *MUST NOT* upgrade the connection.
   Instead it *MUST* answer the upgrade HTTP request with the standard
   W3C WebDriver `invalid session id` error response over plain HTTP — the
   same JSON-shaped error a classic WebDriver endpoint returns for an
   unknown session — and the `101 Switching Protocols` never happens. From
   the client's point of view this is an HTTP `4xx`/error on the upgrade
   (non-retryable per §3.4).
2. **Session reaped mid-connection.** If a WsDriver command arrives over an
   *already-upgraded* socket for a session that the server has since reaped
   (e.g. due to idle timeout), the server *MUST NOT* close the WebSocket;
   instead, it *MUST* reply on the same frame with an "invalid session id"
   Response Message (see §6.2.1) and continue serving subsequent frames on
   the same socket.

The server *MUST* close the WebSocket with WebSocket close code
`1003` (Unsupported Data) if the client sends a `TextMessage`, and with
`1007` (Invalid Frame Payload Data) if the client sends a binary frame
that fails framing validation (see §6.1).

### 3.6. Liveness — Application-level Ping/Pong

The WsDriver layer itself defines no application ping. The reference
client runs the standard WebSocket control-frame `PING/PONG` cycle at
`WS_PING_INTERVAL = 15 000 ms`, expects the `PONG` within
`WS_PING_TIMEOUT = 10 000 ms`, tolerates one missed `PONG`, and
considers the connection broken after `WS_PING_MAX_SUBSEQUENT_FAILS = 2`
missed pongs, triggering a reconnect. The server, by virtue of the
gorilla/websocket default handler, automatically responds to `PING` with
`PONG`. Implementations of either role *MUST* respond to a peer’s
control `PING` per RFC 6455 §5.5.2.

---

## 4. Wire Protocol & Binary Framing

### 4.1. Transport Layer

* **Underlying transport:** RFC 6455 WebSocket, with no `Sec-WebSocket-Protocol`
  sub-protocol negotiated. WsDriver does *not* register a WebSocket
  sub-protocol token; the upgrade target URL (`/wsdriver/<id>/`) and the
  capabilities exchange in §3 are sufficient to identify WsDriver
  traffic.
* **Per-message-deflate:** The reference server *does not* enable RFC 7692
  `permessage-deflate`. Compression is performed at the WsDriver
  application layer instead, so that the framing header itself remains
  inspectable without first running the deflater, and so the server can
  refuse to spend CPU compressing payloads below a threshold.
* **Required message type:** Every WsDriver application message *MUST*
  be a WebSocket `BinaryMessage` (opcode `0x2`). A peer that receives a
  `TextMessage` (opcode `0x1`) *MUST* terminate the connection with
  WebSocket close code `1003` (Unsupported Data).
* **Maximum frame size:** Not constrained by the protocol; the reference
  server pre-grows a 256 KiB buffer per connection but does not impose a
  hard cap.

### 4.2. Frame Structure (overview)

Both Command and Response messages share an identical **8-byte fixed
header** (`Version` + `Header` + `RequestID` + `Method`/`Status`)
followed by a null-terminated UTF-8 path and an optional, possibly
compressed payload body. With the path's NUL terminator the minimum frame
is therefore 9 bytes. Big-endian byte order (network order) is used
throughout for multi-byte integer fields.

```
 0               1               2               3
 0 1 2 3 4 5 6 7 0 1 2 3 4 5 6 7 0 1 2 3 4 5 6 7 0 1 2 3 4 5 6 7
+---------------+---------------+-------------------------------+
|    Version    |    Header     |                               |
|     (u8)      |     (u8)      |                               |
+---------------+---------------+         RequestID             |
|                       RequestID (u32, BE)                     |
+-------------------------------+-------------------------------+
|  Method / Status (u16, BE)    |   Path bytes  (UTF-8) ...     |
+-------------------------------+-------------------------------+
| ... Path bytes ... |  0x00  |          Body bytes ...         |
+-------------------------------+-------------------------------+
```

The minimum legal WsDriver frame length is therefore:

```
1 (Version) + 1 (Header) + 4 (RequestID) + 2 (Method|Status) + 1 (NUL) = 9 bytes
```

A peer that receives a frame strictly shorter than 9 bytes *MUST* treat
it as a framing error (close code `1007`).

### 4.3. The Header Byte

The single Header byte at offset 1 packs four fields, MSB to LSB:

| Bits   | Field             | Width    | Notes                                                   |
| ------ | ----------------- | -------- | ------------------------------------------------------- |
| 7..4   | `MessageType`     | 4 bits   | `0 = Request`, `1 = Response`. Other values reserved.   |
| 3..2   | `CompressionType` | 2 bits   | `0 = None`, `1 = GZIP`, `2 = ZSTD`. `3` is reserved.    |
| 1      | `IsJSON`          | 1 bit    | If `1`, the (decompressed) body is `application/json`.  |
| 0      | `IsWsdriverError` | 1 bit    | Set only on Response Messages. See §6.2.                |

Bit packing/unpacking is canonical:

```
header_byte = (MessageType   << 4)
            | (CompressionType << 2)
            | (IsJSON          << 1)
            | (IsWsdriverError << 0)
```

The `IsWsdriverError` bit, when set, indicates that the *server* (not
the upstream WebDriver) generated this response because the protocol
itself failed (e.g. the upstream WebDriver was unreachable, or the body
could not be re-encoded). See §6.2.

The reference server *MUST* reject a Command Message whose:

* `Version` ≠ 1 — error `invalid message version`.
* `MessageType` ≠ `Request` (0) — error `invalid message type`.
* `CompressionType` > 2 — error `unsupported compression type`.

All three conditions trigger a WebSocket close with code `1007` per §6.1.
In particular, rejecting `Version ≠ 1` is what makes a v1 server *not*
forward-tolerant (§1.1): an incoming v2 frame closes the connection rather
than being ignored.

### 4.4. Multiplexing & Correlation — `RequestID`

`RequestID` is a 32-bit unsigned big-endian integer chosen by the
client. It is the **sole correlation identifier** in the protocol; the
server *MUST* echo it verbatim in the matching Response Message.

The client *MUST* generate `RequestID` values that are unique within the
set of *currently in-flight* requests on a given WebSocket connection.
The reference client uses a per-connection monotonic counter that wraps
to `0` after reaching `WS_MAX_REQUEST_ID = 2147483647` (`INT32_MAX`),
yielding effectively unique IDs for any realistic concurrent-request
fan-out. The value `0` is legal as a `RequestID` (the counter is
pre-incremented, so the first ID issued is `1`, but a wrap restarts
from `1` again — `0` is not produced by the reference generator, yet no
peer *MAY* reject a frame solely because its `RequestID` is `0`).

Multiplexing is fully bidirectional from the client’s point of view: it
*MAY* have an arbitrary number of in-flight commands sharing one
WebSocket. The reference client stores pending requests in a
`Record<number, (response) => void>` map and resolves the matching
promise when a Response with that `RequestID` arrives.

Responses *MAY* arrive in any order, irrespective of the order in which
the corresponding Commands were sent. The wire protocol does not mandate
any response ordering and clients *MUST NOT* rely on it.

**Head-of-line blocking.** The reference server processes commands
*serially* on a single read loop per connection (read frame → upstream
HTTP → write response → repeat; see §7.1.2). It does **not** issue
upstream requests in parallel, so a slow command (bounded only by the
upstream HTTP timeout, §6.2.2) delays the *responses* to every later
command queued on the same socket. Clients that need true response
concurrency should open multiple WebSocket connections.

Even with serial server-side processing, pipelining over a single socket
is still a net win: a client need **not** wait for the response to
previous command before sending the next one. Commands stream out back-to-back
and their responses come back as the server works through them, eliminating
the per-command request/response round-trip stall that HTTP/1.1 WebDriver pays.

### 4.5. Compression

#### 4.5.1. Algorithms

Two compression algorithms are defined for version 1:

| Code | Token  | Algorithm | Decoder source of truth                        |
| ---- | ------ | --------- | ----------------------------------------------- |
| 0    | (none) | identity  | n/a                                             |
| 1    | `gzip` | RFC 1952  | reference encoder: `gzip` level 4 (klauspost on server, `zlib.gzip({level:4})` on client) |
| 2    | `zstd` | RFC 8878  | klauspost/compress/zstd on server; Node.js native `zlib.zstdCompress`/`zstdDecompress` on client (requires Node ≥ 22) |

The reference encoders deliberately use **gzip level 4** ("a little bit
worse than the default level 6 but almost twice as fast"); peers
*SHOULD NOT* assume any particular level — gzip is a streaming format
and any conformant decoder *MUST* accept any level.

#### 4.5.2. Threshold

The threshold is a performance heuristic, not a wire constraint. A peer
*SHOULD NOT* compress a body whose uncompressed length does not exceed
`compressionThresholdBytes = 1024` bytes; the reference server compresses
only when the length is strictly greater than `1024`
(`httpBuffer.Len() > compressionThresholdBytes`), and the reference
client applies the same cutoff via `WSD_COMPRESSION_THRESHOLD_BYTES = 1024`.

Because this is only a heuristic, a peer *MAY* compress a body of any
size — including one below the threshold — and a conforming decoder
*MUST* decompress it regardless of size, relying solely on the
`CompressionType` field (never on the body length) to decide whether to
decompress. Compressing small bodies is permitted but pointless, which
is why the reference implementations skip it.

The 1024-byte cutoff is chosen to sit just below a standard 1500-byte
Ethernet MTU, on the rationale that any payload that already fits in a
single TCP segment derives no wire savings from compression and only
pays the CPU and frame-overhead cost.

When a body is *not* compressed (whether because it falls below
threshold, because the peer advertised no codec, or because the message
is an error frame, or by any other reason), the `CompressionType` field
*MUST* be `0` (None) even if the peer is technically capable of compression.

#### 4.5.3. Codec Selection at Encode Time

The encoding peer *MUST* select a codec that the decoding peer
advertised during the WebSocket upgrade (§3.3). The reference
implementations both prefer `zstd` over `gzip` when both are mutually
supported.

If the encoding peer holds zero mutually-supported codecs, it *MUST*
emit the body uncompressed (`CompressionType = 0`).

---

## 5. Message Schemas & Payload Mapping

### 5.1. Command Message (Client → Server)

```
+--------+--------+----------------+----------------+--------------------------+----+--------------------+
| Ver=1  | Header | RequestID (BE) | RequestMethod  | RequestPath (UTF-8)      |0x00| Body (opt., compr.)|
| 1 byte | 1 byte | 4 bytes        | 2 bytes (BE)   | variable, no NUL inside  |    | variable           |
+--------+--------+----------------+----------------+--------------------------+----+--------------------+
```

| Field           | Type      | Constraints                                                                                          |
| --------------- | --------- | ---------------------------------------------------------------------------------------------------- |
| `Version`       | `u8`      | *MUST* be `1`.                                                                                       |
| `Header`        | `u8`      | `MessageType` *MUST* be `0` (`Request`).                                                             |
| `RequestID`     | `u32` BE  | Client-chosen correlation id. See §4.4.                                                              |
| `RequestMethod` | `u16` BE  | Method code (see §5.3).                                                                              |
| `RequestPath`   | `bytes`   | UTF-8, *MUST NOT* contain `0x00`, *MUST NOT* be empty, *MUST NOT* start with `.` or `/`.             |
| `0x00`          | `u8`      | Single NUL terminator marking end of `RequestPath`.                                                  |
| `Body`          | `bytes`   | Possibly empty. If `CompressionType ≠ 0`, this is the codec-compressed body. If `IsJSON = 1`, the decompressed body is a valid JSON value. The server forwards it as the HTTP request body. |

**Path semantics.** `RequestPath` is the WebDriver command path
*relative to the session prefix*. The reference client computes:

```ts
const command = decodeURIComponent(
  url.pathname.slice(url.pathname.indexOf(sessionPrefix) + sessionPrefix.length)
);
```

where `sessionPrefix = "/session/<sessionId>/"`. The reference server
then reconstructs the upstream URL by concatenating its
already-resolved per-session base URL with `"/"` and the received path:

```go
url := sessionUrl + "/" + m.RequestPath
```

Hence a WebDriver call to `POST /session/abc/element` is transmitted as
a WsDriver Command with `RequestPath = "element"` and `RequestMethod
= 2` (POST). The session identifier itself is **not** transmitted on the
wire per command; it is implicit in the WebSocket connection (the server
resolves it from the URL path of the upgrade request,
`/wsdriver/<sessionId>/`).

The client always sends the command path *relative* to the session prefix
(no leading `/`), so the "must not start with `.` or `/`" rule simply
reflects that contract and keeps the `sessionUrl + "/" + RequestPath`
concatenation well-formed. Interpretation of any other path contents is
left to the implementation.

### 5.2. Response Message (Server → Client)

```
+--------+--------+----------------+-----------------+----+--------------------+
| Ver=1  | Header | RequestID (BE) | ResponseStatus  |0x00| Body (opt., compr.)|
| 1 byte | 1 byte | 4 bytes        | 2 bytes (BE)    |    | variable           |
+--------+--------+----------------+-----------------+----+--------------------+
```

| Field            | Type      | Constraints                                                                                              |
| ---------------- | --------- | -------------------------------------------------------------------------------------------------------- |
| `Version`        | `u8`      | *MUST* be `1`.                                                                                           |
| `Header`         | `u8`      | `MessageType` *MUST* be `1` (`Response`).                                                                |
| `RequestID`      | `u32` BE  | Echo of the originating Command’s `RequestID`.                                                           |
| `ResponseStatus` | `u16` BE  | HTTP status code from the upstream WebDriver, or a server-synthesized status for protocol errors.        |
| `0x00`           | `u8`      | The "path" slot is reserved on responses; the server *SHOULD* emit a single `0x00` (empty path).           |
| `Body`           | `bytes`   | The HTTP response body. Compression and JSON flags follow §4.3 and §4.5.                                 |

The empty-path slot is intentionally retained on responses so that the
two message shapes share the same 8-byte fixed header (9 bytes including
the path NUL). A future revision *MAY* use it.

### 5.3. Method Code Table

`RequestMethod` is a `u16` big-endian value. The full set is fixed and
ordered to mirror RFC 7231 §4 (with `PATCH` from RFC 5789 appended):

| Code | HTTP Method |
| ---- | ----------- |
| 0    | `GET`       |
| 1    | `HEAD`      |
| 2    | `POST`      |
| 3    | `PUT`       |
| 4    | `DELETE`    |
| 5    | `CONNECT`   |
| 6    | `OPTIONS`   |
| 7    | `TRACE`     |
| 8    | `PATCH`     |

Codes `9..65535` are reserved. The server *MUST* reject a Command whose
`RequestMethod` is `> 8` with the framing error `invalid request method`
(close code `1007`).

### 5.4. WebDriver-to-WsDriver Mapping

The mapping from a W3C WebDriver Classic HTTP call to a WsDriver Command
Message is mechanical:

| WebDriver HTTP call (logical)                                             | WsDriver Command                                                                        |
| ------------------------------------------------------------------------- | --------------------------------------------------------------------------------------- |
| `GET /session/{id}/url`                                                   | `Method=GET(0)`, `Path="url"`, `IsJSON=0`, `Body=<empty>`                               |
| `POST /session/{id}/url` body `{"url":"…"}`                               | `Method=POST(2)`, `Path="url"`, `IsJSON=1`, `Body=JSON bytes` (compressed if > 1024 B)  |
| `POST /session/{id}/element` body `{"using":"css selector","value":"#x"}` | `Method=POST(2)`, `Path="element"`, `IsJSON=1`, `Body=JSON bytes`                       |
| `POST /session/{id}/element/{eid}/click` body `{}`                        | `Method=POST(2)`, `Path="element/{eid}/click"`, `IsJSON=1`, `Body=\"{}\"`               |
| `DELETE /session/{id}/cookie/{name}`                                      | `Method=DELETE(4)`, `Path="cookie/{name}"`, `IsJSON=0`, `Body=<empty>`                  |
| `DELETE /session/{id}`                                                    | **Not sent over WsDriver** — `Path` would be empty, which §5.1 forbids. See note below. |

> **Session-deletion note.** `DELETE /session/{id}` is **not** transmitted
> over WsDriver: its path relative to the session prefix is empty, which
> the framing rules in §5.1 forbid. To end a session, the client sends the
> standard W3C WebDriver `DELETE /session/{id}` request over plain HTTP,
> right before or after closing the WebSocket connection on its side
> (either ordering is tolerated, §3.5).

On the server side, the Command is materialized into an upstream HTTP
request:

```go
url := sessionUrl + "/" + m.RequestPath
req, _ := http.NewRequest(m.RequestMethod.String(), url, body)
if m.Header.IsJSON {
    req.Header.Set("Content-Type", "application/json; charset=utf-8")
}
httpClient.Do(req)
```

The server *MUST* set `Content-Type: application/json; charset=utf-8` on
the upstream request iff the Command carried `IsJSON = 1`. No other
HTTP headers are propagated; `Host` and `Connection` are set
automatically by the Go HTTP client, and the upstream WebDriver is
assumed not to require authentication beyond what is configured
server-side.

Symmetrically, the Response sets `IsJSON = 1` iff the upstream
WebDriver’s response carries `Content-Type` starting with
`application/json`:

```go
if strings.HasPrefix(httpResponse.Header.Get("Content-Type"), "application/json") {
    header |= 1 << 1
}
```

### 5.5. Status Code Semantics

`ResponseStatus` is the *exact* HTTP status code from the upstream
WebDriver. The reference server passes through `200`, `4xx`, `5xx`
unchanged. Two status codes are *synthesized* by the WsDriver server
itself; both are framed as a Response Message with `IsJSON = 1`,
`CompressionType = 0`, and a W3C-shaped JSON error body:

| Frame `IsWsdriverError` | `ResponseStatus` | JSON `value.error`          | JSON `value.message`                                 | Condition                                                     |
| ----------------------- | ---------------- | --------------------------- | ---------------------------------------------------- | ------------------------------------------------------------- |
| `0`                     | `404`            | `"invalid session id"`      | `"session timed out or not found"`                   | Command arrived for an unknown or reaped session.             |
| `1`                     | `500`            | `"wsdriver protocol error"` | `"couldn't send request to webdriver. Cause: ..."`   | Upstream HTTP request failed (network, timeout, malformed).   |
| `1`                     | `500`            | `"wsdriver protocol error"` | `"couldn't construct wsdriver response. Cause: ..."` | Server could not re-encode upstream response into wsdriver.   |

The W3C-shaped body always takes the form:

```json
{ "value": { "error": "<W3C error name or wsdriver tag>", "message": "<human readable>" } }
```

so that conformant WebDriver clients can parse it through their
ordinary error path. Note that the *session-timeout* frame is **not**
flagged as a WsDriver protocol error (`IsWsdriverError = 0`) because it
is a legitimate application-level outcome that maps cleanly onto the
W3C "invalid session id" error.

### 5.6. Connection-Level Close Codes

These map *transport faults*, not application errors:

| WebSocket close code | Symbolic                   | Emitted by | Condition                                                  |
| -------------------- | -------------------------- | ---------- | ---------------------------------------------------------- |
| `1000`               | Normal Closure             | client     | Client closed the WebSocket gracefully.                    |
| `1001`               | Going Away                 | client     | Client is going away (e.g. the test runner is exiting).    |
| `1003`               | Unsupported Data           | server     | Server received a `TextMessage`. See §3.5.                 |
| `1007`               | Invalid Frame Payload Data | server     | Server failed to parse a binary frame (any §4 validation). |

The reference server itself *only ever emits* close codes `1003` and
`1007`. It never sends `1000` or `1001`: for every other termination
(client disconnect, broken connection, session end) the read loop simply
breaks and `gorilla/websocket`'s `Conn.Close()` closes the underlying TCP
connection *without* sending an explicit close frame. Codes `1000` and
`1001` are therefore only ever *received* from the client; the server
treats both as expected, non-error closes and does not log them as
unexpected (`websocket.IsUnexpectedCloseError(err, CloseNormalClosure,
CloseGoingAway)`).

In all `1007` cases the server *SHOULD* include a human-readable reason
string of the form `"Invalid message format: <details>"` in the close
frame.

---

## 6. Error Model

### 6.1. Framing Errors (transport-level)

Framing errors are unrecoverable on the current connection. The server
*MUST* send a WebSocket close frame and then close the TCP connection.
The client *MUST* treat the close as a signal to either (a) reconnect
and retry idempotent commands or (b) propagate a fatal error to the
caller.

The reference server emits framing errors for:

| Trigger                                       | Close code | Reason text prefix                                                             |
| --------------------------------------------- | ---------- | ------------------------------------------------------------------------------ |
| Non-binary WebSocket message                  | `1003`     | `WsDriver protocol only accepts binary data`                                   |
| Frame length < 9                              | `1007`     | `Invalid message format: message too short`                                    |
| `Version` ≠ 1                                 | `1007`     | `Invalid message format: invalid message version`                              |
| `MessageType` ≠ `Request` on Server inbound   | `1007`     | `Invalid message format: invalid message type`                                 |
| `CompressionType` > 2                         | `1007`     | `Invalid message format: unsupported compression type`                         |
| `RequestMethod` > 8                           | `1007`     | `Invalid message format: invalid request method`                               |
| Missing NUL terminator for `RequestPath`      | `1007`     | `Invalid message format: no null terminator found in url-path`                 |
| Empty `RequestPath`                           | `1007`     | `Invalid message format: unexpected empty request path`                        |
| `RequestPath` starts with `.` or `/`          | `1007`     | `Invalid message format: invalid request path. It can't start with '.' or '/'` |

### 6.2. Application Errors (per-request)

Application errors are *per-request*; the server *MUST NOT* close the
WebSocket. Instead, the server *MUST* send a Response Message (see §5.5)
with the same `RequestID` as the failing Command and let subsequent
Commands proceed.

#### 6.2.1. Session-Timeout Error

When session with given id is not currently active,
the server constructs the response with:

```
Version=1, Header=(Response<<4)|(0<<2)|(1<<1)|0,
RequestID=<echo>, ResponseStatus=404,
NUL, body = {"value":{"error":"invalid session id","message":"session timed out or not found"}}
```

Matching WebDriver response.

#### 6.2.2. Upstream HTTP Failure

If the upstream `httpClient.Do(req)` returns an error (e.g. dialer error,
read timeout, response parse failure), the server constructs the
response with:

```
Version=1, Header=(Response<<4)|(0<<2)|(1<<1)|1,    # IsWsdriverError = 1
RequestID=<echo>, ResponseStatus=500,
NUL, body = {"value":{"error":"wsdriver protocol error","message":"couldn't send request to webdriver. Cause: <err>"}}
```

The reference upstream HTTP client uses a 3-minute total request timeout
(`httpClient.Timeout = 3 * time.Minute`) and a connection pool of
`MaxIdleConns = 20`, `MaxIdleConnsPerHost = 5`.

#### 6.2.3. Response Encoding Failure

If the server reads the upstream response body but cannot construct the
WsDriver Response Message (e.g. compression failure), it emits the same
shape as §6.2.2 but with message
`"couldn't construct wsdriver response. Cause: <err>"`.

### 6.3. Client-side Classification

The reference client maps each error category to a typed exception. These
are the **application-layer** (`WSDriver*`) errors raised by
`WSDriverRequestAgent`; they wrap the **transport-layer** (`WsConnection*`)
errors described in §3.4. The two hierarchies are distinct: `WsConnection`
models the raw WebSocket transport, while `WSDriver` models WsDriver
request/response semantics on top of it.

| Error class                                | Retryable   | Trigger                                                       |
| ------------------------------------------ | ----------- | ------------------------------------------------------------- |
| `WSDriverError`                            | no          | Missing capabilities, fatal mis-configuration                 |
| `WSDriverRequestAgentEstablishmentError`   | conditional | Upgrade failed                                                |
| `WSDriverRequestAgentBreakError`           | yes         | Send failed on existing socket                                |
| `WSDriverRequestAgentTerminatedError`      | no          | Connection was manually closed                                |
| `WSDriverRequestAgentTimeoutError`         | yes         | Upgrade did not complete in `WSD_CONNECTION_TIMEOUT`          |
| `WSDriverRequestError`                     | yes         | Per-request protocol error from server (`IsWsdriverError=1`)  |
| `WSDriverRequestTimeoutError`              | yes         | Response did not come in expected amount of time              |

WSDriverRequestAgentEstablishmentError is retryable iff the upgrade response status code is **not** in `[400, 500)`.

Custom WebSocket error codes used by the reference client (purely
informational, surfaced inside `WsError.code`; they do **not** appear on
the wire):

```
MALFORMED_RESPONSE       = -32810
SEND_FAILED              = -32820
TIMEOUT                  = -32830
CONNECTION_TERMINATED    = -32840
CONNECTION_ESTABLISHMENT = -32850
CONNECTION_BREAK         = -32860
PROTOCOL_ERROR           = -32680
```

### 6.4. Client Retry Policy

The reference client retries each *request* up to
`WSD_REQUEST_RETRIES = 3` times with exponential backoff (base
`WSD_REQUEST_RETRY_BASE_DELAY = 500 ms`, factor `2`, jitter `100 ms`).
A retry is attempted iff the error is itself retryable per the table in
§6.3 — i.e. the underlying request was an idempotent or
connection-level fault rather than a hard protocol error.

`ETIMEDOUT` is propagated verbatim so that the upstream WebDriver client
(e.g. `webdriverio`) can apply its own retry policy on top.

---

## 7. Reference Implementation Analysis

### 7.1. Server-Side (Selenoid, Go)

The wsdriver server-side lives in `wsdriver/`:

```
wsdriver/
├── wsdriver.go         // top-level HandleConnection: upgrade + read loop
├── ws_req.go           // ParseRequestV1 (binary → RequestMessage struct)
├── ws_res.go           // WriteResponse (HTTP response → binary frame)
├── http_req.go         // MakeRequest (RequestMessage → upstream HTTP call)
└── wsdriver_errors.go  // WriteSessionTimedOutError / WriteHttpRequestError / WriteConstructResponseError
```

#### 7.1.1. Connection Upgrade

```go
var upgrader = websocket.Upgrader{
    CheckOrigin: func(r *http.Request) bool { return true },
}
var wsdriverHandshakeHeaders = http.Header{
    "WsDriver-Accept-Encoding": {"zstd, gzip"},
}

ws, err := upgrader.Upgrade(w, r, wsdriverHandshakeHeaders)
```

The server uses `gorilla/websocket`. `CheckOrigin` is unconditionally
permissive on the rationale that the hub sits behind the test
orchestration network and rejecting `Origin` would break the
`ws://hub.local/wsdriver/<id>/` upgrades that browsers do not issue
anyway. The fixed `wsdriverHandshakeHeaders` map is what advertises the
server-side codec list in §3.3.

The per-connection client-supported codec set is extracted from the
inbound `WsDriver-Accept-Encoding` header via a substring check:

```go
var clientSupportedEncoding = SupportedEncoding{
    IsGzipSupported: strings.Contains(r.Header.Get("WsDriver-Accept-Encoding"), "gzip"),
    IsZstdSupported: strings.Contains(r.Header.Get("WsDriver-Accept-Encoding"), "zstd"),
}
```

#### 7.1.2. Concurrency Model

Each WebSocket connection is handled by a *single goroutine* — the one
that called `HandleConnection`. Within that goroutine, the read loop is
strictly serial: read frame, parse, do upstream HTTP, write frame,
repeat. The protocol’s `RequestID` correlation field is present for
client benefit; the reference server does not exploit it to issue
upstream requests in parallel within a single connection.

The session map (`sessions.Get(sid)`, `sessions.Ensure(sid, sess)`) is
the only piece of cross-goroutine shared state and is synchronized by
Selenoid’s session manager outside the wsdriver package.

The session-liveness check is wired in by `selenoid.go::wsdriverRoute`,
which builds the `isSessionAliveFn` closure passed to
`HandleConnection`. That closure not only reports liveness but also
*re-arms the session’s idle-timeout timer* on every successful
WsDriver command:

```go
isSessionAliveFn := func() bool {
    if !sessions.Ensure(sid, sess) { return false }
    select { case <-sess.TimeoutCh: default: close(sess.TimeoutCh) }
    requestId := serial()
    sess.TimeoutCh = onTimeout(sess.Timeout, func() {
        request{r}.session(sid).Delete(requestId)
    })
    return true
}
```

This means a chatty WsDriver session resets its idle timer on every
binary command, identical to how Selenoid resets the timer on every
HTTP command.

#### 7.1.3. Memory Pooling

To avoid per-frame allocations, the server uses three optimizations:

1. **Per-connection buffers (stack-scoped).**

   ```go
   var wsBuffer = new(bytes.Buffer)
   var httpBuffer = new(bytes.Buffer)
   wsBuffer.Grow(256 * 1024)
   httpBuffer.Grow(256 * 1024)
   ```

   Two `bytes.Buffer`s are pre-grown to 256 KiB and reused for every
   message on this connection: `wsBuffer` accumulates the inbound WS
   frame and is later overwritten with the outbound WS frame;
   `httpBuffer` holds either the decompressed inbound body (handed to
   `http.NewRequest`) or the upstream HTTP response body (consumed
   when constructing the outbound WS frame).

2. **`RequestMessage` reuse.** A single `RequestMessage` struct is
   allocated per connection and re-populated by every call to
   `ParseRequestV1(data, &reqMsg)`. The `RequestPath` field is a
   `string` value but its byte storage comes from a `string(data[...])`
   conversion, which does allocate; the larger `Buffer` field is a
   simple slice header into `wsBuffer.Bytes()`, zero-copy.

3. **`sync.Pool` for codec workers.** Both directions use
   `sync.Pool`-backed gzip/zstd workers to avoid the relatively
   expensive table allocations of each codec:

   ```go
   var gzipWriterPool = sync.Pool{ New: func() any { w, _ := gzip.NewWriterLevel(nil, 4); return w } }
   var zstdWriterPool = sync.Pool{ New: func() any { w, _ := zstd.NewWriter(nil); return w } }
   var gzipReaderPool = sync.Pool{ New: func() any { return new(gzip.Reader) } }
   var zstdReaderPool = sync.Pool{ New: func() any { r, _ := zstd.NewReader(nil); return r } }
   ```

   Workers are `Reset(dst/src)` rather than newed per frame; the
   `klauspost/compress` package was chosen because it supports
   pool-friendly `Reset` on both readers and writers.

#### 7.1.4. Frame Parsing (`ParseRequestV1`)

The parser is a strict, single-pass, zero-allocation* binary reader
(`*` modulo the `string()` conversion of the path):

```go
req.Version = data[0]
h := data[1]
req.Header.MessageType     = MessageType((h >> 4) & 0x0f)
req.Header.CompressionType = CompressionType((h >> 2) & 0x03)
req.Header.IsJSON          = (h >> 1) & 0x01 != 0
req.Header.IsWsdriverError =  h       & 0x01 != 0
req.RequestID     = binary.BigEndian.Uint32(data[2:6])
req.RequestMethod = RequestMethod(binary.BigEndian.Uint16(data[6:8]))
requestPathLength := bytes.IndexByte(data[8:], 0)  // O(N) scan for NUL
req.RequestPath = string(data[8 : 8+requestPathLength])
req.Buffer = data[9+requestPathLength:]            // slice view, no copy
```

Validation order is fixed (length → version → message-type → compression
→ method → path); the parser returns at the first violation with a
descriptive error that becomes the WebSocket close-frame reason.

#### 7.1.5. Response Construction (`WriteResponse`)

The response writer (a) drains the upstream HTTP body into `httpBuffer`
*then* closes it (to free the connection back to the pool early), (b)
chooses a codec, (c) writes the 8-byte fixed header plus the empty-path
NUL (9 bytes total), then (d) streams the body — either directly via
`wsBuffer.Write(httpBuffer.Bytes())` or through a pooled `gzip.Writer` /
`zstd.Encoder` writing into `wsBuffer`.

The encoder’s `Reset(wsBuffer)` call is the critical hook that prevents
the codec from allocating its own destination buffer; the compressed
bytes land directly after those 9 header bytes in the same
`wsBuffer.Bytes()` slice that is handed to `ws.WriteMessage`.

#### 7.1.6. Error Construction

Error frames are emitted by `writeErrorMessage` in `wsdriver_errors.go`.
Note the deliberate `data.Reset()` at the top — the helper is permitted
to receive a buffer carrying stale bytes (typically the failed inbound
frame) because the caller’s contract is to recycle `wsBuffer`. The JSON
body is marshalled with the standard library:

```go
errorObject := map[string]interface{}{
    "value": map[string]string{ "error": errorName, "message": errorMessage },
}
payloadBody, _ := json.Marshal(errorObject)
data.Write(payloadBody)
```

The `panic(err)` on JSON marshal failure is intentional: the input is a
known, fully-static `map[string]string`, so a marshal error indicates a
fatal runtime corruption.

### 7.2. Client-Side (Testplane, TypeScript)

The WsDriver client-side lives in two cooperating modules:

```
testplane/src/ws-connection/       // Generic WS connection wrapper
├── index.ts                       // WsConnection<Response, Request> class
├── constants.ts                   // ping/timeout constants, error codes
├── error.ts                       // WsError hierarchy
└── utils.ts                       // exponentiallyWait

testplane/src/browser/wsdriver/    // WsDriver-specific layer
├── index.ts                       // WSDriverRequestAgent (high-level API)
├── request.ts                     // constructWsDriverRequest (binary encoder)
├── response.ts                    // parseWsDriverIncomingMessage (binary decoder)
├── compression.ts                 // getCompressed / getDecompressed
├── types.ts                       // Enums mirroring server-side codes
├── constants.ts                   // WsDriver tunables (threshold, retries)
├── error.ts                       // WSDriver* error subclasses
└── debug.ts                       // `debug` module integration
```

#### 7.2.1. Layered Architecture

`WsConnection<ResponseMessageType, RequestMessageType>` is a
*protocol-agnostic* WebSocket client that handles:

* Connection establishment with retries and exponential backoff;
* `unexpected-response` discrimination by HTTP status code (the 4xx/5xx
  retryability rule of §3.4);
* Promise-based request/response with timeout, keyed by an
  externally-chosen `requestId`;
* PING/PONG liveness with auto-reconnect (§3.6);
* Pending-request abort propagation on disconnect/close.

`WSDriverRequestAgent` is the WsDriver-specific façade that:

* Owns one `WsConnection<IncomingWsDriverMessage, RawData>`;
* Computes the `wsdriver-accept-encoding` upgrade header from runtime
  Node.js capability detection (`"zstd" in process.versions`);
* Lazily resolves the server-negotiated codec on first request via
  `_getRequestCompressionType()`;
* Encodes each outbound `RequestWsDriverOptions` into a `Buffer` via
  `constructWsDriverRequest`;
* Decodes each inbound `RawData` via `parseWsDriverIncomingMessage`;
* Translates `IsWsdriverError = 1` and malformed bodies into typed
  `WSDriverRequestError` rejections of the pending request promise.

#### 7.2.2. Event-Loop Integration

`WsConnection` is built directly on the `ws` package’s `EventEmitter`
interface. The hot path for a request is, in order:

1. `await this._getWsConnection()` — resolves immediately if the
   connection is `OPEN`, otherwise awaits the in-flight upgrade
   promise (concurrent callers share one `_wsConnectionPromise`).
2. Allocate a fresh `Promise<ResponseMessageType | WsError>`.
3. Register the resolver in `this._pendingRequests[requestId] = done`.
4. Arm a `setTimeout(done, this._timeouts.request)` (with `.unref()`
   so a pending request never holds the Node.js event loop open
   past `process.exit`).
5. Call `ws.send(requestMessage, sendErrorCallback)`.
6. On the WebSocket `"message"` event, the `onMessage` callback
   forwarded into the constructor is invoked; the WsDriver layer
   parses the frame, extracts `requestId`, and calls
   `wsConnection.provideResponseFor(requestId, parsed)`, which looks
   up and invokes `_pendingRequests[requestId]`.

The deliberate use of `.unref()` on the timer and on the ping
interval means the WsDriver client never extends Node.js process
lifetime; the only thing that keeps the event loop spinning is the
underlying socket descriptor.

#### 7.2.3. Promise Resolution Mapping

```ts
private _requestId = 0;
private _pendingRequests: Record<number, (response: T | WsError) => void> = {};

getRequestId(): number {
  const id = ++this._requestId;
  if (this._requestId >= WS_MAX_REQUEST_ID) this._requestId = 0;
  return id;
}
```

The pending-request map is the *sole* state used to demultiplex
responses. There is no per-request `Promise.race`; the timeout and the
"socket dropped" abort both call into the same `done(...)` resolver
registered under the request’s id, which is the function `delete`d
from the map atomically before being invoked.

When the connection is forcibly closed (`forceReconnect`,
`close`, ping-failure, send-failure, or remote close), the agent
synchronously snapshots `pendingRequestIds`, clears the map, and
rejects each pending request with either `ConnectionTerminated`
(manual close) or `ConnectionBreak` (involuntary), preserving the
original `requestId` in the error for logging.

#### 7.2.4. Chunk & Binary Streaming Handling

The `ws` package delivers a binary message either as a `Buffer`, an
`ArrayBuffer`, or a `Buffer[]` (the fragment array form, used when
`ws` cannot easily concatenate). `parseWsDriverIncomingMessage`
normalizes all three:

```ts
const rawDataToBuffer = (rawData: RawData): Buffer => {
  if (rawData instanceof Buffer) return rawData;
  if (Array.isArray(rawData))    return Buffer.concat(rawData as Uint8Array[]);
  return Buffer.from(rawData as Uint8Array);
};
```

The WsDriver client does not implement *streaming* decompression —
each binary message is treated as a complete, atomic WsDriver frame
and decompressed in one shot via the asynchronous `zlib.gunzip` /
`zlib.zstdDecompress` callbacks. This is consistent with the
single-binary-message-per-wsdriver-frame contract of §4.1.

If `parseWsDriverIncomingMessage` returns an `Error` (framing
malformed), the agent calls `wsConnection.forceReconnect(...)`. If it
returns `null` (valid framing but unsupported `Version`), the agent
silently discards the frame and logs a warning — this is the forward
compatibility hook that lets a v1-only client coexist on a connection
where a future v2 server might emit unsolicited side-channel frames.

#### 7.2.5. Outbound Encoding (`constructWsDriverRequest`)

```ts
const resultMessage = Buffer.alloc(
  OUTGOING_MESSAGE_AUXILIARY_BYTES + Buffer.byteLength(command) + Buffer.byteLength(compressedPayload)
);
let ptr = 0;
ptr = resultMessage.writeUInt8(WSDRIVER_VERSION);
ptr = resultMessage.writeUint8(headerByte, ptr);
ptr = resultMessage.writeUint32BE(connectionOptions.requestId, ptr);
ptr = resultMessage.writeUint16BE(requestMethod, ptr);
ptr += resultMessage.write(command, ptr, "utf8");
ptr = resultMessage.writeUint8(0, ptr);
ptr += compressedPayload.copy(resultMessage, ptr);
if (ptr !== resultMessage.byteLength) {
  throw new Error("WSDriver request message construction failed");
}
```

Notable details:

* The exact byte budget is computed up-front
  (`OUTGOING_MESSAGE_AUXILIARY_BYTES = 1 + 1 + 4 + 2 + 1 = 9`,
  matching the `minMessageSize` constant on the server) and the buffer
  is allocated to that exact size — a single allocation per outbound
  request.
* The post-condition `ptr === resultMessage.byteLength` is a
  build-time invariant guard, not a defensive runtime check, ensuring
  that any future field-layout drift between client and server is
  caught immediately.
* `IsJSON` is hard-coded to `true` on the outbound side: the reference
  client sets `IsJSON = 1` on **every** Command regardless of method or
  body, because the W3C WebDriver protocol models command/response payloads
  as JSON. Consequently the `IsJSON = 0` rows in the §5.4 mapping table
  describe the *logical* W3C semantics, not what the reference client puts
  on the wire — in practice it always emits `IsJSON = 1` (an empty body
  with `IsJSON = 1` is harmless: the server only sets the upstream
  `Content-Type` header and forwards the empty body).
* Compression is performed *only* when the codec is non-`None` *and*
  the uncompressed payload is ≥ `WSD_COMPRESSION_THRESHOLD_BYTES`,
  matching the server-side threshold from §4.5.2. The negotiated
  `compressionType` is also overwritten to `None` in the header byte
  whenever the threshold was not met, so a peer can rely on
  `CompressionType` to faithfully describe how the body was actually
  encoded.

---

## Appendix A — Constants & Tunables

### A.1. Wire Constants (normative)

| Name                          | Value                           | Where used                                            |
| ----------------------------- | ------------------------------- | ----------------------------------------------------- |
| `ExpectedMessageVersion`      | `1`                             | First byte of every frame.                            |
| `minMessageSize`              | `9`                             | Minimum legal frame length (bytes).                   |
| `compressionThresholdBytes`   | `1024`                          | Body length below which a peer *SHOULD NOT* compress. |
| `WsDriver-Accept-Encoding`    | `"zstd, gzip"` (server default) | Upgrade header advertising codec list.                |

### A.2. Reference-Server Tunables (informative)

| Name                  | Value             | Effect                                               |
| --------------------- | ----------------- | ---------------------------------------------------- |
| `gzipCompressionLevel`| `4`               | Server-side gzip level (≈ 2× faster than default 6). |
| `httpClient.Timeout`  | `3 * time.Minute` | Upstream WebDriver request total timeout.            |
| `MaxIdleConns`        | `20`              | Upstream HTTP connection pool size.                  |
| `MaxIdleConnsPerHost` | `5`               | Per-host upstream pool size.                         |
| Per-connection buffer | `256 KiB`         | Initial `Grow` of `wsBuffer` and `httpBuffer`.       |

### A.3. Reference-Client Tunables (informative)

| Name                              | Value                      | Effect                                         |
| --------------------------------- | -------------------------- | ---------------------------------------------- |
| `WSD_CONNECTION_TIMEOUT`          | `15 000 ms`                | Per-attempt WS upgrade deadline.               |
| `WSD_CONNECTION_RETRIES`          | `3`                        | Upgrade retries.                               |
| `WSD_CONNECTION_RETRY_BASE_DELAY` | `500 ms`                   | Base backoff between upgrade retries.          |
| `WSD_REQUEST_RETRIES`             | `3`                        | Per-request retries.                           |
| `WSD_REQUEST_RETRY_BASE_DELAY`    | `500 ms`                   | Base backoff between request retries.          |
| `WSD_COMPRESSION_THRESHOLD_BYTES` | `1024`                     | Mirrors `compressionThresholdBytes`.           |
| `WS_MAX_REQUEST_ID`               | `2147483647` (`INT32_MAX`) | RequestID wrap boundary.                       |
| `WS_PING_INTERVAL`                | `15 000 ms`                | Application ping cadence.                      |
| `WS_PING_TIMEOUT`                 | `10 000 ms`                | Per-ping pong deadline.                        |
| `WS_PING_MAX_SUBSEQUENT_FAILS`    | `2`                        | Pongs missed before triggering reconnect.      |
| gzip level (client encode)        | `4`                        | Symmetric with server.                         |

---

## Appendix B — Canonical Wire Examples

All multi-byte integers are big-endian. Whitespace is purely visual.

### B.1. `GET /session/<id>/status`, no body, no compression

Command (15 bytes):

```
01                          // Version=1
02                          // Header: MsgType=Request(0), Comp=None(0), IsJSON=1, IsWdErr=0
00 00 00 01                 // RequestID=1
00 00                       // Method=GET(0)
73 74 61 74 75 73           // Path = "status"
00                          // NUL terminator
                            // (no body)
```

Successful Response (≈ 23 bytes):

```
01                          // Version=1
12                          // Header: MsgType=Response(1), Comp=None(0), IsJSON=1, IsWdErr=0
00 00 00 01                 // RequestID=1 (echo)
00 C8                       // Status=200
00                          // empty path
7B 22 76 61 6C 75 65 22 3A 6E 75 6C 6C 7D   // {"value":null}
```

### B.2. `POST /session/<id>/element` with `{"using":"css selector","value":"#x"}`, no compression

Command:

```
01                          // Version
02                          // Header: Request, None, IsJSON=1, IsWdErr=0
00 00 00 2A                 // RequestID=42
00 02                       // Method=POST(2)
65 6C 65 6D 65 6E 74        // Path = "element"
00                          // NUL
7B 22 75 73 69 6E 67 22 3A 22 63 73 73 20 73 65 6C 65 63 74 6F 72 22 2C 22 76 61 6C 75 65 22 3A 22 23 78 22 7D
```

### B.3. Same command, compressed with gzip

`Header` becomes `0x06` = `Request(0) | GZIP(1)<<2 | IsJSON(1)<<1`, and
the trailing JSON is replaced by the RFC 1952 gzip stream of those same
bytes. This payload is below the `1024`-byte threshold, so the reference
implementations would *not* compress it; but the threshold is only a
heuristic (§4.5.2), so a peer *MAY* compress a body of any size and the
decoder *MUST* honor the `CompressionType` field regardless of length.

### B.4. Server-synthesized session-timeout error

```
01                          // Version
12                          // Header: Response, None, IsJSON=1, IsWdErr=0
00 00 00 2A                 // RequestID=42 (echo)
01 94                       // Status=404
00                          // empty path
7B 22 76 61 6C 75 65 22 3A 7B 22 65 72 72 6F 72 22 3A 22 69 6E 76 61 6C 69 64 20 73 65 73 73 69 6F 6E 20 69 64 22 2C 22 6D 65 73 73 61 67 65 22 3A 22 73 65 73 73 69 6F 6E 20 74 69 6D 65 64 20 6F 75 74 20 6F 72 20 6E 6F 74 20 66 6F 75 6E 64 22 7D 7D
```

### B.5. Server-synthesized protocol error (upstream unreachable)

```
01                          // Version
13                          // Header: Response, None, IsJSON=1, IsWdErr=1   <-- low bit set
00 00 00 2A                 // RequestID=42
01 F4                       // Status=500
00                          // empty path
{"value":{"error":"wsdriver protocol error","message":"couldn't send request to webdriver. Cause: dial tcp 127.0.0.1:1: connect: connection refused"}}
```

---

*End of WsDriver Protocol Specification v1.0.0.*
