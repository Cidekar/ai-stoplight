#include "blelink.h"

#ifndef SL_HOST_TEST
#include <BLE2902.h>
#include <BLEDevice.h>
#include <BLEServer.h>
#include <BLEUtils.h>
#endif

BleLink::BleLink() : head_(0), tail_(0), connected_(false) {}

uint16_t BleLink::available() const {
  // Unsigned subtraction, so a wrapped tail_ still yields the true count.
  return (uint16_t)((tail_ - head_ + BLE_RX_CAPACITY) % BLE_RX_CAPACITY);
}

int BleLink::read() {
  if (head_ == tail_) {
    return -1;
  }
  uint8_t c = buf_[head_];
  head_ = (uint16_t)((head_ + 1) % BLE_RX_CAPACITY);
  return (int)c;
}

void BleLink::push(const uint8_t* data, uint16_t len) {
  if (data == nullptr || len == 0) {
    return;
  }

  // One slot is left permanently empty so that head_ == tail_ means empty
  // and never full. Capacity is therefore BLE_RX_CAPACITY - 1.
  uint16_t free = (uint16_t)(BLE_RX_CAPACITY - 1 - available());
  if (len > free) {
    // Drop the whole chunk. See the note in blelink.h: a partial write puts
    // a hole in the middle of a frame, which costs that frame AND can eat
    // the next frame's newline.
    return;
  }

  for (uint16_t i = 0; i < len; i++) {
    buf_[tail_] = data[i];
    tail_ = (uint16_t)((tail_ + 1) % BLE_RX_CAPACITY);
  }
}

void BleLink::onConnect() { connected_ = true; }

void BleLink::onDisconnect() {
  connected_ = false;
  // The partial line left in the reader is NOT cleared here, and that is
  // deliberate. The contract requires tolerating a truncated frame: the
  // next frame's bytes append to the remains of the last one, producing one
  // corrupt line that fails to parse and is discarded. The central resends
  // a whole frame after it reconnects, so the cost is one update.
  //
  // Clearing it would need a reference to the reader, which would make this
  // module know about parsing. It does not.
}

#ifndef SL_HOST_TEST

// The globals below exist because the BLE API takes raw callback objects
// with no user-data pointer, so the callbacks cannot be handed an instance.
// There is exactly one BleLink, created in the sketch, so a file-scope
// pointer to it is the whole of the indirection.
static BleLink* g_link = nullptr;
static BLEAdvertising* g_advertising = nullptr;

class ServerCallbacks : public BLEServerCallbacks {
  void onConnect(BLEServer*) override {
    if (g_link != nullptr) {
      g_link->onConnect();
    }
  }

  void onDisconnect(BLEServer*) override {
    if (g_link != nullptr) {
      g_link->onDisconnect();
    }
    // Advertising stops on connect and does NOT restart on disconnect by
    // itself. Without this the light is invisible after the first central
    // ever disconnects, and the only cure is a power cycle.
    if (g_advertising != nullptr) {
      g_advertising->start();
    }
  }
};

class WriteCallbacks : public BLECharacteristicCallbacks {
  void onWrite(BLECharacteristic* characteristic) override {
    if (g_link == nullptr || characteristic == nullptr) {
      return;
    }
    // getData() and getLength() rather than getValue(): a chunk boundary is
    // arbitrary and may fall between the two bytes of a UTF-8 sequence, so
    // the payload is bytes and not a string. A String round trip would also
    // stop at an embedded NUL, and the contract does not forbid one.
    uint8_t* data = characteristic->getData();
    size_t len = characteristic->getLength();
    if (data == nullptr || len == 0) {
      return;
    }
    // A single write larger than the queue cannot happen at any negotiated
    // MTU, but clamping costs one comparison and removes the question.
    if (len > BLE_RX_CAPACITY) {
      len = BLE_RX_CAPACITY;
    }
    g_link->push(data, (uint16_t)len);
  }
};

static ServerCallbacks g_serverCallbacks;
static WriteCallbacks g_writeCallbacks;

void BleLink::begin() {
  g_link = this;

  BLEDevice::init(BLE_DEVICE_NAME);

  BLEServer* server = BLEDevice::createServer();
  server->setCallbacks(&g_serverCallbacks);

  BLEService* service = server->createService(BLE_SERVICE_UUID);

  // Write only. The contract gives this characteristic no read and no
  // notify: the light is a sink, and the relay never asks it anything.
  BLECharacteristic* characteristic = service->createCharacteristic(
      BLE_CHAR_UUID, BLECharacteristic::PROPERTY_WRITE);
  characteristic->setCallbacks(&g_writeCallbacks);

  service->start();

  g_advertising = BLEDevice::getAdvertising();
  // THE LINE THAT MAKES THE LIGHT FINDABLE. The central matches on the
  // service UUID in the advertisement and nothing else, so a board that
  // omits this is invisible however it is named and however well the rest
  // of this file works.
  g_advertising->addServiceUUID(BLE_SERVICE_UUID);
  g_advertising->setScanResponse(true);
  BLEDevice::startAdvertising();
}

#else  // SL_HOST_TEST

// On a host there is no radio. begin() is a no-op so the queue, the drop
// rule and the reassembly can be tested without one.
void BleLink::begin() {}

#endif  // SL_HOST_TEST
