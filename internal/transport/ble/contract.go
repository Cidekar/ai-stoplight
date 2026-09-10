package ble

// ChunkContract is the wire contract between this transport and the firmware's
// BLE peripheral. It is a documented constant rather than a comment so the
// firmware readme can quote it and a change to it cannot be made silently.
//
// Both sides are written against this text. Change it and the firmware breaks.
const ChunkContract = `
Stoplight BLE wire contract (central -> peripheral)

GATT
  Service        6e5d0001-b5a3-f393-e0a9-e50e24dcca9e   advertised
  Characteristic 6e5d0002-b5a3-f393-e0a9-e50e24dcca9e   write, no read, no notify

  The peripheral MUST advertise the service UUID in its advertisement packet.
  The central matches on that UUID and nothing else, so a board that omits it
  is invisible however it is named. The advertised local name is a label for
  humans only.

Payload
  The characteristic carries the same byte stream as the serial transport:
  one JSON object, then one '\n'. The newline is the frame delimiter. The
  bytes are produced by transport.EncodeFrame and are identical over either
  link, so the firmware parses one format.

Chunking
  1. A frame is split into consecutive chunks and written in order on one
     connection. Chunk N+1 is written only after the write of chunk N is
     acknowledged, so the peripheral receives them in order.
  2. Chunk size is the negotiated ATT MTU minus 3, clamped to [20, 244].
     A link that reports no MTU gets 20, the default every BLE stack supports.
  3. Boundaries are arbitrary. A chunk is a slice of bytes at a byte offset,
     not a token, a line or a JSON value. A split can fall inside a string,
     inside a number, between the two bytes of a UTF-8 sequence, or between
     the last '}' and the '\n'.
  4. There is no header, no length prefix, no sequence number and no
     end-of-frame marker other than the '\n' in the payload itself.
  5. A frame whose byte count is an exact multiple of the chunk size produces
     no empty trailing chunk. The peripheral must not wait for one.
  6. Writes use write-with-response, so every chunk is acknowledged. A
     peripheral that rejects a write causes the central to abandon the rest of
     that frame and mark itself disconnected.

Reassembly, on the peripheral
  Append every received chunk to a line buffer. Scan the appended bytes for
  '\n'. On finding one, the bytes before it are one complete frame: parse it
  and reset the buffer. Bytes after it begin the next frame.

  This is the same algorithm the serial reader already uses. A BLE chunk is
  indistinguishable from a partial serial read, so the existing LineReader
  works unchanged if it is fed chunk bytes instead of Serial bytes.

Failure modes the peripheral must tolerate
  - A truncated frame. If the link drops mid-frame, the '\n' never arrives.
    The partial line stays in the buffer and the next frame's bytes append to
    it, producing one corrupt line that fails to parse. Discard a line that
    exceeds SL_LINE_MAX rather than growing the buffer. The central resends a
    whole frame after it reconnects, so a discarded line costs one update and
    never wedges the light.
  - A repeated frame. After a reconnect the relay resends the current frame,
    so the same content can arrive twice. Frames are idempotent: applying one
    twice is indistinguishable from applying it once.
  - Chunk size changing between connections. The MTU is renegotiated on every
    connection, so the peripheral must not assume a fixed chunk size or infer
    a frame boundary from the length of a write.
`
