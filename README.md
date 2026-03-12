# fakepbx

[![CI](https://github.com/x-phone/fakepbx/actions/workflows/ci.yml/badge.svg)](https://github.com/x-phone/fakepbx/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/x-phone/fakepbx.svg)](https://pkg.go.dev/github.com/x-phone/fakepbx)
[![Go Report Card](https://goreportcard.com/badge/github.com/x-phone/fakepbx)](https://goreportcard.com/report/github.com/x-phone/fakepbx)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Go Version](https://img.shields.io/github/go-mod/go-version/x-phone/fakepbx)](https://github.com/x-phone/fakepbx)
[![Latest Release](https://img.shields.io/github/v/release/x-phone/fakepbx)](https://github.com/x-phone/fakepbx/releases/latest)

In-process SIP server for Go tests. Real SIP over loopback — no Docker, no Asterisk, no hardcoded ports.

## Why?

If you're building a SIP client, softphone, or any VoIP application in Go, you need something to talk to during tests. The usual options are painful:

- **Spin up Asterisk/FreeSWITCH in Docker** — slow startup, complex config files, brittle networking, extra CI dependencies, and you still can't easily script "send 180 Ringing, wait 500ms, then 486 Busy Here."
- **Point at a shared staging PBX** — flaky, non-deterministic, can't run tests in parallel, breaks when someone else is using it.
- **Mock at the SIP library level** — fast, but you're not testing real SIP anymore. Your mocks drift from reality.

FakePBX gives you a **real SIP server** that lives inside your test process. It speaks actual SIP over UDP on loopback — your code sends and receives real SIP messages — but you control every response programmatically. No config files, no containers, no network setup.

```go
pbx := fakepbx.NewFakePBX(t) // real SIP server, ready in <1ms
```

Each test gets its own server on an OS-assigned port, torn down automatically via `t.Cleanup`. Tests run in parallel without conflicts. CI just needs `go test`.

## What can it do?

You script the PBX side of any SIP flow step by step. Wraps [sipgo](https://github.com/emiago/sipgo) under the hood.

## Install

```
go get github.com/x-phone/fakepbx
```

Requires Go 1.23+.

## Quick Start

```go
func TestMyUA_ReceivesCall(t *testing.T) {
    pbx := fakepbx.NewFakePBX(t) // starts on 127.0.0.1:<random>

    pbx.OnInvite(func(inv *fakepbx.Invite) {
        inv.Trying()
        inv.Ringing()
        inv.Answer(fakepbx.SDP("127.0.0.1", 20000, fakepbx.PCMU))
    })

    // Point your SIP UA at pbx.Addr() and dial pbx.URI("1002")
}
```

No handler? FakePBX auto-answers everything with 200 OK.

## Examples

### Busy Rejection

```go
pbx := fakepbx.NewFakePBX(t)
pbx.AutoBusy() // all INVITEs get 486 Busy Here
```

### Early Media

```go
pbx.OnInvite(func(inv *fakepbx.Invite) {
    inv.Trying()
    inv.EarlyMedia(fakepbx.SDP("127.0.0.1", 20000, fakepbx.PCMU)) // 183
    time.Sleep(100 * time.Millisecond)
    inv.Answer(fakepbx.SDP("127.0.0.1", 20000, fakepbx.PCMU))     // 200
})
```

### CANCEL Handling

```go
pbx.OnInvite(func(inv *fakepbx.Invite) {
    inv.Trying()
    inv.Ringing()
    // Block until caller cancels or timeout
    if inv.WaitForCancel(time.Second) {
        t.Log("caller cancelled")
    }
})
```

### PBX Hangs Up Mid-Call

```go
var call *fakepbx.ActiveCall
pbx.OnInvite(func(inv *fakepbx.Invite) {
    inv.Trying()
    call = inv.Answer(fakepbx.SDP("127.0.0.1", 20000, fakepbx.PCMU))
})

// ... establish the call, then:
call.SendBye(context.Background()) // PBX initiates hangup
```

### Outbound Call (PBX Calls Your UA)

```go
ctx := context.Background()
call, err := pbx.SendInvite(ctx, "sip:alice@127.0.0.1:5060",
    fakepbx.SDP("127.0.0.1", 20000, fakepbx.PCMU))
if err != nil {
    t.Fatal(err)
}
// call is an *OutboundCall — same mid-call methods as ActiveCall
defer call.SendBye(ctx)
```

### Re-INVITE (Hold)

```go
holdSDP := fakepbx.SDPWithDirection("127.0.0.1", 20000, "sendonly", fakepbx.PCMU)
call.SendReInvite(context.Background(), holdSDP)
```

### REFER (Call Transfer)

```go
// Receiving REFER from your UA:
pbx.OnRefer(func(ref *fakepbx.Refer) {
    fmt.Println("transfer to:", ref.ReferTo())
    ref.Accept() // 202 Accepted
})

// Sending REFER to your UA (within an established call):
call.SendRefer(ctx, "sip:bob@192.168.1.100")
```

### Send MESSAGE

```go
err := pbx.SendMessage(ctx, "sip:alice@127.0.0.1:5060",
    "text/plain", []byte("Hello from PBX"))
```

### Send OPTIONS (Health Check)

```go
res, err := pbx.SendOptions(ctx, "sip:alice@127.0.0.1:5060")
if err == nil {
    fmt.Println("Allow:", res.GetHeader("Allow").Value())
}
```

### Registration with Auth Challenge

```go
pbx.OnRegister(func(reg *fakepbx.Register) {
    if pbx.RegisterCount() <= 1 {
        reg.Challenge("fakepbx", "testnonce123") // 401
        return
    }
    reg.Accept() // 200
})
```

### Request Inspection

```go
// After running your test flow:
last := pbx.LastInvite()
byes := pbx.Requests(sip.BYE)

// Polling waiters (never hang — always time out):
pbx.WaitForInvite(1, time.Second)  // blocks until 1 INVITE arrives
pbx.WaitForBye(1, time.Second)     // blocks until 1 BYE arrives
```

### Parallel Tests

```go
func TestA(t *testing.T) {
    t.Parallel()
    pbx := fakepbx.NewFakePBX(t) // own port, no conflicts
    _ = pbx
}

func TestB(t *testing.T) {
    t.Parallel()
    pbx := fakepbx.NewFakePBX(t) // different port
    _ = pbx
}
```

## API Overview

| Type | Purpose |
|---|---|
| `FakePBX` | The SIP server. Create with `NewFakePBX(t)`. UAC: `SendInvite()`, `SendMessage()`, `SendOptions()` |
| `Invite` | Handle for INVITE. `Trying()`, `Ringing()`, `Answer()`, `Reject()`, `WaitForCancel()` |
| `ActiveCall` | Returned by `Answer()`. `SendBye()`, `SendReInvite()`, `SendRefer()`, `SendNotify()` |
| `OutboundCall` | Returned by `SendInvite()`. Same mid-call methods as `ActiveCall` |
| `Register` | Handle for REGISTER. `Accept()`, `Challenge()`, `Reject()` |
| `Bye` | Handle for BYE. `Accept()`, `Reject()` |
| `Cancel` | Handle for CANCEL (notification-only). `Request()` |
| `Refer` | Handle for REFER. `Accept()`, `Reject()`, `ReferTo()` |
| `Options` | Handle for OPTIONS. `Accept()`, `Reject()` |
| `Info` | Handle for INFO. `Accept()`, `Reject()`, `Body()` |
| `Message` | Handle for MESSAGE. `Accept()`, `Reject()`, `Body()` |
| `Subscribe` | Handle for SUBSCRIBE. `Accept()`, `Reject()`, `Event()` |
| `Ack` | Handle for ACK. `Request()`, `SDP()` |
| `SDP()` | Minimal SDP generator for test responses |
| `Codec` | RTP codec descriptor. Predefined: `PCMU`, `PCMA`, `G722` |

### Options

```go
fakepbx.NewFakePBX(t,
    fakepbx.WithTransport("udp"),       // default
    fakepbx.WithUserAgent("MyPBX/1.0"),
    fakepbx.WithAuth("user", "pass"),   // digest auth (RFC 2617)
)
```

### Convenience Presets

```go
pbx.AutoAnswer(sdp)             // 100 → 180 → 200 OK
pbx.AutoBusy()                  // 100 → 486 Busy Here
pbx.AutoReject(503, "Unavail")  // 100 → 503
```

## Default Behaviors

When no handler is registered:

| Request | Default Response |
|---|---|
| REGISTER | 200 OK |
| INVITE | 100 Trying → 200 OK + SDP |
| ACK | absorbed silently |
| BYE | 200 OK |
| CANCEL | 200 OK (+ 487 to INVITE) |
| REFER | 202 Accepted |
| OPTIONS | 200 OK |
| INFO | 200 OK |
| MESSAGE | 200 OK |
| SUBSCRIBE | 200 OK |

## License

[MIT](LICENSE)
