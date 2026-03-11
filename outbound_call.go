package fakepbx

import "github.com/emiago/sipgo/sip"

// OutboundCall represents a call initiated by the PBX (UAC side).
// It is returned by [FakePBX.SendInvite] when the remote answers with 2xx.
type OutboundCall struct {
	dialogCall
}

// Request returns the INVITE request that was sent.
func (c *OutboundCall) Request() *sip.Request { return c.req }

// Response returns the 2xx response received from the remote.
func (c *OutboundCall) Response() *sip.Response { return c.res }
