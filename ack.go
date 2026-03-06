package fakepbx

import "github.com/emiago/sipgo/sip"

// Ack is the handle passed to OnAck handlers.
type Ack struct {
	req *sip.Request
}

// Request returns the original SIP ACK request.
func (a *Ack) Request() *sip.Request { return a.req }

// SDP returns the SDP body from the ACK, or nil.
func (a *Ack) SDP() []byte { return a.req.Body() }
