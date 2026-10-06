```
bLIP: XXXX (assigned from the pull request number)
Title: Nearby Payment Request Exchange over BLE and NFC
Status: Draft
Author: Oli <gugger@gmail.com>
Created: 2026-10-06
License: CC0
```

## Abstract

This bLIP defines how a wallet that is showing a payment request (the
*payee*) offers that request to a wallet within a few metres (the *payer*),
without the payer pointing a camera at a QR code.

The payload is opaque: it is exactly the string the payee's QR code would
encode, such as a `lightning:` URI, a BIP 21 URI, a bare BOLT 11 invoice or a
BOLT 12 offer. The payer treats it exactly as if it had scanned it.

Two carriers are defined:

* **Bluetooth Low Energy (BLE).** The payee runs a GATT server on a fixed
  service UUID. A payer connects and runs a five-message exchange: a
  `Noise_NN_25519_ChaChaPoly_SHA256` handshake followed by a commit/reveal of
  two nonces. Both wallets then display the same six-digit *comparison code*,
  and the payer's user confirms the codes match before paying. This
  authenticates the request against a nearby attacker without pairing,
  without long-term keys and without any OS prompt.
* **NFC.** A payee whose platform supports host card emulation serves the
  same string as a read-only NFC Forum Type 4 Tag holding one NDEF URI record.
  The physical tap is the authentication.

A shared service UUID and payload format let any two compliant wallets
exchange payment requests, whatever wallet software or payment rail each one
uses.

## Copyright

This bLIP is licensed under the CC0 license.

## Motivation

Paying someone in person still almost always means scanning a QR code. That
works, but it has well-known friction:

* The payer has to open a camera view, hold the phones at the right distance
  and angle, and fight glare, low light and cracked or dimmed screens.
* Large payment requests produce dense QR codes. BOLT 11 invoices with route
  hints, BIP 21 URIs carrying a `lightning=` parameter, and BOLT 12 offers with
  blinded paths can each exceed 1,000 characters. Codes that dense scan
  slowly or not at all on older cameras.
* Accessibility. Aiming a camera at a small code is hard for users with
  limited vision or motor control.

Earlier attempts show the demand. Bitcoin Wallet for Android shipped BIP 70
payment requests over classic Bluetooth and NFC in 2014, and NFC tag
emulation is reappearing in point-of-sale apps (see
[Prior art](#prior-art)). Some wallets also work around the problem with
proprietary "tap to pay" or "send to nearby" features. Each one uses its own UUIDs, framing and security
model, so they only interoperate with themselves. A user can only pay a
nearby friend this way if both run the same app. And because every
proprietary scheme advertises its own identifier, the advertisement also
reveals which app is in use.

Phones can already move a few kilobytes reliably between them over BLE GATT,
and many Android phones can emulate NFC tags. What is missing is an agreed
wire format and an agreed authentication step. This bLIP provides both,
keeping the payload exactly as opaque as a QR code, so a wallet can add the
feature without a new parsing surface and support new payment formats on
the same day it supports them in its QR scanner.

## Specification

The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT", "SHOULD",
"SHOULD NOT", "RECOMMENDED", "MAY", and "OPTIONAL" in this document are to be
interpreted as described in [BCP 14][bcp14] when, and only when, they appear in
all capitals.

`‖` denotes byte concatenation. All multi-byte integers are big-endian unless
stated otherwise. `SHA-256(x)` is the 32-byte SHA-256 digest of `x`.

### Roles

* The **payee** is the wallet currently displaying a payment request it
  wants to be paid on. It is the BLE GATT *peripheral* (server) and the Noise
  *responder*.
* The **payer** is the wallet that wants to pay. It is the BLE GATT *central*
  (client) and the Noise *initiator*.

One device MAY act in both roles at the same time, for example with a
receive screen still open underneath a send screen. The two roles are
independent.

### Payment request payload

The payload is a UTF-8 string, called `payment_request` below.

The payee:
  - MUST set `payment_request` to the exact string it would encode in a QR
    code for the same request.
  - SHOULD use a URI form with a scheme (`lightning:`, `bitcoin:`, …) where one
    exists.
  - MUST NOT send a `payment_request` that is empty, longer than 8192 bytes,
    not valid UTF-8, or that contains a NUL (`0x00`) byte.

The payer:
  - MUST process a received `payment_request` exactly as it would process the
    same string decoded from a scanned QR code, including all validation.
  - MUST treat a `payment_request` that is empty, longer than 8192 bytes, not
    valid UTF-8 or contains a NUL byte as rejected (see
    [Session flow](#session-flow)).

The transport never inspects the payload, so any format the payer's QR
scanner accepts is carried unchanged.

### BLE carrier

#### GATT service

| Attribute | UUID | Properties | Permissions |
|---|---|---|---|
| Service "Nearby Payment Request v1" | `7be78411-b151-48bf-a570-252b55847970` | primary | — |
| `rx` characteristic (payer → payee) | `35effe11-bcc0-4db2-8ef4-c7271011c5e6` | write (with response) | writeable; no encryption or authentication required |
| `tx` characteristic (payee → payer) | `13cccaba-d2e9-47d5-ab11-2b3730d3c97e` | notify, with a Client Characteristic Configuration descriptor (`0x2902`) | none |

The payee:
  - MUST expose exactly the service and characteristics above.
  - MUST NOT require encryption, authentication or authorisation on either
    characteristic or the descriptor. Pairing or bonding MUST NOT be
    initiated or required.
  - MUST NOT expose a readable characteristic in this service.
  - MUST reject prepared (long) writes to `rx`.

The payer:
  - MUST NOT initiate pairing or bonding with the payee.
  - MUST enable notifications on `tx` before its first write to `rx`.
  - MUST write each chunk (see [Chunk layer](#chunk-layer)) to `rx` as a single
    ATT Write Request. It MUST NOT use Write Command (write without response)
    or prepared writes.

#### Advertising

The payee:
  - MUST advertise connectable, with the 128-bit service UUID in the
    advertising data as a complete or incomplete list of 128-bit service
    UUIDs.
  - MUST NOT include a local name, manufacturer-specific data or service
    data in the advertising data or the scan response.
  - SHOULD use a private (resolvable or non-resolvable) device address where
    the platform allows.
  - MUST advertise only while it is displaying the payment request it would
    serve, and SHOULD stop advertising as soon as that request is no longer
    on screen or the application leaves the foreground.

#### Discovery and peer selection

The payer:
  - SHOULD scan for the service UUID only while the user is on a screen
    where a payment request can be entered.
  - SHOULD NOT connect to the first payee it sees. It SHOULD keep collecting
    advertisements for a short window after the first sighting (800 ms is
    RECOMMENDED), track a moving average of each payee's RSSI (an exponential
    moving average with a new-sample weight of 0.3 is RECOMMENDED), and then
    connect to the payee with the strongest average.
  - SHOULD NOT connect to a payee whose averaged RSSI is below a proximity
    floor (−75 dBm is RECOMMENDED). It SHOULD instead tell the user to move
    closer.
  - MUST NOT reconnect to a payee whose request the user said did not match
    (see [Comparison code](#comparison-code)) for the rest of the current
    search.
  - SHOULD skip a payee whose session failed earlier in the same search and
    try the next candidate.

Payee selection is a usability heuristic, not a security mechanism. Picking
the wrong payee is caught by the comparison code.

#### Connection setup

After connecting, the payer discovers the service and both characteristics,
then enables `tx` notifications by writing `0x0001` to its descriptor. The
payer SHOULD request the largest ATT MTU the platform allows.

The *chunk size* for each direction is `min(512, ATT_MTU − 3)`, where
`ATT_MTU` is the MTU negotiated for this connection.

* The payer MUST NOT write a chunk larger than its chunk size, even if the
  platform API reports a larger maximum write length for writes with
  response.
* The payee MUST NOT notify a chunk larger than its chunk size.
* If the chunk size is below 20 bytes, the session MUST be abandoned.

The payee:
  - MUST serve at most one payer at a time. A payer that writes while another
    payer's session is active MUST be answered with `ABORT(BUSY)` (if a
    notification to it is possible) and disconnected.
  - MUST rate-limit new sessions. It SHOULD accept at most one new session
    per second and at most 30 per rolling 60 seconds, and SHOULD answer any
    session beyond that limit with `ABORT(BUSY)`.

### Chunk layer

Every ATT value written to `rx` or notified on `tx` is one *chunk*. A chunk
carries part of one *message*:

```
byte 0       flags   bit 7 START, bit 6 END, bits 5..0 reserved (0)
byte 1       seq     uint8: 0 on the START chunk, +1 per chunk, wrapping 255 → 0
bytes 2..3   len     uint16 total message length; present only when START is set
rest         data    at least one byte of message data
```

The sender of a message:
  - MUST split it into consecutive chunks of at most the chunk size. The
    first chunk has START set and carries `len`; the last has END set. A
    message that fits into one chunk has both flags set.
  - MUST set the reserved flag bits to 0.
  - MUST NOT send a message longer than 12288 bytes.

The receiver of a chunk MUST fail the session with `PROTOCOL_ERROR` if:
  - any reserved flag bit is set;
  - a chunk without START arrives when no message is in progress, or a
    chunk with START arrives while one is;
  - the START chunk's `seq` is not 0, or a later chunk's `seq` is not the
    previous chunk's `seq` plus one, modulo 256;
  - `len` is 0 or greater than 12288;
  - a chunk carries no data;
  - the received data would exceed `len`;
  - END is set before `len` bytes arrived, or `len` bytes arrived without END
    being set.

The chunk layer has no retransmission or windowing. ATT delivers write
requests and notifications on a live link reliably and in order, the
exchange below alternates strictly between the two directions, and a broken
link fails the session.

### Message layer

Every reassembled message starts with a two-byte header:

```
byte 0   version   0x01 for this specification
byte 1   type
rest     body
```

| Type | Name | Direction | Body |
|---|---|---|---|
| `0x01` | `NOISE_E` | payer → payee | Noise handshake message 1 |
| `0x02` | `NOISE_E_EE` | payee → payer | Noise handshake message 2; its payload is a TLV stream |
| `0x03` | `SEALED` | both | a Noise transport message whose plaintext is a TLV stream |
| `0x7f` | `ABORT` | both | plaintext abort reason (see [ABORT](#abort)) |

A receiver:
  - MUST answer a message whose `version` it does not support with
    `ABORT(VERSION_UNSUPPORTED)` and end the session.
  - MUST treat an unknown `type`, or a known type that is not the next one
    expected in [Session flow](#session-flow), as `PROTOCOL_ERROR`.

An `ABORT` MUST be processed whatever its `version` byte.

### TLV records

The handshake payload of `NOISE_E_EE` and the plaintext of every `SEALED`
message are TLV streams, encoded as in [BOLT 1][bolt1]: `type` and `length` are
BigSize, and records appear in strictly increasing `type` order. Even types
are mandatory: a receiver MUST fail the session with `PROTOCOL_ERROR` on an
unknown even type. Odd types are optional: a receiver MUST ignore an unknown
odd type.

| Type | Name | Carried in | Value |
|---|---|---|---|
| 0 | `commitment` | message 2 | 32 bytes |
| 2 | `payer_nonce` | message 3 | 32 bytes |
| 4 | `payee_nonce` | message 4 | 32 bytes |
| 6 | `payment_request` | message 4 | UTF-8 string, 1–8192 bytes |
| 8 | `ack` | message 5 | 1 byte: `0` = received and usable, `1` = rejected |

Each message MUST carry every mandatory record listed for it, and no other
even type. An even type that is defined but not listed for the message at
hand counts as unknown for that message. The receiver MUST fail the session
with `PROTOCOL_ERROR` if a mandatory record is missing or has the wrong
length. Odd types are reserved for optional extensions that future
bLIP revisions may define.

### Handshake and authentication

#### Constants

```
SERVICE_UUID = 7be78411-b151-48bf-a570-252b55847970 (16 bytes, RFC 4122 byte order)
PROLOGUE     = "nearby-payreq/1" ‖ SERVICE_UUID            (ASCII, 15 + 16 bytes)
COMMIT_TAG   = "nearby-payreq/1/commit"                    (ASCII)
CODE_TAG     = "nearby-payreq/1/code"                      (ASCII)
```

#### Noise

The handshake is `Noise_NN_25519_ChaChaPoly_SHA256` as specified in [the
Noise Protocol Framework][noise] (revision 34), with `PROLOGUE` as the
prologue:

* The payer is the initiator and the payee the responder. Both sides
  generate a fresh ephemeral key pair for every session. No static keys are
  used.
* Message 1 (`e`) carries an empty payload, so its body is exactly the
  payer's 32-byte ephemeral public key.
* Message 2 (`e, ee`) carries the TLV stream `{commitment}` as its payload,
  encrypted as the Noise framework specifies.
* `h` is the handshake hash after message 2 has been processed: the value
  that `GetHandshakeHash()` returns once the handshake is complete.
* After message 2, `Split()` produces two cipher states. The first encrypts
  payer → payee and the second payee → payer. Each `SEALED` body is
  `EncryptWithAd(empty, plaintext)` of the sending direction's cipher state,
  using that cipher state's implicit nonce counter, which starts at 0.
* A failed decryption MUST fail the session.

#### Commit/reveal

`Nb` (payee) and `Na` (payer) are 32-byte nonces, freshly drawn from a
cryptographically secure random number generator for every session.

```
C    = SHA-256(COMMIT_TAG ‖ Nb)
code = BE-uint32(SHA-256(CODE_TAG ‖ h ‖ Na ‖ Nb)[0..4]) mod 1000000,
       written as six decimal digits with leading zeros
```

### Session flow

```
   payer (central, initiator)                     payee (peripheral, responder)

1. NOISE_E      e                        ───────▶
2.                                       ◀─────── NOISE_E_EE  e, ee, TLV{0: C}
3. SEALED       TLV{2: Na}               ───────▶
                                                  payee computes and shows code
4.                                       ◀─────── SEALED  TLV{4: Nb, 6: payment_request}
   payer checks C, computes and shows code
5. SEALED       TLV{8: ack}              ───────▶
   payer disconnects
```

The payee:
  - MUST draw `Nb` and send `C` in message 2, before it has received `Na`.
  - On message 3, MUST compute `code`, and SHOULD display it straight away,
    replacing any code it displayed for an earlier session.
  - MUST send `Nb` and `payment_request` in message 4, and only after it
    received message 3.
  - On message 5 with `ack` = 0, SHOULD tell the user the request was
    delivered. On `ack` = 1, SHOULD treat the session as failed.
  - SHOULD warn its user if three or more sessions within 60 seconds failed
    after it displayed their code. That pattern is what an attacker
    grinding for a matching code looks like (see
    [Security considerations](#security-considerations)).

The payer:
  - MUST send message 1 as the first message on a connection.
  - MUST NOT draw or send `Na` before it has received and successfully
    processed message 2.
  - On message 4:
    - MUST check, in constant time, that `SHA-256(COMMIT_TAG ‖ Nb)` equals
      `C`. If it does not, the payer MUST send `ABORT(PROTOCOL_ERROR)`, fail
      the session and discard the payload.
    - MUST validate `payment_request` as described in
      [Payment request payload](#payment-request-payload). If it fails, the
      payer MUST send `ABORT(PAYLOAD_REJECTED)` and fail the session.
    - MUST compute `code`.
  - MUST send message 5 with `ack` = 0 if it can use the payment request, or
    `ack` = 1 if it parsed it and cannot use it (for example, an unknown
    format or the wrong network). The payer MAY send the `ack` before its
    user has compared the codes: `ack` = 0 means "received and usable", not
    "will pay".
  - MUST disconnect after sending message 5, and SHOULD stop scanning.

Either side:
  - MUST fail the session if it does not complete within a session timeout,
    measured from the payer's write of message 1 to the payee's receipt of
    message 5. 10 seconds is RECOMMENDED.
  - MUST fail the session, and SHOULD disconnect, on any `PROTOCOL_ERROR`.
    Where a notification or write is still possible, it SHOULD send
    `ABORT(PROTOCOL_ERROR)` first.

#### Comparison code

The payer:
  - MUST display `code` to its user together with the payment request it
    received, and MUST ask the user to confirm that the payee's screen shows
    the same code.
  - MUST NOT start a payment, or hand the payment request on to a step that
    can start one, before the user confirmed the codes match.
  - MUST discard the payment request if the user says the codes differ, and
    MUST NOT connect to that payee again in the current search.
  - SHOULD, if the user dismisses the confirmation without answering,
    discard the request and search again, without excluding that payee.

Both wallets SHOULD present the code in two groups of three digits
(`315 835`) and in a monospaced or tabular font. Each wallet SHOULD tell its
user what the code is for in plain words: the payer's user pays only if the
two codes are the same.

### ABORT

An `ABORT` body is a two-byte reason code. For `VERSION_UNSUPPORTED`, it is
followed by one byte: the highest message version the sender supports.

| Code | Name | Meaning |
|---|---|---|
| 1 | `PROTOCOL_ERROR` | malformed, unexpected or unauthenticated input |
| 2 | `VERSION_UNSUPPORTED` | the message `version` is not supported; carries the highest supported version |
| 3 | `BUSY` | the payee is serving another payer, or is rate limiting |
| 4 | `TIMEOUT` | a step took too long |
| 5 | `PAYLOAD_REJECTED` | the payer cannot use the payment request it received |
| 6 | `CANCELLED` | the sender stopped the session on purpose |

`ABORT` is neither encrypted nor authenticated: anyone in radio range can
inject one.

A receiver of an `ABORT`:
  - MUST end the session. An `ABORT` MUST NOT change what a session has
    already delivered or displayed, apart from ending it.
  - MUST NOT reply with an `ABORT`.
  - MUST treat an unknown reason code as a generic failure.

A payer receiving `ABORT(BUSY)` SHOULD move on to the next candidate payee and
MAY retry the same payee later. A payer receiving
`ABORT(VERSION_UNSUPPORTED)` MAY open a new session using the indicated
version if it supports that version.

### NFC carrier

A payee whose platform supports host card emulation MAY additionally serve
`payment_request` as an emulated NFC Forum Type 4 Tag.

The payee:
  - MUST emulate the NDEF Tag Application (AID `D2760000850101`) of the
    [NFC Forum Type 4 Tag specification][t4t], with mapping version 2.0 and
    the NDEF file marked read-only (write access `0xFF`).
  - MUST serve one NDEF message holding exactly one record: an NFC Forum
    well-known type `U` (URI) record whose URI is `payment_request`. The URI
    identifier code is chosen per the [URI Record Type Definition][urirtd];
    `0x00` (no abbreviation) applies to `lightning:` and `bitcoin:`.
  - MUST answer `SELECT` of the NDEF Tag Application with status `6A82` (file
    not found) whenever it is not displaying a payment request, so it never
    captures a tap meant for another application.
  - SHOULD serve the tag only while the device is unlocked.
  - MAY report to its user that the request was read once a reader has read
    the whole NDEF file.

A payer that reads an NDEF URI record from a tag MUST process it as described
in [Payment request payload](#payment-request-payload). A payer MAY skip the
comparison code for NFC: the user physically touching the payee's device
provides the proximity binding that the code provides for BLE.

A payer that supports both carriers SHOULD scan BLE and keep an NFC reader
open at the same time. A completed NFC read SHOULD cancel a BLE session that
is still in progress.

## Rationale

### Prior art

As far as the authors could find, no earlier proposal defines a BLE
payment-request exchange, or authenticates a request delivered over radio
against a nearby attacker without certificates. The closest work:

* **TBIP 74 / TBIP 75** ([Schroder and Schildbach, 2014][tbip74]) defined
  BIP 70 payment requests over classic Bluetooth RFCOMM. Two fixed service
  UUIDs carry varint-delimited protobuf messages, and the payee's Bluetooth
  address travels in the `bitcoin:` URI shown as a QR code. [Bitcoin
  Wallet][schildbach] for Android deployed it, along with BIP 70 requests and
  BIP 21 URIs over NFC. It was never assigned a BIP number and faded with
  BIP 70. The only authentication was BIP 70's X.509 merchant signature,
  which an ad-hoc payee does not have. TBIP 75's `h=` parameter, a hash of
  the request, pins it only when the payer has already scanned the QR code.
  This bLIP differs in four ways:
  * It uses BLE, which both major mobile platforms allow applications to
    use as a peripheral, instead of RFCOMM.
  * It carries any QR string instead of BIP 70.
  * It needs no QR code at all to find the payee.
  * It authenticates the request with a comparison code instead of a
    certificate.

  The `bt:` payment URL form used inside Bitcoin Wallet carries a classic
  Bluetooth MAC address and is not reused here.
* **NFC tag emulation.** [Numo][numo] (2026), a Cashu point-of-sale app,
  emulates an NFC Forum Type 4 tag through Android host card emulation.
  The tag holds a Cashu payment request, or a `lightning:` URI with a
  BOLT 11 invoice. Payers read it as NDEF and, for Cashu, write a token back
  to the tag. [NWC over NFC][nwcnfc] (draft, 2025) exchanges Nostr Wallet
  Connect requests over ISO-DEP with APDU chunking. The NFC carrier here
  matches the read-only half of the Numo convention: a single NDEF URI
  record holding the request string. A payer that already reads such tags
  needs no change.
* **Bolt Card** carries an LNURL-withdraw URI in an NDEF record on an
  NTAG 424 DNA card. It works in the opposite direction (the payee reads the
  payer's card) and authenticates the card with its own SUN/CMAC scheme. It
  is cited as the established NDEF URI convention for Lightning.

### An opaque payload

The transport carries exactly what the QR code would carry. The payer
therefore gains no new parsing surface, existing validation (network,
amount, expiry) applies unchanged, and a new payment format becomes
transferable the day a wallet supports it in its QR scanner. Defining a
structured, rail-specific payload would have tied the transport to today's
formats and duplicated logic that already exists in every wallet.

### GATT rather than advertising data or L2CAP

Payment requests are hundreds to several thousand bytes long. Legacy
advertising carries 31 bytes, extended advertising is not universally
available, and iOS does not let applications place service or manufacturer
data in advertisements at all, so the payload cannot ride in the
advertisement. L2CAP connection-oriented channels would avoid the chunk
layer, but GATT is the most widely and consistently supported way for
applications on both major mobile platforms to exchange data in either role.
A future version may define an L2CAP mapping.

The chunk layer exists only because ATT values are bounded by the MTU. It is
deliberately minimal: a START/END flag pair, a sequence byte to detect a lost
or duplicated value, and an explicit total length so the receiver never
buffers more than the sender declared.

Prepared writes are forbidden because platform behaviour for them differs
widely, and because some platform APIs report a maximum write length
(512 bytes) larger than `ATT_MTU − 3`, silently turning a large write into a
prepared write. Sizing chunks from the negotiated MTU avoids that path
entirely.

### No pairing

BLE pairing would bring OS-controlled prompts the wallet cannot style or
explain. It would leave bonds behind on both devices, and on some platforms
it cannot be driven by the application at all. More importantly, LE Secure
Connections numeric comparison shows its code in system UI, out of the
wallet's control and disconnected from the payment request it would
protect. Running authentication inside the application keeps the code next
to the request it vouches for.

### Noise NN with a commit/reveal comparison code

Neither device knows a key of the other in advance, so any key exchange
between them is unauthenticated on its own. Noise NN provides
confidentiality against passive listeners and a transcript hash `h` that
binds both ephemerals. A short code compared by a human provides
authentication.

A code derived from `h` alone would be weak: a man in the middle chooses its
own ephemerals and can grind them until the codes of its two sessions
collide, which takes about 2²⁰ attempts for a six-digit code. The
commit/reveal step removes that freedom. This is the construction behind
Bluetooth LE Secure Connections numeric comparison, analysed as SAS-based
authenticated key agreement by Vaudenay:

* **Attacker posing as the payee to the payer.** It must send `C` before it
  sees the payer's fresh `Na`, so the payer's code is uniformly random to it.
* **Attacker posing as the payer to the payee.** It must send `Na` before it
  learns the payee's `Nb`, which is hidden behind `C`, so the payee's code is
  uniformly random to it as well.

Each attempt therefore succeeds with probability 10⁻⁶, and no offline work
improves on that. The only remaining strategy is to repeat sessions against
the payee until the codes collide. The rate limit (30 sessions per minute)
pushes a 50 % chance of success out to about 16 days of uninterrupted
attempts. Every attempt also makes the payee's displayed code change and
counts towards the suspicious-activity warning.

Six digits match Bluetooth numeric comparison and are easy to compare at a
glance.

### No static keys

Static keys would add nothing without a way to know a peer in advance, and
they would give every session a stable identifier that could be tracked.

### Payer as central

The payee is the passive party, exactly like a displayed QR code. The payer
actively looks for a request when the user opens a send screen, as a QR
scanner would. Advertising is cheap for the payee while its receive screen is
visible. Scanning only runs while the payer is on a send screen.

### BOLT 1 TLV

Lightning implementers already have BigSize TLV codecs and the even/odd
extension rule. Optional fields, such as a payee display name or a payment
completion signal in a later version, can be added as odd types without a
version bump.

### Acknowledgement

The `ack` lets the payee tell its user the request reached the payer, and
lets a payer report a request it cannot use, such as a wrong network or an
unsupported format, so the payee does not wait in vain.

### A shared service UUID

A single UUID shared by every compliant wallet means an advertisement reveals
only that "some wallet nearby is showing a payment request", not which
application is in use. Per-wallet identifiers would split users into small,
distinguishable groups.

## Security considerations

| Threat | Mitigation | Residual risk |
|---|---|---|
| A nearby attacker advertises the service and serves its own payment request, possibly copying the real amount and description | The comparison code. "Codes differ" excludes that payee | A user who never compares the codes is not protected. Wallets should make the comparison an explicit step, not a passive banner. NFC and QR are unaffected. |
| Active man in the middle relaying between both devices | The commitment ordering caps success at 10⁻⁶ per attempt | none material |
| Grinding: repeated sessions against the payee until the codes collide | Rate limiting, a visibly changing code, the suspicious-activity warning | none material |
| Replay of an earlier session | Fresh ephemerals and nonces in every session; the code depends on both | none |
| Passive eavesdropping on the payment request (amount, node id, address) | ChaCha20-Poly1305 from message 2 onwards | The advertisement itself reveals that a payment request is being displayed nearby |
| Tracking through stable identifiers | No static keys, no local name, no manufacturer or service data, private addresses, advertising only while a request is displayed | The shared service UUID identifies the protocol, not the wallet |
| Pairing prompt abuse | No attribute requires security, so the OS never offers pairing | none |
| Malformed input (memory exhaustion, parser attacks) | Bounded message length (12288 bytes), bounded payload (8192 bytes), strict chunk and TLV rules, one payer at a time, session timeout | none material |
| A forged `ABORT` | `ABORT` can only end a session, never change what it delivered | An attacker in range can keep disrupting sessions; QR and NFC still work |
| Slot holding: an attacker connects and stalls | One payer at a time, the session timeout, rate limits | A persistent attacker in range can keep the BLE share busy; QR and NFC still work |
| NFC relay of the payee's tag to a distant payer | none needed: a relay delivers the payee's genuine request | none |
| The emulated tag capturing taps meant for other applications | `6A82` whenever no request is displayed | none |

Implementations SHOULD NOT log payment requests or comparison codes at log
levels enabled in production builds.

## Universality

This protocol runs between two wallets in physical proximity, at the
application layer. It does not touch the Lightning peer-to-peer protocol,
gossip, invoices or the node-to-node wire format, and no Lightning node
needs to understand it. Its payload also covers on-chain and other payment
formats. It is an optional convenience that only matters to wallets with a
user interface on a phone, so it is not suitable as a BOLT.

Because it is not Lightning-specific, publishing a matching document as a BIP
or a Bitcoin wallet standard may also be appropriate. This bLIP is offered
here because Lightning wallets are where the dense-QR problem is most acute
and where the TLV conventions it uses originate.

It defines no Lightning feature bits, message types or TLV types, so bLIP 2
needs no reservations. Its TLV namespace is private to its own messages.

## Backwards Compatibility

This is a new, optional protocol. A wallet that does not implement it keeps
working unchanged with QR codes, and a payer that does not find a compliant
payee falls back to the camera.

Future changes are accommodated as follows:

* optional fields as odd TLV types, without a version change;
* incompatible changes inside the message layer through the `version` byte
  and `ABORT(VERSION_UNSUPPORTED)`;
* a change to the chunk layer or GATT layout through a new service UUID.

## Reference Implementations

* [github.com/guggero/nearby-pay-req][impl]: a Go library with the chunk
  layer, message layer, TLV, sans-IO sessions, a Type 4 Tag emulator, and
  an orchestrator (peer selection, timeouts, rate limiting) behind a small
  radio interface that Android and iOS code implements. Its session tests
  reproduce every vector below.

The `gen/` directory next to this document holds an independent generator for
the test vectors below. It implements the TLV, framing, commitment, code and
chunking from this text and uses a Noise library only for the NN handshake.

## Test Vectors

Payloads in the vectors are placeholders, not valid invoices: the transport
never interprets them.

### Full exchange

Inputs:

```
payment_request         = "lightning:lnbc1" ‖ "q" × 60   (75 bytes)
payer ephemeral privkey = dabb28e976a234d060ab08975b94c4f255da77b6e19307040faf77b7989e6ff8
payee ephemeral privkey = ac0d693bfe8a89c768e860ba295115f7a6736b6c21ce2e39fed3d17572f65ca3
Na                      = ed4de10f911c0afe8576dca3fc5d6dcc87d630e1a48547f0b98a2f9d8031c303
Nb                      = b395331cf5250850b4010458e0c6cda13604cb9908579c5f9e2ee21698103f27
```

Intermediate values:

```
PROLOGUE                = 6e65617262792d7061797265712f317be78411b15148bfa570252b55847970
payer ephemeral pubkey  = fbcb4292b67b6b1f3ab65f66f5a98c9fddd137e128c5484d124ec054aa60d07e
payee ephemeral pubkey  = 0dc73519d59fce9ea4c41aac7ead51892ad26203c3d1c5f97e62f99be0d93235
C                       = 002f25f185171dc6801d15f4c59615092e09802699c6318c5122e1c56af27c09
h                       = 474cc501a595d09569ece7d74a261ee130384d8427e45aa8842b2905a0545779
code                    = 315835
```

Messages (header included, before chunking):

```
1 NOISE_E     (34 bytes)
0101fbcb4292b67b6b1f3ab65f66f5a98c9fddd137e128c5484d124ec054aa60d07e

2 NOISE_E_EE  (84 bytes)
01020dc73519d59fce9ea4c41aac7ead51892ad26203c3d1c5f97e62f99be0d932358f417cec326b
364f54ee44baf12f87a64f9342a1e415ad93a9194359564e6642d9630d757ff055aec1d7c5ef03c0
5cc79673

3 SEALED      (52 bytes)
010391194ac12980991bcdae1160c737f947e1ccf02ab2c1ad24d68fb0ddfd15e096b1f943fe35bb
80e395a2b63b8400499ebf5d

4 SEALED      (129 bytes)
0103eea60e8901a88deb08b909be6aa2180b39a991f02fde2d09ec8958b19080f7266a532297dced
7a096915b820ecd50211f51677e610d6c20380923ffa40008a16d819a47b375953826a901270ecde
ebdbbedfc2c5bbb56515d5bff4ab1c27440f389907e4b065f55a71ba6e60434753ad9770c1ebba35
dbd5acd78b31f4f195

5 SEALED      (21 bytes, ack = 0)
0103f22b86b0298c36571f62b4509e4870fbc4e361
```

Message 4 split at a chunk size of 20 bytes:

```
800000810103eea60e8901a88deb08b909be6aa2
0001180b39a991f02fde2d09ec8958b19080f726
00026a532297dced7a096915b820ecd50211f516
000377e610d6c20380923ffa40008a16d819a47b
0004375953826a901270ecdeebdbbedfc2c5bbb5
00056515d5bff4ab1c27440f389907e4b065f55a
000671ba6e60434753ad9770c1ebba35dbd5acd7
40078b31f4f195
```

### ABORT encodings

```
ABORT(PROTOCOL_ERROR)                     017f0001
ABORT(VERSION_UNSUPPORTED), max version 1 017f000201
ABORT(BUSY)                               017f0003
```

### More vectors

[`vectors.json`](vectors.json) holds the exchange above plus five more, with
every message and the chunking of message 4 at chunk sizes 20, 182 and 512:

| Name | Payload | Payload bytes | Message 4 bytes |
|---|---|---|---|
| `inline-short` | `lightning:` BOLT 11 | 75 | 129 |
| `bolt11-short` | `lightning:` BOLT 11 | 305 | 361 |
| `bolt11-route-hints` | `lightning:` BOLT 11, large | 3005 | 3061 |
| `bip21-unified` | BIP 21 with `lightning=` | 380 | 436 |
| `ark-bare` | bare Ark address | 125 | 179 |
| `bolt12-offer` | bare BOLT 12 offer | 204 | 258 |

[bcp14]: https://www.rfc-editor.org/info/bcp14
[bolt1]: https://github.com/lightning/bolts/blob/master/01-messaging.md#type-length-value-format
[noise]: https://noiseprotocol.org/noise.html
[t4t]: https://nfc-forum.org/build/specifications
[urirtd]: https://nfc-forum.org/build/specifications
[tbip74]: https://github.com/AndySchroder/bips/blob/master/tbip-0074.mediawiki
[schildbach]: https://github.com/bitcoin-wallet/bitcoin-wallet/wiki/Payment-Requests
[numo]: https://github.com/cashubtc/Numo
[nwcnfc]: https://github.com/agustinkassis/nwc-nfc
[impl]: https://github.com/guggero/nearby-pay-req
