G3: Shared Memory Transport for gRPC
----
* Author(s): Mark Russinovich, Qiming Sun
* Approver: a11r
* Status: Draft
* Implemented in: n/a
* Last updated: 2026-05-25
* Discussion at: (TBD)

## Abstract

This proposal defines a protocol for gRPC over shared memory, for
inter-process calls on the same host. Two processes map the same memory
region and exchange gRPC frames through SPSC (single-producer /
single-consumer) ring buffers, using OS address-wait/wake primitives for
synchronization.

## Background

gRPC uses HTTP/2 over TCP. When both the client and server are on the same
host, the TCP path still traverses the full kernel networking stack — socket
buffers, TCP state machine, congestion control — which is unnecessary for
local IPC.
Even with HTTP/2 over a loopback or Unix domain socket, the transport still copies
bytes between user-space buffers and the kernel.

Shared memory lets two processes read and write the same physical pages
directly, allowing the receiver to parse frames in place without an intermediate copy.
This document specifies how HTTP/2 frames are carried over shared memory ring
buffers, together with the connection lifecycle.

### Related Proposals:

n/a

## Proposal

### Design Overview

Each SHM connection consists of a single memory-mapped segment containing two
unidirectional SPSC ring buffers:

- **Ring A**: Client → Server
- **Ring B**: Server → Client

The client writes to Ring A and reads from Ring B; the server does the
reverse. Multi-byte integers in the segment header, ring headers, and
control-segment frames are **little-endian**. HTTP/2 frames on the data
segment use the byte order defined by RFC 7540.

Frames carried on the data segment use HTTP/2 framing
([RFC 7540](https://httpwg.org/specs/rfc7540.html)) with HPACK
([RFC 7541](https://www.rfc-editor.org/rfc/rfc7541)), subject to the
constraints in [HTTP/2 Mapping](#http2-mapping). Stream multiplexing,
flow control, and keepalive follow HTTP/2 semantics. gRPC
[Length-Prefixed-Message](https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md)
encoding, status codes, metadata conventions, and compression negotiation
apply without modification.

The control segment uses a small SHM-specific framing, defined in
[Connection Establishment](#connection-establishment), for the one-time
CONNECT/ACCEPT exchange that brings up a data segment.

![Dual-Ring Architecture](G3_graphics/dual_ring_architecture.png)

## Shared Memory Segment Layout

### Segment Header (128 bytes)

The segment header resides at offset 0 of the mapped region.

| Offset | Size | Field | Description |
|--------|------|-------|-------------|
| 0x00 | 8B | Magic | Fixed value `"GRPCSHM\0"` (0x47 52 50 43 53 48 4D 00) |
| 0x08 | 4B | Version | Protocol version (current = 1) |
| 0x0C | 4B | Flags | Reserved; MUST be 0 |
| 0x10 | 8B | TotalSize | Total mapped region size in bytes |
| 0x18 | 8B | RingAOffset | Byte offset to Ring A header |
| 0x20 | 8B | RingACapacity | Ring A data area capacity; MUST be a power of 2 |
| 0x28 | 8B | RingBOffset | Byte offset to Ring B header |
| 0x30 | 8B | RingBCapacity | Ring B data area capacity; MUST be a power of 2 |
| 0x38 | 4B | ServerPID | Server process ID |
| 0x3C | 4B | ClientPID | Client process ID |
| 0x40 | 4B | ServerReady | Server ready flag (0 or 1) |
| 0x44 | 4B | ClientReady | Client ready flag (0 or 1) |
| 0x48 | 4B | Closed | Connection closed flag (0 or 1) |
| 0x4C | 4B | Pad | Alignment padding |
| 0x50 | 4B | MaxStreamsHint | Informational pre-SETTINGS hint for maximum concurrent streams. The authoritative limit is `SETTINGS_MAX_CONCURRENT_STREAMS` exchanged during HTTP/2 SETTINGS (per RFC 7540 §6.5.2); this field is provided so clients can size internal state before SETTINGS arrives. A value of 0 means "unspecified". Implementations MAY ignore this field and rely solely on SETTINGS. |
| 0x54 | 4B | OpenerWakeReady | Set to 1 by the opener (client) AFTER establishing any cross-process wake amplifier (e.g., a Linux eventfd received via SCM_RIGHTS) and BEFORE setting `ClientReady`. A value of 0 indicates the opener fell back to the address-wait/wake primitive only; the creator (server) MUST use the same fallback to avoid an asymmetric park/wake deadlock. See [Linux Cross-Process Wake Amplifier](#linux-cross-process-wake-amplifier). |
| 0x58–0x7F | 40B | Reserved | MUST be 0 |

Implementations MUST validate the segment header after mapping. The
following checks MUST be performed before any other field is read or
any ring is accessed:

- `Magic` matches the expected value and `Version` is recognized;
  otherwise the mapping MUST be discarded.
- `RingACapacity` and `RingBCapacity` are each powers of two and at
  least 4 KiB (see [Ring Sizing](#ring-sizing)).
- `RingAOffset + RingACapacity + ring-header overhead ≤ TotalSize` and
  the same for Ring B.
- The region `[RingAOffset, RingAOffset + ring-header overhead + RingACapacity)`
  does not overlap the corresponding region of Ring B, nor the segment
  header at offsets `0x00 .. 0x80`.
- `TotalSize` matches the size of the mapped file (or, on Windows, the
  size reported for the mapped section object).

Any failed check MUST cause the mapping to be discarded; the peer MAY
be notified through the control-plane reject path when validation
happens during establishment.

**Ready flag rules:**

- The server sets `ServerReady = 1` after the segment is fully initialized.
  The client MUST NOT read any other header field until `ServerReady == 1`.
- The client sets `ClientReady = 1` after it has mapped the data segment and
  is prepared to receive frames.
- Each of `ServerReady`, `ClientReady`, and `Closed` transitions only
  from 0 → 1 within its own lifetime; the reverse transition is
  never valid. The three flags are independent of one another.
- `Closed` in the **segment header** transitions only from 0 → 1 and
  indicates the connection is shutting down. Either side MAY set it. Once
  set, no new streams may be opened. Existing data already written to the
  rings SHOULD be consumed (drained) before unmapping, except when an
  abrupt error condition (such as receipt of GOAWAY with a non-zero error
  code or unrecoverable peer failure) makes drain impractical, in which
  case unprocessed ring data MAY be discarded.
- `Closed` in a **ring header** is set by that ring's producer (the side
  that writes to it) to indicate it will write no more data. The consumer
  MUST drain any remaining bytes before treating the ring as closed. The
  segment-level `Closed` flag is typically set after both rings are closed.
- All flag reads and writes MUST use acquire/release memory ordering.

### Ring Header (64 bytes)

Each ring buffer has a 64-byte header at the byte offset given by
`RingAOffset` or `RingBOffset` in the segment header.

| Offset | Size | Field | Description |
|--------|------|-------|-------------|
| 0x00 | 8B | Capacity | Data area capacity in bytes; power of 2 |
| 0x08 | 8B | WriteIdx | Monotonically increasing write index (producer) |
| 0x10 | 8B | ReadIdx | Monotonically increasing read index (consumer) |
| 0x18 | 4B | DataSeq | Data-available sequence (wait/wake target) |
| 0x1C | 4B | SpaceSeq | Space-available sequence |
| 0x20 | 4B | Closed | Ring closed flag; producer sets to 1 |
| 0x24 | 4B | Pad | Alignment padding |
| 0x28 | 4B | ContigSeq | Contiguity sequence (optional; see below) |
| 0x2C | 4B | SpaceWaiters | Writers blocked waiting for space |
| 0x30 | 4B | ContigWaiters | Writers blocked waiting for contiguity (optional) |
| 0x34 | 4B | DataWaiters | Readers blocked waiting for data |
| 0x38–0x3F | 8B | Reserved | MUST be 0 |

### Ring Data Area

Immediately following the ring header is a contiguous region of `Capacity`
bytes used as a circular buffer:

- Write position: `WriteIdx & (Capacity - 1)`
- Read position: `ReadIdx & (Capacity - 1)`
- Readable bytes: `WriteIdx - ReadIdx`
- Writable bytes: `Capacity - (WriteIdx - ReadIdx)`

### Overall Memory Layout

```
Offset 0:          Segment Header (128B)
Offset 128:        Ring A Header  (64B)     [Client → Server]
Offset 192:        Ring A Data    (N bytes, power-of-2)
Offset 192+N:      Ring B Header  (64B)     [Server → Client]
Offset 256+N:      Ring B Data    (M bytes, power-of-2)
```

![Segment Memory Layout](G3_graphics/segment_memory_layout.png)

## Transport Discovery

Before using shared memory, the client and server negotiate over an
existing HTTP/2 connection whether to switch to SHM. Discovery uses gRPC
metadata on any RPC:

1. The client sends initial metadata key `shm-offer` with an empty value.
2. If the server supports shared memory and the connection originates from
   the same host, the server includes trailing metadata key `shm-ctl` whose
   value is the control segment name.
3. The client opens the control segment and proceeds with the
   [Establishment Sequence](#establishment-sequence).

If the server does not return `shm-ctl`, or if the client fails to open
the control segment, the client continues using HTTP/2.

The bootstrap connection over which `shm-offer` is exchanged MAY be
TCP, TCP+TLS, or a Unix domain socket. The negotiation runs as standard
gRPC metadata on whatever channel is already open between the peers;
no new URI scheme is introduced for SHM. Either side MAY be deployed
without SHM support, in which case the missing metadata key causes the
RPC to complete over the bootstrap channel exactly as if SHM were not
involved.

The negotiation is backward compatible. A server that does not
implement this protocol ignores the unknown `shm-offer` key (per standard
gRPC metadata handling) and never returns `shm-ctl`; the client stays on
HTTP/2. A client that does not implement this protocol never sends
`shm-offer`; the server does not push SHM. Either side may be upgraded
independently without breaking existing deployments.

`shm-offer` and `shm-ctl` are reserved metadata keys. Applications and
interceptors MUST NOT modify them. The discovered control segment name
applies to the lifetime of the HTTP/2 connection; the client SHOULD NOT
repeat discovery on the same connection. Discovery affects subsequent RPCs
on that connection, not the RPC carrying `shm-offer` itself.

### Control Segment Naming

The control segment name returned in `shm-ctl` SHOULD contain a
cryptographically random component to prevent name-guessing attacks.
Recommended format:

```
<server-id>_<uuid>_ctl
```

The control segment is created once by the server, typically at
listener startup (e.g. when the server invokes the SHM listener API
with a chosen name). The server does not create a new control segment
per RPC or per offer; it returns the name of the already-existing
control segment in the trailing metadata. The same control segment
name applies to all clients connecting to that server.

### Same-Host Detection

Servers MUST verify same-host before returning `shm-ctl`. Sufficient
evidence:

- The peer address is an IPv4/IPv6 loopback address (per the standard
  `IsLoopback` semantics of the platform's network library).
- The peer address has network type `unix` or `unixpacket`.

Servers MUST NOT return `shm-ctl` to peers that present a non-loopback
IP address or whose peer information is unavailable. Implementations
behind a reverse proxy that rewrites the peer address MUST disable
`shm-ctl` advertisement entirely or explicitly opt out, since a remote
peer can otherwise be misidentified as local.

### Client Verify-Ready Timing

After receiving `shm-ctl`, the client opens the announced control
segment and proceeds with the [Establishment Sequence](#establishment-sequence).
The client MUST NOT discard the bootstrap channel until the SHM
transport reaches its connected state (HTTP/2 SETTINGS exchange
complete, or the implementation's equivalent of the gRPC connectivity
`Ready` state).

A bounded timeout SHOULD be applied. Reference implementations use 2
seconds. On timeout the client MUST close the SHM attempt and continue
on the bootstrap channel; a successful discovery is otherwise permanent
for the lifetime of the bootstrap connection.

## Connection Establishment

### Control Segment

Connection establishment uses a shared control segment. The control
segment name is provided by the server during
[Transport Discovery](#transport-discovery) or through an out-of-band
mechanism. The control segment uses the same binary layout as a data
segment; fields that are connection-specific (ClientPID, ClientReady)
apply to the current exchange only and are reset between connections.

The control segment is shared among all clients connecting to the same
server. Because Ring A of the control segment may receive CONNECT frames
from multiple client processes, the SPSC assumption does not hold on this
ring. Implementations MUST serialize writes to Ring A using an OS-level
mutual exclusion primitive tied to the control segment name:

- **Linux / POSIX**: advisory lock (`flock`) on the control segment's
  backing file.
- **Windows**: a named mutex whose name is the control segment name with
  a `.lock` suffix (e.g. if the control segment is `grpc_ctl`, the mutex
  is `grpc_ctl.lock`).

The client acquires the lock before writing CONNECT and releases it after
reading ACCEPT or REJECT. While the lock is held, the holding client is
the sole reader of Ring B; other clients MUST NOT read or advance Ring B
state. Both rings are therefore single-producer / single-consumer for
the duration of each exchange.

### Control Frame Envelope

Each control frame begins with a fixed 16-byte header followed by an
opaque payload:

| Offset | Size | Field | Description |
|--------|------|-------|-------------|
| 0x00 | 4B | Length | Payload length in bytes, not counting the 16-byte header |
| 0x04 | 4B | StreamID | MUST be 0 for all control frames |
| 0x08 | 1B | Type | Frame type (see below) |
| 0x09 | 1B | Flags | MUST be 0 |
| 0x0A | 6B | Reserved | MUST be 0 |

Multi-byte fields are little-endian (consistent with the rest of the
control segment). Defined Type values:

| Value | Name |
|-------|------|
| 0x10 | CONNECT |
| 0x11 | ACCEPT |
| 0x12 | REJECT |

The payload encodings for each Type are defined below.

The `Length` field is an unsigned 32-bit value, but receivers MUST cap
the body they are willing to allocate; 4 KiB is sufficient for all
frame types defined here (the largest legitimate body is an ACCEPT
carrying a UTF-8 segment name well below 1 KiB). On encountering an
oversized `Length`, the receiver:

- MUST NOT allocate the advertised buffer.
- SHOULD attempt a bounded best-effort drain on the ring so subsequent
  well-formed frames from other clients can still be parsed.
- MUST NOT tear down the listener immediately. Implementations SHOULD
  bound CPU spent on consecutive malformed frames (e.g., short backoff
  plus a maximum consecutive count) before considering the control ring
  unrecoverable.

### Control Frame Payloads

#### CONNECT Payload (variable, minimum 20 bytes)

```
Version(1B) | RingACapacity(8B LE) | RingBCapacity(8B LE) | Flags(1B)
            | WireFormatCount(1B) | WireFormats(count B)
```

- Version: control-frame encoding version (current = 2). This is
  independent of the segment header Version field, which describes the
  segment binary layout. v2 introduces the Flags byte on CONNECT and
  a reserved Flags byte on ACCEPT (see ACCEPT Payload). v1 peers that
  omit these bytes MUST be rejected at the handshake boundary; the
  protocol is pre-1.0 and does not preserve v1 wire compatibility.
- RingACapacity / RingBCapacity: client's preferred ring sizes in bytes.
  A value of 0 means "use the server's default." The server is free to
  choose smaller capacities.
- Flags: bitfield for connection options. Defined bits:

  | Bit | Name | Description |
  |-----|------|-------------|
  | 0 | SINGLE_STREAM | Client requests single-stream mode (see below) |
  | 1–7 | (reserved) | MUST be 0 on send; ignored on receive. |

  Senders MUST set reserved flag bits to 0. Receivers MUST ignore
  unknown flag bits for forward compatibility.

  `SINGLE_STREAM` is a hint that the client will not open more than one
  concurrent stream. It does not appear on the wire after CONNECT; the
  server signals acceptance by advertising
  `SETTINGS_MAX_CONCURRENT_STREAMS = 1` in its initial SETTINGS frame on
  the data segment. The hint allows the server to skip per-stream
  scheduling state. Setting the bit does not change frame syntax: the
  client MUST still emit valid HTTP/2 frames with non-zero StreamID for
  RPCs.

  The server MAY decline the hint; in that case it advertises its
  normal `SETTINGS_MAX_CONCURRENT_STREAMS` value (or omits the
  setting), and the client MUST accept the negotiated value and use
  the general multi-stream path.

- WireFormatCount: number of data-plane wire formats advertised by the
  client. The value MUST be at least 1.
- WireFormats: list of accepted data-plane wire-format codes, one byte
  each. Defined codes:

  | Code | Name | Notes |
  |------|------|-------|
  | 0x01 | HTTP/2 | The only data-plane wire format defined by this specification. |

  Senders MUST include 0x01 in the list. Receivers that do not find
  0x01 in the advertised set MUST respond with REJECT. The advertisement
  is mandatory in this revision; a peer that omits it (zero-length
  trailing bytes) MUST be treated as protocol-incompatible and rejected
  at the handshake boundary.

#### ACCEPT Payload (variable, minimum 7 bytes)

```
Version(1B) | NameLen(4B LE) | DataSegmentName(var, UTF-8)
            | SelectedWire(1B) | Flags(1B)
```

Contains the name of the data segment the server has allocated and the
wire-format the server selected from the client's CONNECT advertisement.
After receiving ACCEPT, the client maps the named segment. The negotiated
ring capacities are read from the data segment's header.

`SelectedWire` is the wire-format code the server chose from the
client's CONNECT advertisement. MUST be 0x01 (HTTP/2). Clients MUST
treat any other value as a connection failure and continue on the
bootstrap channel.

`Flags` is a v2 reserved byte. Senders MUST set it to 0; receivers
MUST accept any value for forward compatibility but MUST NOT
interpret bits without a normative definition in a later revision
of this gRFC.

#### REJECT Payload (variable)

```
Version(1B) | MsgLen(4B LE) | ErrorMessage(var, UTF-8)
```

The connection attempt has failed; ErrorMessage is diagnostic text.

The Version field in CONNECT, ACCEPT, and REJECT identifies the control-
frame encoding version. A receiver that does not recognize the Version
MUST respond with REJECT (when reading CONNECT) or treat the response as
a connection failure (when reading ACCEPT or REJECT) and continue using
HTTP/2.

#### Control-frame payload validation

Receivers MUST validate every control-frame payload before acting on
it:

- CONNECT: payload length ≥ 20 (`Version(1) + RingACapacity(8) +
  RingBCapacity(8) + Flags(1) + WireFormatCount(1) + WireFormats(≥1)`),
  `WireFormatCount ≥ 1`, the WireFormats list fits within the remaining
  `Length`, and the advertised list contains code 0x01 (HTTP/2).
- ACCEPT: `NameLen > 0`, `NameLen + 7 == Length` (Version + NameLen +
  Name + SelectedWire + Flags), `DataSegmentName` is valid UTF-8, and
  `SelectedWire` is one of the codes the client advertised in CONNECT.
- REJECT: `MsgLen + 5 == Length` and `ErrorMessage` is valid UTF-8.

A payload that fails any check MUST be treated as a connection
failure: the receiver of a malformed CONNECT responds with REJECT and
drops the offending client; the receiver of a malformed ACCEPT or
REJECT closes the SHM attempt and continues on the bootstrap channel.

### Establishment Sequence

![Connection Establishment](G3_graphics/connection_establishment.png)

1. Server creates the control segment and sets `ServerReady = 1`.
2. Client opens the control segment. It MUST wait until `ServerReady == 1`
   and then perform full segment-header validation (see
   [Segment Header](#segment-header-128-bytes)).
3. Client acquires the control-segment write lock and sends CONNECT on
   Ring A.
4. Server reads CONNECT, allocates a data segment, and responds with ACCEPT
   (or REJECT) on Ring B.
5. Client reads the response and releases the write lock.
6. Client maps the data segment and sets `ClientReady = 1` in the **data**
   segment header.
7. HTTP/2 frames begin flowing on Ring A and Ring B of the data segment,
   starting with each side's initial SETTINGS frame (see
   [Connection Preface](#connection-preface)).

A server MUST refuse to start a listener whose control segment name
already exists with `ServerReady == 1` in the existing segment,
returning a clear "address in use" error to the operator. Only when
the existing segment has `ServerReady == 0` (or cannot be opened at
all) MAY the implementation clean it up and proceed. This prevents a
misconfigured restart from silently unlinking the active inode while
existing clients hold a mapping to it.

### Security Handshake

The base protocol does not require a security handshake on the data
segment. Implementations MAY perform one, in which case it runs on
the data segment's Ring A and Ring B AFTER `ClientReady == 1` but
BEFORE the HTTP/2 SETTINGS exchange.

Frame types 0x20–0x2F are RESERVED for security-handshake frames.
Implementations that do not perform a handshake interoperate with
peers that also omit it; the first frame on the data segment with a
type outside this reserved range is treated as the start of HTTP/2
frame flow.

The wire format of handshake frames is left to a follow-up gRFC. See
[Open Issues](#open-issues-if-applicable).

## HTTP/2 Mapping

Frames carried on the data segment use HTTP/2 framing as defined in
[RFC 7540](https://httpwg.org/specs/rfc7540.html), with HPACK header
compression as defined in [RFC 7541](https://www.rfc-editor.org/rfc/rfc7541),
subject to the constraints in this section. The control segment uses the
SHM-specific framing defined in [Connection Establishment](#connection-establishment)
and is not subject to these rules.

### Framing on the Ring

HTTP/2 frame parsing on this transport is byte-stream oriented and
identical to HTTP/2 over any other transport. A frame's 9-byte header and
its payload MAY be produced and consumed across multiple ring write or
read operations. Receivers MUST process frame bytes incrementally;
senders MAY advance `WriteIdx` mid-frame so that the receiver can begin
draining payload while the rest is still being written.

Ring capacity is therefore independent of `MAX_FRAME_SIZE`. A ring
smaller than the peer's advertised `MAX_FRAME_SIZE` is well-formed; a
frame whose total size exceeds ring capacity is carried by alternating
writer-blocks-on-space and reader-blocks-on-data through the
[Wait/Wake](#waitwake) mechanism.

### Frame Set

| Frame | Used | Notes |
|-------|------|-------|
| DATA (0x0) | yes | Carries gRPC Length-Prefixed-Message bytes |
| HEADERS (0x1) | yes | Initial headers; trailers carry END_STREAM |
| PRIORITY (0x2) | ignored | Stream prioritization is not used; receivers MUST silently ignore |
| RST_STREAM (0x3) | yes | Stream cancellation |
| SETTINGS (0x4) | yes | Negotiation; ACK MUST be sent per RFC 7540 |
| PUSH_PROMISE (0x5) | not used | gRPC does not use server push |
| PING (0x6) | yes | Keepalive |
| GOAWAY (0x7) | yes | Connection shutdown |
| WINDOW_UPDATE (0x8) | yes | See [Flow Control](#flow-control) |
| CONTINUATION (0x9) | yes | Sent when a header block exceeds the peer's MAX_FRAME_SIZE |

Senders MUST NOT emit PRIORITY or PUSH_PROMISE frames. Receipt of a
PUSH_PROMISE frame is a connection error of type `PROTOCOL_ERROR`
(RFC 7540 §6.6).

### Connection Preface

The HTTP/2 connection preface (`PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n`) MUST NOT
be used. The Connection Establishment handshake on the control segment has
already established a peer relationship by the time the data segment is
mapped.

After [Establishment Sequence](#establishment-sequence) step 7, peers
MAY exchange HTTP/2 SETTINGS frames on the data-segment ring; receivers
MUST be able to parse SETTINGS and SETTINGS ACK frames, and MUST NOT
treat a SETTINGS frame as a protocol error. Because SHM peers run on
the same host with full out-of-band configuration access, this gRFC
does not require a SETTINGS preface: both endpoints MAY operate
entirely from locally-configured defaults (e.g., `INITIAL_WINDOW_SIZE`
from `grpc.WithInitialWindowSize` / `ServerConfig.InitialWindowSize`),
provided the two sides are symmetrically configured. If a peer
chooses to advertise SETTINGS, the other side MUST acknowledge per
RFC 7540 §6.5. Wire-format normative SETTINGS exchange (the
preface-and-ACK handshake required by RFC 7540 §3.5) is OPTIONAL
in this gRFC and MAY be required by a future revision.

### SETTINGS

The following parameters apply to this transport. The defaults below
apply to parameters not explicitly advertised:

| Parameter | Default | Purpose |
|-----------|---------|---------|
| HEADER_TABLE_SIZE (0x1) | 0 | Disable HPACK dynamic table; MUST be 0 (see [HPACK](#hpack)) |
| ENABLE_PUSH (0x2) | 0 | Server push is not used; MUST be 0 |
| MAX_CONCURRENT_STREAMS (0x3) | unlimited | Server-defined; clients MUST honor when advertised |
| INITIAL_WINDOW_SIZE (0x4) | 33,554,432 (32 MiB, SHM-tuned default) | See [Flow Control](#flow-control). Implementations SHOULD expose configuration knobs for the per-endpoint initial window size (e.g. gRPC-Go's `grpc.WithInitialWindowSize` / `grpc.InitialWindowSize`); both endpoints SHOULD be configured symmetrically. |
| MAX_FRAME_SIZE (0x5) | 16,777,215 (2²⁴ − 1) | Maximum permitted by RFC 7540 |
| MAX_HEADER_LIST_SIZE (0x6) | 1,048,576 (1 MiB) | Bound on header list size |

For parameters other than HEADER_TABLE_SIZE and ENABLE_PUSH, a peer MAY
advertise smaller values. When SETTINGS are advertised (see [Connection
Preface](#connection-preface)), senders MUST honor the peer's advertised
values per RFC 7540 §6.5. When SETTINGS are NOT advertised, both
endpoints SHOULD be symmetrically configured to the same parameter
values via local out-of-band configuration; implementations MUST NOT
silently apply asymmetric values that would violate inbound enforcement
(e.g., sending more than the peer's `INITIAL_WINDOW_SIZE`).

### HPACK

Senders MUST NOT add entries to a dynamic table; receivers MUST NOT use
dynamic-table state. With `HEADER_TABLE_SIZE = 0` advertised, a receiver
MUST treat any reference to an index above the static table size (61,
defined in RFC 7541 Appendix A) as a connection error of type
COMPRESSION_ERROR.

Senders MAY use Huffman encoding for string literals. Receivers MUST
support Huffman decoding.

### Length-Prefixed Message over DATA

Each gRPC application message is encoded as a 5-byte
[Length-Prefixed-Message](https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md)
header (`Compressed-Flag(1) | Message-Length(4 BE)`) followed by
`Message-Length` bytes of message body. This byte stream is carried in one
or more DATA frames. A single DATA frame MAY carry:

- one complete LPM message,
- a fragment of one LPM message (when the encoded message exceeds
  MAX_FRAME_SIZE), or
- multiple complete LPM messages.

Receivers MUST handle all three cases. Senders SHOULD emit one application
message per DATA frame so that receivers can apply zero-copy optimizations
on the common case.

When the encoded message exceeds the peer's MAX_FRAME_SIZE, the sender
splits it into the minimum number of DATA frames needed. END_STREAM, if
applicable, is set on the last DATA frame.

### CONTINUATION

Receivers MUST handle CONTINUATION frames per RFC 7540. Senders MUST emit
CONTINUATION when a header block exceeds the peer's advertised
MAX_FRAME_SIZE.

### PADDED Frames

The PADDED flag on DATA and HEADERS frames MUST be supported on the read
path. Senders SHOULD NOT set PADDED.

### Stream Lifecycle

Stream ID allocation, stream states, and state transitions follow
[RFC 7540 §5.1](https://httpwg.org/specs/rfc7540.html#StreamStates),
including the handling of frames received on closed streams.

### Flow Control

SHM transports use HTTP/2 flow control as defined in
[RFC 7540 §5.2 and §6.9](https://httpwg.org/specs/rfc7540.html#FlowControl):

- `SETTINGS_INITIAL_WINDOW_SIZE` is honored in both directions
  (see [SETTINGS](#settings)).
- Stream-level `WINDOW_UPDATE` is paced by application consumption,
  drip-credited at the receiver's `limit/4` threshold.
- Connection-level `WINDOW_UPDATE` is paced by inbound DATA receive,
  decoupled from application reads (matching stock HTTP/2 conn FC;
  see [RFC 7540 §5.2.1](https://httpwg.org/specs/rfc7540.html#FlowControlConsiderations)).
- Receivers MUST treat over-window inbound DATA as `STREAM_ERROR`
  with `FLOW_CONTROL_ERROR` per [RFC 7540 §5.2.2](https://httpwg.org/specs/rfc7540.html#StreamErrorHandler).

Receivers MAY merge adjacent connection-level `WINDOW_UPDATE` frames
within a single drain pass; HTTP/2 increments are additive so the
merged frame is wire-equivalent.

#### Stream-level pre-credit at LPM parse (MUST)

Stock HTTP/2 emits stream-level pre-credit when the application
requests to read `N` bytes — its parser calls `Read(bodyLen)` and
the transport advertises a `WINDOW_UPDATE` sufficient to admit the
remainder of `N`. SHM transports aggregate DATA frames into a
complete LPM at the codec layer before delivering to the application;
the application read does not occur until assembly completes, which
is too late to drive pre-credit while the message is in flight.

To preserve the stock HTTP/2 sender contract under this aggregation,
SHM receivers MUST emit a stream-level `WINDOW_UPDATE` sufficient
to admit the full LPM at LPM-header parse time whenever the
announced LPM does not already fit within the remaining stream
window, bypassing the regular `limit/4` drip threshold for that
message. The wire effect is identical to stock HTTP/2 stream
pre-credit; only the trigger location moves earlier.

Connection-level pre-credit is NOT required: the receiver
continuously drains DATA bytes from the ring as each frame arrives
(advancing `ReadIdx`), independent of application read pace, which
keeps connection-level inbound accounting flowing and lets
drip-on-receive emit `WINDOW_UPDATE` at the receiver's own cadence.

### Receiver Back-Pressure

To bound the memory a single misbehaving stream can consume, receivers
SHOULD apply a per-stream inbound-queue cap. On exceeding the cap the
receiver MUST emit `RST_STREAM` with a non-OK error code and drop the
offending stream. The exact cap value and error code are
implementation-defined. Reference implementations apply a hard cap on
the unconsumed message bytes per stream.

## Synchronization

### SPSC Ring Contract

- The producer MUST only write `WriteIdx`; the consumer MUST only write
  `ReadIdx`.
- Updates to `WriteIdx` and `ReadIdx` MUST use release semantics; reads MUST
  use acquire semantics.
- All bytes in the range about to be published MUST be written before
  `WriteIdx` is advanced past them. A producer MAY publish a frame
  incrementally (see [Framing on the Ring](#framing-on-the-ring)).

### Wait/Wake

When the ring is empty or full, the protocol uses an OS-provided
cross-process wait/wake primitive to avoid busy-waiting. The portable
abstraction is:

- **WaitOnAddress(addr, expected)**: block until `*addr != expected`.
- **WakeByAddress(addr)**: unblock one thread waiting on `addr`.

Concrete mappings:

| Operation | Linux | Windows |
|-----------|-------|---------|
| Wait | `futex(addr, FUTEX_WAIT, expected)` | An equivalent cross-process wait primitive (e.g. `WaitOnAddress` where the OS guarantees cross-process semantics on the same mapped section, or a named event keyed to `addr`). |
| Wake | `futex(addr, FUTEX_WAKE, 1)` | The matching wake primitive (e.g. `WakeByAddressSingle`, or signaling the named event). |

The primitive on each OS MUST be one whose wait and wake operate on
the shared physical backing of the segment (not per-process virtual
addresses); independent processes mapping the same segment MUST be
able to wake each other through this primitive.

The `DataSeq` and `SpaceSeq` fields are the primary wait/wake target
addresses. After writing data, the producer increments `DataSeq` and wakes
if `DataWaiters > 0`. After consuming data, the consumer increments
`SpaceSeq` and wakes if `SpaceWaiters > 0`. The `ContigSeq` and
`ContigWaiters` fields are reserved for implementations that require
contiguous (non-wrapping) frame placement and need an additional wait/wake
target for that condition. Implementations that allow frame payloads to
wrap around the end of the data area need not use these fields.

To avoid lost wakes, a waiter MUST increment the waiter count and re-check
the ring condition before entering the wait syscall.

### Adaptive Spin-Then-Block

Before falling back to a kernel wait, implementations MAY apply a
bounded spin on the wait-target address. Reference implementations
default to no spin (zero iterations) to avoid burning CPU on idle
connections; operators MAY enable a non-zero spin budget at deployment
time. Measured impact at the time of writing (reference
implementations, x86-64 Linux): about 2× streaming-latency improvement
at payload ≤ 4 KiB; neutral or slightly negative at payload ≥ 64 MiB.
Spin SHOULD be opt-in for latency-sensitive workloads rather than a
global default.

## Ring Sizing

Ring capacities MUST be powers of two and MUST be at least 4 KiB.
Implementations SHOULD provision ring capacity ≥ `SETTINGS_INITIAL_WINDOW_SIZE`
per direction so the HTTP/2 flow-control window remains the binding
constraint rather than physical ring back-pressure. Reference
implementations default to 64 MiB rings per direction
(136 MiB total mapped segment, including both rings and headers).
Smaller rings (~64 KiB) suit low-stream-count deployments; larger rings
primarily increase the number of in-flight streams the transport can
sustain without producer stalls, rather than per-stream throughput.

Implementations exposing a user-facing `SegmentSize` knob MUST validate
`SegmentSize ≥ RingACapacity + RingBCapacity + header overhead` and
reject under-sized configurations at the API boundary.

## Security Considerations

### Threat Model

SHM transport runs on a single host. Its security model relies on OS
process isolation, similar to Unix domain sockets. The protocol does not
defend against a malicious process that already has permission to map the
shared memory region.

### Segment Names

Control segment names SHOULD contain a cryptographically random component
(see [Control Segment Naming](#control-segment-naming)). A predictable
name allows a rogue process to pre-create a segment with the same name and
intercept connections.

### File Permissions

The shared memory backing file SHOULD be readable and writable only by
processes that need access. When server and client run as the same OS user,
Linux file mode 0600 is sufficient. Cross-user deployments require broader
permissions (e.g. a shared group), which increases the attack surface.

### Data Confidentiality and Integrity

Segment contents are neither encrypted nor signed. Any process with
mapping permission can read and write arbitrary bytes. Deployments that
require confidentiality SHOULD restrict access through OS file permissions
rather than protocol-level encryption, which would negate the performance
benefit of shared memory.

### Process Identity

The ServerPID and ClientPID fields in the segment header are informational
and MUST NOT be used for authentication (PIDs may be recycled). Process
authentication beyond PID is deferred to the security handshake described
in [Security Handshake](#security-handshake).

### Denial of Service

A malicious client may hold the control-segment write lock indefinitely,
preventing other clients from connecting. Implementations SHOULD apply a
timeout when acquiring the lock. A malicious peer may also fill the ring
without reading, causing the other side to block on writes; ring-level
backpressure is inherent to the protocol.

A server MUST also bound CPU and memory spent on malformed control-plane
frames: the receiver MUST cap accepted `Length` values (4 KiB is
sufficient for currently defined frame types), SHOULD bound the number
of consecutive malformed frames it tolerates before tearing the listener
down, and SHOULD insert a short backoff between recovery attempts so a
hostile peer flooding the ring cannot drive the listener thread to
consume a full CPU core. See [Control Frame Envelope](#control-frame-envelope)
for the per-frame cap.

A new listener attempting to bind to a name whose control segment is
already in use (i.e., the existing segment opens successfully and reports
`ServerReady == 1`) MUST refuse rather than silently unlinking the
existing segment; see [Establishment Sequence](#establishment-sequence).

## Implementation

The protocol requires a platform that supports:

- Memory-mapped files shared between processes.
- A cross-process wait/wake primitive operating on the shared
  physical backing (e.g. Linux `futex`, or a Windows primitive
  that provides equivalent cross-process semantics on a mapped
  section). See [Wait/Wake](#waitwake).
- (optional) A cross-process file-descriptor passing mechanism for
  eventfd-based wake amplification on Linux (`SCM_RIGHTS` over
  `AF_UNIX`). Without this mechanism, cross-process wakes fall back to
  the address-wait/wake primitive.

## Linux Cross-Process Wake Amplifier

On Linux, an implementation MAY reduce cross-process wake latency
using `eventfd(2)` instances exchanged via `SCM_RIGHTS` over a sibling
`AF_UNIX` socket. This section defines the wire format so independent
implementations interoperate.

### Socket Path

For a data segment whose backing file is at path `<seg>`, the
fd-passing socket is at `<seg>.fds.sock`. The server (creator) binds
and listens on this socket as part of creating the segment; the client
(opener) connects to it after mapping the segment but before setting
`ClientReady`.

### Wire Payload

The server sends a single message containing:

- A 4-byte ASCII token `"FDS\n"` (0x46 0x44 0x53 0x0A) as the ordinary
  `sendmsg(2)` payload, so the client can detect a wrong-protocol
  partner on a stale socket.
- An `SCM_RIGHTS` ancillary message carrying exactly two file
  descriptors:
  - `fd[0]`: client-to-server direction eventfd. The client writes a 1
    (`u64` write) to wake the server.
  - `fd[1]`: server-to-client direction eventfd. The server writes a 1
    to wake the client.

The client MUST verify the token before consuming the file descriptors
and MUST close any extra descriptors it receives.

### Peer-UID Check

The server MUST consult `SO_PEERCRED` (or equivalent) on the accepted
connection and serve file descriptors only when the peer's UID matches
the server's own UID. This prevents an unrelated local user from
hijacking the cross-process wake channel.

### OpenerWakeReady Coordination

After the client successfully receives and arms its eventfd pair, it
MUST set `OpenerWakeReady = 1` in the segment header BEFORE setting
`ClientReady`. If fd-passing fails (socket unavailable, token mismatch,
UID mismatch, recvmsg error, etc.), the client MUST leave
`OpenerWakeReady` at 0 and proceed with the address-wait/wake path
only. The server reads this flag immediately after the client signals
`ClientReady` and uses it to decide whether to retain its own eventfd
waker or release it (preventing an asymmetric park-on-eventfd /
wake-on-futex deadlock).

## Open issues (if applicable)

* **macOS support.** macOS does not provide `futex`. Plausible
  candidates include `EVFILT_USER` on `kqueue` for cross-process wake
  amplification and `os_unfair_lock` / `dispatch_semaphore` for the
  in-process spin/park primitive. None has yet been validated.

* **Security handshake wire format.** Frame types 0x20–0x2F are
  reserved (see [Security Handshake](#security-handshake)). The
  reference implementations carry an experimental nonce-based handshake
  whose wire format will be normative in a follow-up gRFC after
  cross-implementation interop has been demonstrated.

* **Cross-container IPC.** Containers isolate IPC namespaces by default.
  Shared memory between containers requires either a shared IPC namespace or
  a volume pointing to the same backing file.

* **Stale segment cleanup.** If a server process crashes, its shared memory
  backing files and lock files may remain on disk. Cleanup of stale
  segments is implementation-defined.
