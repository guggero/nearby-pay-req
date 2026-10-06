# nearby-pay-req

A Go implementation of **Nearby Payment Request Exchange**: wallet-to-wallet
sharing of a payment request over Bluetooth LE and NFC, so a payer doesn't
have to point a camera at a QR code.

The payee wallet keeps showing its receive screen. The payer wallet opens its
send screen, finds the payee nearby and receives the exact string the QR code
encodes (a `lightning:` URI, a BIP 21 URI, a BOLT 11 invoice, a BOLT 12
offer, …). Both screens then show the same six-digit code, and the payer
confirms the codes match before paying. There is no pairing, no account, no
long-term key and no OS prompt.

The protocol is written up as a bLIP draft in [`spec/`](spec/). This library
is its reference implementation, and its tests reproduce the draft's test
vectors byte for byte.

> **Status: draft.** The wire format may still change before the bLIP is
> merged, and nothing here has been externally audited yet.

## How it works

- **BLE carrier.** The payee runs a GATT server on a fixed service UUID and
  advertises it without a name or manufacturer data. The payer picks the
  closest advertiser by smoothed RSSI, connects and runs five messages:
  1. A `Noise_NN_25519_ChaChaPoly_SHA256` handshake. The payee's reply
     carries a commitment `SHA-256(tag ‖ Nb)` to its nonce.
  2. The payer sends its nonce `Na`.
  3. The payee reveals `Nb` together with the payment request.
  4. The payer acknowledges.

  Both sides derive the code from the handshake hash and both nonces. The
  payee commits to its nonce before it learns the payer's, so a man in the
  middle cannot grind for a matching code: each attempt succeeds with
  probability 10⁻⁶.
- **NFC carrier (Android payees).** The same string is served as a read-only
  NFC Forum Type 4 Tag holding one NDEF URI record. Any NFC reader, wallet or
  not, sees exactly what the QR code shows. The physical tap is the
  authentication.
- **Opaque payload.** The transport never parses the payment request. The
  payer runs it through the same code path as a scanned QR code, so no new
  parsing surface is added.

Read the [spec](spec/blip-nearby-payment-requests.md) for the full wire
format, the rationale and the security considerations.

## Packages

| Package | What it does |
|---|---|
| [`nearby`](.) (root) | The `Manager`: drives the platform radio, runs one share and one find at a time, and handles peer selection, timeouts, rate limiting and the suspicious-activity warning. It reports progress as typed `ShareEvent` / `FindEvent` values. |
| [`radio`](radio/) | The `Radio` interface the platform implements (BLE peripheral, BLE central, HCE toggle) and its callback interfaces. It only uses gomobile-bindable types. |
| [`session`](session/) | The protocol as sans-IO state machines (`PayerSession`, `PayeeSession`): bytes in, bytes out, with no goroutines, clocks or radios of its own. |
| [`wire`](wire/) | Chunk layer (ATT-sized fragments), message header, ABORT codes and BOLT 1 TLV helpers. |
| [`hce`](hce/) | A Type 4 Tag emulator that answers command APDUs from an Android `HostApduService`. |
| [`ndef`](ndef/) | Encoding and strict decoding of the single-URI-record NDEF message the tag serves. |
| [`nearbytest`](nearbytest/) | In-memory radios linked through a shared `World`, for end-to-end tests without Bluetooth, both here and in apps that embed the `Manager`. |
| [`spec/`](spec/) | The bLIP draft, its test vectors, and an independent vector generator ([`spec/gen`](spec/gen/), its own module) written from the spec text alone. |

## Usage

```go
mgr := nearby.New(nearby.Config{
	Radio: platformRadio, // your radio.Radio implementation
})
defer mgr.Stop()

// Payee: serve the receive screen's QR string until the screen closes.
err := mgr.Share(ctx, "lightning:lnbc1...", nearby.ShareOptions{
	NFC: true,
}, func(ev nearby.ShareEvent) error {
	switch ev := ev.(type) {
	case nearby.PeerConnected:
		showCode(ev.Code) // the payer sees the same code
	case nearby.Delivered:
		markDelivered(ev.Code)
	case nearby.SuspiciousActivity:
		warn(ev.FailedSessions, ev.Window)
	}
	return nil
})

// Payer: find one request, checked by the wallet's own parser.
err = mgr.Find(ctx, nearby.FindOptions{
	Validate: func(req string) error {
		_, err := wallet.ParsePaymentRequest(req)
		return err
	},
}, func(ev nearby.FindEvent) error {
	if ev, ok := ev.(nearby.Received); ok {
		// Ask the user to compare ev.Code with the payee's screen.
		// If the codes differ, call Find again with
		// FindOptions{Exclude: []string{ev.PeerID}}.
	}
	return nil
})
```

`Share` and `Find` block and call `emit` on the calling goroutine, so they
map directly onto a server-streaming RPC handler.

They end with:

- `ErrReplaced` when a newer call of the same kind took over;
- `ErrPaused` after `Pause`, for example when the app goes to the background;
- `ErrScanFailed` when the OS stopped a find's scan;
- the context's error when `ctx` is cancelled;
- `ErrInvalidPaymentRequest` or an `*UnavailableError` before starting, when
  the call can never work. Bluetooth being off is not one of these cases:
  the call waits and reports `AvailabilityChanged` instead.

The platform also calls three things on the `Manager`:

- `StatusChanged` from its Bluetooth, permission and NFC state listeners;
- `ProcessAPDU` from its `HostApduService`;
- `Status` before showing nearby controls at all.

## Implementing the radio

The radio is a byte pump. Chunking, UUIDs, encryption and timeouts all stay
in Go, so the native side is a thin wrapper around the platform's Bluetooth
API. The doc comments in [`radio/radio.go`](radio/radio.go) are the contract.

What implementations most often get wrong:

- **No security on the attributes.** The `rx` write and `tx` notify
  characteristics must not require encryption or authentication. Otherwise
  the OS offers to pair, which the protocol forbids.
- **Advertise only the service UUID.** No local name, no manufacturer data
  and no service data, and only while a payment request is on screen.
- **Payee: refuse prepared (long) writes.** Report `maxChunk` as the
  negotiated MTU − 3 (Android) or `central.maximumUpdateValueLength` (iOS).
- **Payer: report the single-write size.** That is the negotiated MTU − 3.
  On iOS, use `maximumWriteValueLength(for: .withoutResponse)`: the
  `.withResponse` value is 512 whatever the MTU, and writes above MTU − 3
  turn into prepared writes the payee refuses. Request the largest MTU
  before reporting `OnConnected` on Android.
- **Callbacks must not block.** The Go callbacks only enqueue. Call them
  from any thread, but never while holding a lock the Go side might need
  through another radio method.
- **Android HCE.** Register a `HostApduService` for AID `D2760000850101`
  in category `other`, with `requireDeviceUnlock="true"`. It must never be a
  payment service. Make it the preferred service only while `SetHceActive`
  is on. The tag answers `6A82` whenever nothing is shared, so it never
  captures taps meant for other apps.
- **Android permissions.** `BLUETOOTH_SCAN` (with `neverForLocation`),
  `BLUETOOTH_CONNECT` and `BLUETOOTH_ADVERTISE`. The feature needs Android 12
  or later, which is where these permissions exist.
- **iOS.** CoreBluetooth needs `NSBluetoothAlwaysUsageDescription`. iOS
  cannot emulate NFC tags, so iOS payees share over BLE only.

### gomobile

gomobile can only bind interfaces declared in the bound package. Declare
copies of `radio.Radio` and its two callback interfaces in your binding
package for Kotlin and Swift to implement. Then pass the native value to
the `Manager` through an adapter that overrides the two methods whose
callback parameter has a different named type:

```go
type radioAdapter struct{ bindings.NearbyRadio }

func (a radioAdapter) StartAdvertising(service, rx, tx string,
	cb radio.PeripheralCallback) error {

	return a.NearbyRadio.StartAdvertising(service, rx, tx, cb)
}

func (a radioAdapter) StartScan(service string, cb radio.CentralCallback) error {
	return a.NearbyRadio.StartScan(service, cb)
}
```

`cb` can be passed straight through because the method sets are identical.

## Logging

The root package logs through [btclog](https://github.com/btcsuite/btclog)
and stays silent until you call `nearby.UseLogger`. Payment requests
and codes are only logged at trace level. Keep trace disabled in production
builds, as the spec recommends.

## Development

```sh
make check            # build, golangci-lint, unit tests with -race
make unit pkg=session case=TestSpecVectors
make fuzz             # fuzz the NDEF decoder and the chunk reassembler
make vectors          # regenerate spec/vectors.json from spec/gen
```

Two sets of vectors guard the protocol:

- `spec/vectors.json` is the bLIP's set, produced by the independent
  generator. `session.TestSpecVectors` replays it with the vectors' fixed
  keys and nonces.
- `session/testdata/vectors.json` is this implementation's own regression
  set. Regenerate it with
  `go test ./session -run TestVectors -args -update`. It only ever needs
  regenerating when the protocol changes, which also means a new protocol
  version.

## License

The code is released under the [MIT license](LICENSE). The specification
text in `spec/` is CC0, as stated in the document.
