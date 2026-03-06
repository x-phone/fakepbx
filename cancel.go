package fakepbx

import "github.com/emiago/sipgo/sip"

// Cancel is the handle passed to OnCancel handlers.
//
// Cancel handlers are notification-only: the SIP stack automatically sends
// 200 OK to the CANCEL and 487 Request Terminated to the original INVITE.
// The handler lets tests observe that cancellation occurred but cannot
// influence the response.
type Cancel struct {
	req *sip.Request
}

// Request returns the original SIP CANCEL request.
func (c *Cancel) Request() *sip.Request { return c.req }
