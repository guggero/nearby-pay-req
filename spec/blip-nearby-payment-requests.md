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
    try the next candidate. A payee that answered `ABORT(BUSY)` is not
    failed: see [ABORT](#abort).
  - MAY collect the payment requests of several payees in one search, by
    running a session with each candidate in turn (strongest first) instead
    of stopping after the first, so its user can pick the request whose
    code matches the payee they want to pay. Such a payer MUST NOT run a
    session with a payee it already received a request from in the same
    search while that request's comparison lasts (see
    [Comparison code](#comparison-code)).
  - MUST NOT hold more than a small number of received requests whose
    comparison still lasts (4 is RECOMMENDED), counted across searches,
    so restarting a search does not reset the count. Every such request is
    a code an attacker may try to make a genuine payee show (see
    [Security considerations](#security-considerations)). Once at the
    limit, it MUST wait for the oldest comparison to end before running
    another session.
  - SHOULD bound the number of distinct payees it tracks in one search
    (64 is RECOMMENDED) and ignore new ones beyond that, rather than
    forgetting a payee that failed or did not match.

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
    payer's session is active, or while the payee holds the comparison of
    an earlier session (see [Session flow](#session-flow)), MUST be
    answered with `ABORT(BUSY)` (if a notification to it is possible) and
    disconnected. A connection whose first message is `CHOSEN` is not a
    session: it does not count towards this limit or the rate limits
    below, and SHOULD be answered even while another session is active.
  - MUST rate-limit new sessions. It SHOULD accept at most one new session
    per second and at most 30 per rolling 60 seconds, and SHOULD answer any
    session beyond that limit with `ABORT(BUSY)`.
  - MUST limit the sessions that reach their code (message 3): at most 6
    per rolling 60 seconds is RECOMMENDED, and at most 50 while it shares
    one request, after which it MUST stop serving that request over BLE.
    A session counts once it reached its code, whatever its outcome. The
    payee answers sessions beyond the per-minute limit with `ABORT(BUSY)`.
  - MUST drop a payer that has not sent its first complete message within a
    short time of enabling notifications (3 seconds is RECOMMENDED),
    however many fragments it sent, and SHOULD bound the number of payers
    connected without a session (8 is RECOMMENDED), so silent or trickling
    connections cannot hold resources indefinitely.

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

A sender MUST stop sending a message's chunks once the session timeout
(see [Session flow](#session-flow)) has passed, rather than finishing the
message: a peer that accepts every chunk slowly must not stretch a session
beyond its timeout.

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
| `0x04` | `CHOSEN` | both | payer → payee: a 32-byte `chosen_token`; payee → payer: empty, confirming it (see [Choosing a request](#choosing-a-request)) |
| `0x7f` | `ABORT` | both | plaintext abort reason (see [ABORT](#abort)) |

A receiver:
  - MUST answer a message whose `version` it does not support with
    `ABORT(VERSION_UNSUPPORTED)` and end the session.
  - MUST treat an unknown `type`, or a known type that is not the next one
    expected in [Session flow](#session-flow), as `PROTOCOL_ERROR`. As the
    only exception, a payee accepts `CHOSEN` as the first message on a
    connection (see [Choosing a request](#choosing-a-request)).

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
CHOSEN_TAG   = "nearby-payreq/1/chosen"                    (ASCII)
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
    replacing any code it displayed for an earlier session. Message 3 is
    where a session becomes a code attempt (see
    [Connection setup](#connection-setup)).
  - MUST send `Nb` and `payment_request` in message 4, and only after it
    received message 3.
  - On message 5 with `ack` = 0, SHOULD tell the user the request was
    delivered. On `ack` = 1, SHOULD treat the session as failed. A
    delivery does not mean the payer's user picked this request; that is
    signalled separately with `CHOSEN`. The payee SHOULD keep sharing
    after a delivery until it receives `CHOSEN` or its user stops.
  - After message 5 with `ack` = 0, MUST hold that session's comparison:
    keep displaying its code and answer every new session with
    `ABORT(BUSY)` until the comparison timeout passed (20 seconds is
    RECOMMENDED) or that session's payer sent `CHOSEN`, whichever comes
    first. Otherwise a second payer's session would replace the code the
    first payer's user is still comparing. The hold MUST end at the
    timeout whatever happens, so a session never reserves the payee
    longer.
  - SHOULD warn its user if four or more sessions within 60 seconds reached
    their code, whether they ended with `ack` = 0 or not. Sessions are
    counted regardless of the `ack`, because the `ack` is the payer's word
    and comes before any comparison: an attacker repeating sessions until
    a code matches would acknowledge every attempt. The warning is a
    heuristic; several honest payers in quick succession trigger it as
    well.

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
    "will pay". If message 5 cannot be sent, the payer MUST treat the
    session as failed and MUST NOT present the request: the payee did not
    count it as delivered and does not hold its code.
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
  - MUST NOT accept the user's confirmation once the comparison timeout
    (see [Session flow](#session-flow)), counted from receiving message 4,
    has passed: the payee may show another code by then, and every request
    that can still be confirmed is a code an attacker may aim for. The
    payer SHOULD search again instead.
  - SHOULD, if the user dismisses the confirmation without answering,
    discard the request and search again, without excluding that payee.

Both wallets SHOULD present the code in two groups of three digits
(`315 835`) and in a monospaced or tabular font. Each wallet SHOULD tell its
user what the code is for in plain words: the payer's user pays only if the
two codes are the same.

#### Choosing a request

When the payer's user has confirmed the codes match and picked a request,
the payer tells that payee, so the payee can stop offering the request to
other payers in range. Both sides derive a per-session token on message 3
(payee) and message 4 (payer):

```
chosen_token = SHA-256(CHOSEN_TAG ‖ h ‖ Na ‖ Nb)   (32 bytes)
```

`Na` and `Nb` only ever travel encrypted, so only the two ends of a session
can compute `chosen_token`. It is a different hash of the same inputs as
`code`, so neither reveals anything about the other.

The payer sends it on a new connection, after the session ended:

```
   payer                                          payee

1. CHOSEN       chosen_token (32 bytes)  ───────▶
2.                                       ◀─────── CHOSEN  (empty body)
   payer disconnects                              payee stops sharing
```

The payer:
  - SHOULD send `CHOSEN` once its user confirmed the codes match and picked
    the request. It MUST NOT send it for a request whose code the user did
    not confirm.
  - MUST send `CHOSEN` as the first and only message on a fresh connection,
    after enabling notifications as for a session, and MUST disconnect once
    it received the payee's answer or a timeout passed.
  - MAY retry a few times if the connection fails; it MUST NOT retry after
    `ABORT(UNKNOWN_SESSION)`.
  - MUST only count an empty `CHOSEN` with `version` 1 as the payee's
    confirmation.
  - MUST NOT make the payment depend on the payee's answer. `CHOSEN` is a
    courtesy to the payee, not part of authenticating the request.

The payee:
  - MUST remember `chosen_token` for every session that ended with
    `ack` = 0, for a retention time (2 minutes is RECOMMENDED), and MUST
    forget all of them when it stops sharing the request.
  - MUST forget a token once its retention passed, whether or not a
    `CHOSEN` arrives, and SHOULD bound the number of tokens it keeps.
  - On `CHOSEN` with a remembered token, MUST answer with an empty `CHOSEN`,
    MUST forget that token, MUST end the hold of that session's comparison
    if it still lasts, and MAY stop sharing the request (BLE and NFC) and
    tell its user which session chose it. A `CHOSEN` is the word of
    whoever completed that session, which may be an attacker's own
    session, not proof that the intended payer chose or paid: a payee
    SHOULD NOT treat it as a payment, and stopping the share on it is a
    convenience the payee's user can undo by sharing again.
  - On `CHOSEN` with any other token, or a body that is not 32 bytes, MUST
    answer `ABORT(UNKNOWN_SESSION)` or `ABORT(PROTOCOL_ERROR)` respectively
    and change nothing.
  - MUST disconnect after answering.

### ABORT

An `ABORT` body is a two-byte reason code. For `VERSION_UNSUPPORTED`, it is
followed by one byte: the highest message version the sender supports.
Bytes after the fields defined for a reason are reserved for future
extensions: a sender MUST NOT add any, and a receiver MUST ignore them.

| Code | Name | Meaning |
|---|---|---|
| 1 | `PROTOCOL_ERROR` | malformed, unexpected or unauthenticated input |
| 2 | `VERSION_UNSUPPORTED` | the message `version` is not supported; carries the highest supported version |
| 3 | `BUSY` | the payee is serving another payer, or is rate limiting |
| 4 | `TIMEOUT` | a step took too long |
| 5 | `PAYLOAD_REJECTED` | the payer cannot use the payment request it received |
| 6 | `CANCELLED` | the sender stopped the session on purpose |
| 7 | `UNKNOWN_SESSION` | the payee does not remember the `chosen_token` of a `CHOSEN` |

`ABORT` is neither encrypted nor authenticated: anyone in radio range can
inject one.

A receiver of an `ABORT`:
  - MUST end the session. An `ABORT` MUST NOT change what a session has
    already delivered or displayed, apart from ending it.
  - MUST NOT reply with an `ABORT`.
  - MUST treat an unknown reason code as a generic failure.

A payer receiving `ABORT(BUSY)` SHOULD move on to the next candidate payee and
SHOULD retry the same payee after a delay (2 seconds is RECOMMENDED), a
limited number of times (12 is RECOMMENDED, so the retries outlast a held
comparison). With several payers in one room this is what lets each of
them get the request in turn. A payer receiving
`ABORT(VERSION_UNSUPPORTED)` MAY open a new session using the indicated
version if it supports that version.

### Parameters

The numeric values this document RECOMMENDS are starting points for two
phones held close together. Radio conditions vary widely between devices
and rooms, so implementations MAY tune every one of them to their
environment, and SHOULD let applications do so. In one place:

| Parameter | Recommended | Where |
|---|---|---|
| Session timeout | 10 s | [Session flow](#session-flow) |
| First-message timeout | 3 s | [Connection setup](#connection-setup) |
| Payers connected without a session | 8 | [Connection setup](#connection-setup) |
| Comparison timeout (payee hold, payer confirmation) | 20 s | [Session flow](#session-flow), [Comparison code](#comparison-code) |
| Collection window | 800 ms | [Discovery](#discovery-and-peer-selection) |
| RSSI smoothing weight | 0.3 | [Discovery](#discovery-and-peer-selection) |
| Proximity floor | −75 dBm | [Discovery](#discovery-and-peer-selection) |
| Requests a payer may hold for confirmation | 4 | [Discovery](#discovery-and-peer-selection) |
| Payees tracked per search | 64 | [Discovery](#discovery-and-peer-selection) |
| Minimum gap between sessions | 1 s | [Connection setup](#connection-setup) |
| Sessions per rolling window | 30 per 60 s | [Connection setup](#connection-setup) |
| Code attempts per rolling window | 6 per 60 s | [Connection setup](#connection-setup) |
| Code attempts per shared request | 50 | [Connection setup](#connection-setup) |
| Suspicious code attempts | 4 per 60 s | [Session flow](#session-flow) |
| Busy retry delay, retries | 2 s, 12 | [ABORT](#abort) |
| `chosen_token` retention | 2 min | [Choosing a request](#choosing-a-request) |

A busy room (a shop counter, an event) typically wants a higher session cap
and a longer collection window. Any change to the code-attempt limits, the
number of requests a payer may hold or the comparison timeout changes the
bound on code grinding derived in [Rationale](#noise-nn-with-a-commitreveal-comparison-code);
the suspicious-activity threshold only changes when the user is warned.

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
    not found) whenever it is not displaying a payment request.
  - SHOULD take itself out of the platform's routing for that AID whenever
    it is not displaying a payment request (on Android: disable its host
    card emulation service component, or remove a dynamically registered
    AID), and make sure an abandoned share, a crash or a restart does not
    leave it registered. Answering `6A82` alone does not hand the tap to
    another application: platforms pick the service for an AID before any
    command reaches it.
  - SHOULD serve the tag only while the device is unlocked.
  - MAY report to its user that the request was read once a reader has read
    every byte of the NDEF file, in whatever order. A read of only part of
    the file, or at its end, is not a read of the request.

A payer that reads an NDEF URI record from a tag MUST process it as described
in [Payment request payload](#payment-request-payload). A payer MAY skip the
comparison code for NFC: the user physically touching the payee's device
provides the proximity binding that the code provides for BLE.

A payer that supports both carriers SHOULD scan BLE and keep an NFC reader
open at the same time. A completed NFC read SHOULD cancel a BLE session that
is still in progress.

A payee SHOULD start and stop serving the tag as NFC is switched on and off
while it shares, rather than deciding once when it starts.

## Rationale

### Choosing among several requests

Two payments can happen in one room at the same time. A payer that took
only the strongest payee would sometimes take the wrong one, and the user
would have to reject it and search again. Collecting the requests in range
and letting the user pick by code avoids that. It does weaken the code in
one respect: every collected request is a code the user may accept, so an
attacker who served several of them only needs the genuine payee to show
any one of those codes. That is why a payer may hold only a few requests
for confirmation at a time, each for a limited time, across searches (see
[the grinding bound](#noise-nn-with-a-commitreveal-comparison-code)).

A payee that keeps sharing after a delivery is what lets the second payer
in the room get the same request; `CHOSEN` is what lets it stop once the
right payer has it. Meanwhile the payee holds the delivered code on screen
and turns other payers away as busy, so a second payer cannot silently
replace the code the first payer's user is comparing; the hold ends with
that payer's `CHOSEN` or after the comparison timeout, and busy payers
retry. The hold costs a full session, which the code-attempt limits
bound, and always expires, so a payer, honest or not, never reserves a
payee for longer. A payer collecting from several payees holds each of
them for the comparison timeout at most.

`CHOSEN` travels on a new connection rather than keeping the session's
link open while the user compares codes, because an open link would hold
the payee's single session slot, and many phones handle several
simultaneous BLE links poorly. The token is derived rather than random, so
neither side has to send anything extra during the session, and it is only
known to the two ends because it depends on both encrypted nonces.

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

Each pairing of one payer session with one payee session therefore
matches with probability 10⁻⁶, and no offline work improves on that. What
remains is repetition: an attacker completes its own sessions with the
payer (posing as payees) and with the genuine payee (posing as a payer),
and wins when the genuine payee displays a code equal to one the payer's
user will accept for an attacker request. With `K` such codes acceptable
at once and `M` payee sessions that reach their code, the chance is about

```
P = 1 − (1 − K / 1,000,000)^M  ≈  K · M / 1,000,000
```

(treating the six-digit reduction as uniform; its modulo bias does not
change the result). The protocol bounds both factors instead of relying on
the attacker's patience:

* `K` is the number of requests a payer holds for confirmation: at most 4
  at a time across searches, each only for the comparison timeout. A payer
  that does not collect has `K` = 1.
* `M` is the number of payee sessions that reach their code. They are
  counted whatever their outcome, because an attacker would acknowledge
  every attempt; at most 6 per minute and 50 per shared request.

So against one shared request, `P` stays below 4 · 50 / 10⁶ = 0.02 %
(0.005 % for a payer that does not collect), however long the attacker
tries, and changing BLE identities changes neither bound. Matching codes
alone are not enough either: the user must also pick the attacker's
request. These figures are for the RECOMMENDED parameters; an application
that raises the limits raises the bound accordingly. The construction
itself has not been formally analysed beyond the SAS literature cited
above; review of the multi-session argument is welcome.

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
| Identity of the peer | none: Noise NN only protects the channel against passive listeners | The human comparison of the code is what authenticates the exchange the user picked |
| Active man in the middle relaying between both devices | The commitment ordering caps success at 10⁻⁶ per pair of sessions | See the next row |
| Grinding: repeated sessions until the payee shows a code the payer's user accepts | The payer holds at most 4 requests for confirmation, each for 20 s; the payee counts every session that reaches its code, at most 6 per minute and 50 per shared request; the warning | Below 0.02 % per shared request with the RECOMMENDED parameters (see [Rationale](#noise-nn-with-a-commitreveal-comparison-code)); the warning is a heuristic with false positives in busy rooms |
| A second payer replacing the code the first payer's user compares | The payee holds a delivered code for the comparison timeout and answers other payers `BUSY` | A held payee answers other payers `BUSY` for up to 20 s; an attacker can renew holds only within the code-attempt limits |
| Replay of an earlier session | Fresh ephemerals and nonces in every session; the code depends on both | none |
| Passive eavesdropping on the payment request (amount, node id, address) | ChaCha20-Poly1305 from message 2 onwards | The advertisement itself reveals that a payment request is being displayed nearby. Any nearby party can run its own payer session and read the request: encryption does not restrict who the audience is, exactly as anyone can photograph a displayed QR code |
| Proximity | RSSI-based payee selection | RSSI is a usability heuristic an attacker can influence with transmit power; it neither establishes distance nor rules out relays |
| Tracking through stable identifiers | No static keys, no local name, no manufacturer or service data, private addresses, advertising only while a request is displayed | The shared service UUID identifies the protocol, not the wallet |
| Pairing prompt abuse | Compliant attributes require no security, so a compliant payee never makes the OS offer pairing | A rogue peripheral advertising the service can require security on its own attributes, and some platforms then start pairing on their own; payers cannot fully prevent that prompt |
| Malformed input (memory exhaustion, parser attacks) | Bounded message length (12288 bytes), bounded payload (8192 bytes), strict chunk and TLV rules, one payer at a time, a session timeout enforced across every chunk, a first-message timeout, bounded payers without a session, tokens and tracked payees | Radio-level denial of service remains possible; on iOS a payee cannot force a payer's link closed and only stops serving it |
| A forged `CHOSEN`, to stop a payee from sharing | `chosen_token` depends on both nonces, which only travel encrypted, so only a payer that completed a session with the payee can produce it | An attacker that completes its own session as a payer can send `CHOSEN` and stop the share if the payee stops on it, as it could by holding the session slot; `CHOSEN` is no proof that the intended payer chose or paid. The payee's user can share again, and QR still works |
| Replay of an observed `CHOSEN` | The payee forgets a token on first use, and an eavesdropper only sees a token as it is used | none |
| A forged `ABORT` | `ABORT` can only end a session, never change what it delivered | An attacker in range can keep disrupting sessions; QR and NFC still work |
| Slot holding: an attacker connects and stalls | One payer at a time, the session timeout, rate limits | A persistent attacker in range can keep the BLE share busy; QR and NFC still work |
| NFC relay of the payee's tag to a distant payer | none needed: a relay delivers the payee's genuine request | none |
| An attacker's tag or emulator presented to the payer instead of the payee's | The user physically chooses which device to tap | Outside this protocol's protection, as with a QR code pasted over the genuine one |
| The emulated tag capturing taps meant for other applications | `6A82` whenever no request is displayed, and the HCE service removed from AID routing while not sharing | Platform routing behaviour (conflict resolution, the time a registration change takes) varies and needs testing per platform |

Implementations MUST NOT log payment requests or comparison codes at log
levels enabled in production builds, and MUST NOT log chosen tokens, keys
or nonces at all. A development-only level such as trace MAY carry
requests and codes for debugging; an application that ships with such a
level enabled leaks them, so it MUST keep that level off in production
builds rather than assume nobody enables it.

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
* new message types that only a peer opting in sends first, such as
  `CHOSEN`: a payee that predates them answers `ABORT(PROTOCOL_ERROR)`,
  which the sender treats as "not supported";
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
chosen_token            = 6a0a51f15e5026bf4f99bcb7d4026869e8e86bb01714d6bb79a48b1940616388
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

`CHOSEN` on a later connection, and the payee's confirmation:

```
CHOSEN        (34 bytes)
01046a0a51f15e5026bf4f99bcb7d4026869e8e86bb01714d6bb79a48b1940616388

CHOSEN        (2 bytes, confirmation)
0104
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
ABORT(UNKNOWN_SESSION)                    017f0007
```

### More vectors

[`vectors.json`](vectors.json) holds the exchange above plus five more, with
every message, the chunking of message 4 at chunk sizes 20, 182 and 512, and
the `chosen_token` with its `CHOSEN` messages:

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
