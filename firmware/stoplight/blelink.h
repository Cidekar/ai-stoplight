#ifndef STOPLIGHT_BLELINK_H
#define STOPLIGHT_BLELINK_H

#include <stdbool.h>
#include <stdint.h>

#include "protocol.h"

// BleLink is the GATT peripheral half of the Stoplight BLE transport. The
// contract both ends implement is ble.ChunkContract in
// internal/transport/ble/contract.go, which is a Go constant rather than a
// comment so neither side can change it silently.
//
// WHAT THIS MODULE IS NOT. It does not parse. A chunk arrives, its bytes go
// into a queue, and loop() drains that queue into the same LineReader the
// serial path uses. The contract is explicit that this must be so:
//
//   "A BLE chunk is indistinguishable from a partial serial read, so the
//    existing LineReader works unchanged if it is fed chunk bytes instead
//    of Serial bytes."
//
// Two readers would be two chances to disagree about the frame format, and
// the BLE one would be the one nobody watches.
//
// WHY A QUEUE. The write callback runs on the Bluetooth stack's task, not on
// the Arduino loop task. Parsing there would touch Display and Lamps from a
// second thread, and neither is synchronised. The queue is the handoff: the
// callback only appends bytes, and every byte is consumed by loop().

// The service and characteristic UUIDs from ChunkContract. The central
// matches on the service UUID and nothing else, so a board that advertises a
// different one is invisible however it is named.
#define BLE_SERVICE_UUID "6e5d0001-b5a3-f393-e0a9-e50e24dcca9e"
#define BLE_CHAR_UUID "6e5d0002-b5a3-f393-e0a9-e50e24dcca9e"

// The advertised local name. A label for humans only: nothing matches on it.
#define BLE_DEVICE_NAME "stoplight"

// BLE_RX_CAPACITY is the byte queue between the Bluetooth task and loop().
//
// Sized to hold one whole frame plus headroom, so a frame that arrives in
// chunks faster than one loop() iteration drains it is not torn. At
// SL_LINE_MAX 1536 the worst case frame fits with room for the next frame's
// first chunks to begin arriving.
#define BLE_RX_CAPACITY 2048

// BleLink owns the peripheral. One instance; begin() is called once.
class BleLink {
 public:
  BleLink();

  // begin starts the stack, publishes the service and starts advertising.
  // Safe to call when no central is present: advertising simply continues
  // until one connects.
  void begin();

  // available reports how many bytes are queued.
  uint16_t available() const;

  // read removes and returns the next byte, or -1 when the queue is empty.
  // Mirrors Serial.read() so the drain loop in loop() reads the same for
  // both transports.
  int read();

  // connected reports whether a central is currently connected. Two callers:
  // the re-advertise decision, and standby, which treats a live link as
  // activity so a connected-but-quiet central does not blank the light and so
  // the grace timer only counts from the moment the link actually drops. Frame
  // handling itself still does not depend on it. See standby.{h,cpp}.
  bool connected() const { return connected_; }

  // onConnect and onDisconnect are called from the stack's task. They are
  // public because the callback objects are separate classes.
  void onConnect();
  void onDisconnect();

  // push appends one chunk's bytes to the queue. Called from the stack's
  // task by the write callback.
  //
  // A chunk that does not fit is DROPPED WHOLE rather than partially
  // written. A half-written chunk would leave a frame with a hole in the
  // middle, which parses as corrupt and costs the frame anyway, and could
  // corrupt the FOLLOWING frame too by consuming its newline. Dropping the
  // chunk costs one frame and the next one arrives intact. The contract
  // already requires tolerating a truncated frame for exactly this reason.
  void push(const uint8_t* data, uint16_t len);

 private:
  // A plain ring buffer. Written by the Bluetooth task through push(), read
  // by the Arduino task through read(). head_ is only advanced by the
  // reader and tail_ only by the writer, so single-byte reads and writes of
  // volatile indices are enough on this core: there is one producer and one
  // consumer, and neither index is written by both.
  volatile uint16_t head_;
  volatile uint16_t tail_;
  volatile bool connected_;
  uint8_t buf_[BLE_RX_CAPACITY];
};

#endif  // STOPLIGHT_BLELINK_H
