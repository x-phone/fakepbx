package fakepbx

import (
	"time"

	"github.com/emiago/sipgo/sip"
)

// RecordedRequest holds a captured SIP request with timestamp.
type RecordedRequest struct {
	Request   *sip.Request
	Timestamp time.Time
}

// Requests returns all recorded requests of the given SIP method.
func (pbx *FakePBX) Requests(method sip.RequestMethod) []RecordedRequest {
	pbx.mu.Lock()
	defer pbx.mu.Unlock()
	reqs := pbx.recorded[method]
	out := make([]RecordedRequest, len(reqs))
	copy(out, reqs)
	return out
}

// RegisterCount returns the number of REGISTER requests received.
func (pbx *FakePBX) RegisterCount() int { return pbx.countMethod(sip.REGISTER) }

// InviteCount returns the number of INVITE requests received.
func (pbx *FakePBX) InviteCount() int { return pbx.countMethod(sip.INVITE) }

// ByeCount returns the number of BYE requests received.
func (pbx *FakePBX) ByeCount() int { return pbx.countMethod(sip.BYE) }

// CancelCount returns the number of CANCEL requests received.
func (pbx *FakePBX) CancelCount() int { return pbx.countMethod(sip.CANCEL) }

// AckCount returns the number of ACK requests received.
func (pbx *FakePBX) AckCount() int { return pbx.countMethod(sip.ACK) }

// ReferCount returns the number of REFER requests received.
func (pbx *FakePBX) ReferCount() int { return pbx.countMethod(sip.REFER) }

// OptionsCount returns the number of OPTIONS requests received.
func (pbx *FakePBX) OptionsCount() int { return pbx.countMethod(sip.OPTIONS) }

// InfoCount returns the number of INFO requests received.
func (pbx *FakePBX) InfoCount() int { return pbx.countMethod(sip.INFO) }

// MessageCount returns the number of MESSAGE requests received.
func (pbx *FakePBX) MessageCount() int { return pbx.countMethod(sip.MESSAGE) }

// SubscribeCount returns the number of SUBSCRIBE requests received.
func (pbx *FakePBX) SubscribeCount() int { return pbx.countMethod(sip.SUBSCRIBE) }

// LastInvite returns the most recent INVITE request, or nil.
func (pbx *FakePBX) LastInvite() *sip.Request { return pbx.lastMethod(sip.INVITE) }

// LastRegister returns the most recent REGISTER request, or nil.
func (pbx *FakePBX) LastRegister() *sip.Request { return pbx.lastMethod(sip.REGISTER) }

func (pbx *FakePBX) countMethod(method sip.RequestMethod) int {
	pbx.mu.Lock()
	defer pbx.mu.Unlock()
	return len(pbx.recorded[method])
}

func (pbx *FakePBX) lastMethod(method sip.RequestMethod) *sip.Request {
	pbx.mu.Lock()
	defer pbx.mu.Unlock()
	reqs := pbx.recorded[method]
	if len(reqs) == 0 {
		return nil
	}
	return reqs[len(reqs)-1].Request
}

// WaitForRegister blocks until at least n REGISTERs are received or timeout elapses.
func (pbx *FakePBX) WaitForRegister(n int, timeout time.Duration) bool {
	return pbx.waitForMethod(sip.REGISTER, n, timeout)
}

// WaitForInvite blocks until at least n INVITEs are received or timeout elapses.
func (pbx *FakePBX) WaitForInvite(n int, timeout time.Duration) bool {
	return pbx.waitForMethod(sip.INVITE, n, timeout)
}

// WaitForBye blocks until at least n BYEs are received or timeout elapses.
func (pbx *FakePBX) WaitForBye(n int, timeout time.Duration) bool {
	return pbx.waitForMethod(sip.BYE, n, timeout)
}

// WaitForCancel blocks until at least n CANCELs are received or timeout elapses.
func (pbx *FakePBX) WaitForCancel(n int, timeout time.Duration) bool {
	return pbx.waitForMethod(sip.CANCEL, n, timeout)
}

// WaitForAck blocks until at least n ACKs are received or timeout elapses.
func (pbx *FakePBX) WaitForAck(n int, timeout time.Duration) bool {
	return pbx.waitForMethod(sip.ACK, n, timeout)
}

func (pbx *FakePBX) waitForMethod(method sip.RequestMethod, n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pbx.countMethod(method) >= n {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return pbx.countMethod(method) >= n
}
