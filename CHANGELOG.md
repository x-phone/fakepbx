# Changelog

## Unreleased

### Added

- **Outbound INVITE (UAC)**: `FakePBX.SendInvite()` initiates calls to the device under test, returning an `OutboundCall` for mid-call control.
- **In-dialog REFER**: `SendRefer()` on both `ActiveCall` and `OutboundCall` for triggering call transfers within established dialogs.
- **Out-of-dialog MESSAGE**: `FakePBX.SendMessage()` sends SIP MESSAGE requests (text, JSON, etc.).
- **Out-of-dialog OPTIONS**: `FakePBX.SendOptions()` sends SIP OPTIONS for health checks and capability discovery.
- **`OutboundCall` type**: returned by `SendInvite()`, shares all mid-call methods (`SendBye`, `SendReInvite`, `SendRefer`, `SendNotify`) with `ActiveCall` via the shared `dialogCall` base.

### Changed

- **`dialogCall` shared base**: extracted from `ActiveCall` to eliminate duplication. Both `ActiveCall` (inbound/UAS) and `OutboundCall` (outbound/UAC) embed it. Direction-aware From/To orientation per RFC 3261.
- **Test helper `testUAS`**: split into `testUAS()` (creation) + `start(t)` (listening) to fix a data race with sipgo's internal handler map.

## v0.1.0

Initial release of fakepbx — in-process SIP server for Go tests.
