package fakepbx

import (
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/emiago/sipgo/sip"
)

// Invite is the handle passed to OnInvite handlers.
// It controls each step of call setup.
type Invite struct {
	pbx        *FakePBX
	req        *sip.Request
	tx         sip.ServerTransaction
	responded  sync.Once
	cancelCh   chan struct{}
	cancelOnce sync.Once
}

// Request returns the original SIP INVITE request.
func (inv *Invite) Request() *sip.Request { return inv.req }

// From returns the From URI of the INVITE.
func (inv *Invite) From() string {
	if h := inv.req.From(); h != nil {
		return h.Address.String()
	}
	return ""
}

// To returns the To URI of the INVITE.
func (inv *Invite) To() string {
	if h := inv.req.To(); h != nil {
		return h.Address.String()
	}
	return ""
}

// SDP returns the SDP body from the INVITE, or nil.
func (inv *Invite) SDP() []byte { return inv.req.Body() }

// Trying sends 100 Trying.
func (inv *Invite) Trying() {
	res := sip.NewResponseFromRequest(inv.req, 100, "Trying", nil)
	inv.tx.Respond(res)
}

// Ringing sends 180 Ringing.
func (inv *Invite) Ringing() {
	res := sip.NewResponseFromRequest(inv.req, 180, "Ringing", nil)
	inv.tx.Respond(res)
}

// EarlyMedia sends 183 Session Progress with SDP body.
func (inv *Invite) EarlyMedia(sdp []byte) {
	res := sip.NewResponseFromRequest(inv.req, 183, "Session Progress", nil)
	res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	res.SetBody(sdp)
	inv.tx.Respond(res)
}

// Answer sends a 200 OK with SDP and returns an ActiveCall handle.
// Must be called at most once per Invite.
func (inv *Invite) Answer(sdp []byte) *ActiveCall {
	return inv.AnswerWithCode(200, sdp)
}

// AnswerWithCode sends a 2xx response with SDP. For non-200 success codes.
func (inv *Invite) AnswerWithCode(code int, sdp []byte) *ActiveCall {
	var ac *ActiveCall
	inv.responded.Do(func() {
		res := sip.NewResponseFromRequest(inv.req, code, "OK", nil)
		res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		// Build Contact with the PBX's actual host:port so in-dialog
		// requests (BYE, re-INVITE, etc.) route back correctly.
		contactURI := sip.Uri{Scheme: "sip", Host: "127.0.0.1"}
		if host, portStr, err := net.SplitHostPort(inv.pbx.addr); err == nil {
			contactURI.Host = host
			if p, err := strconv.Atoi(portStr); err == nil {
				contactURI.Port = p
			}
		}
		res.AppendHeader(&sip.ContactHeader{Address: contactURI})
		res.SetBody(sdp)
		inv.tx.Respond(res)

		// NOTE: We don't wait for ACK here. Per RFC 3261, the ACK for a 2xx
		// response is a separate transaction with a new Via branch, so it won't
		// arrive on tx.Acks(). The ACK is handled by srv.OnAck if needed.

		ac = &ActiveCall{
			pbx: inv.pbx,
			req: inv.req,
			res: res,
		}
	})
	return ac
}

// Respond sends a SIP response with the given status code, reason, and optional headers.
// For provisional responses (1xx), this can be called multiple times.
// For final responses (>=200), this is guarded by sync.Once — only the first
// final response (via Respond, Answer, or Reject) takes effect.
func (inv *Invite) Respond(code int, reason string, hdrs ...sip.Header) {
	send := func() {
		res := sip.NewResponseFromRequest(inv.req, code, reason, nil)
		for _, h := range hdrs {
			res.AppendHeader(h)
		}
		inv.tx.Respond(res)
	}

	if code < 200 {
		send()
	} else {
		inv.responded.Do(send)
	}
}

// Reject sends a non-2xx final response (e.g. 486, 503, 603).
func (inv *Invite) Reject(code int, reason string) {
	inv.responded.Do(func() {
		res := sip.NewResponseFromRequest(inv.req, code, reason, nil)
		inv.tx.Respond(res)
	})
}

// WaitForCancel blocks until a CANCEL is received or timeout elapses.
// Returns true if CANCEL arrived.
func (inv *Invite) WaitForCancel(timeout time.Duration) bool {
	select {
	case <-inv.cancelCh:
		return true
	case <-time.After(timeout):
		return false
	}
}
