package fakepbx

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/emiago/sipgo/sip"
)

// dialogDir indicates which side of the dialog the PBX is on.
type dialogDir int

const (
	dirInbound  dialogDir = iota // PBX was UAS (received the INVITE)
	dirOutbound                  // PBX was UAC (sent the INVITE)
)

// dialogCall holds the common state for an established SIP dialog.
// It is embedded by both [ActiveCall] and [OutboundCall].
type dialogCall struct {
	pbx  *FakePBX
	req  *sip.Request  // original INVITE (incoming or outgoing)
	res  *sip.Response // the 2xx response
	cseq atomic.Uint32 // monotonically increasing CSeq for dialog requests
	dir  dialogDir
}

// SendBye terminates the call.
func (c *dialogCall) SendBye(ctx context.Context) error {
	bye := c.newDialogRequest(sip.BYE)

	res, err := c.pbx.cli.Do(ctx, bye)
	if err != nil {
		return err
	}
	if res.StatusCode != 200 {
		return fmt.Errorf("BYE failed: %d %s", res.StatusCode, res.Reason)
	}
	return nil
}

// SendReInvite sends a re-INVITE with new SDP (hold, codec change, etc.).
func (c *dialogCall) SendReInvite(ctx context.Context, sdp []byte) error {
	invite := c.newDialogRequest(sip.INVITE)
	invite.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	invite.SetBody(sdp)

	res, err := c.pbx.cli.Do(ctx, invite)
	if err != nil {
		return err
	}
	if !res.IsSuccess() {
		return fmt.Errorf("re-INVITE failed: %d %s", res.StatusCode, res.Reason)
	}
	return nil
}

// SendNotify sends a NOTIFY within the dialog (e.g., transfer status after REFER).
func (c *dialogCall) SendNotify(ctx context.Context, eventType, body string) error {
	notify := c.newDialogRequest(sip.NOTIFY)
	notify.AppendHeader(sip.NewHeader("Event", eventType))
	notify.AppendHeader(sip.NewHeader("Subscription-State", "terminated"))
	if body != "" {
		notify.SetBody([]byte(body))
	}

	res, err := c.pbx.cli.Do(ctx, notify)
	if err != nil {
		return err
	}
	if !res.IsSuccess() {
		return fmt.Errorf("NOTIFY failed: %d %s", res.StatusCode, res.Reason)
	}
	return nil
}

// newDialogRequest builds a SIP request within the established dialog.
// The From/To orientation and target URI depend on the dialog direction.
func (c *dialogCall) newDialogRequest(method sip.RequestMethod) *sip.Request {
	req := sip.NewRequest(method, c.remoteContactURI())
	req.SetTransport(c.pbx.cfg.transport)

	// From/To depend on which side initiated the dialog.
	switch c.dir {
	case dirInbound:
		// PBX answered a call it received.
		// From = response To (PBX's identity, with PBX's tag)
		// To   = request From (caller's identity, with caller's tag)
		if h := c.res.To(); h != nil {
			req.AppendHeader(&sip.FromHeader{
				DisplayName: h.DisplayName, Address: h.Address, Params: h.Params,
			})
		}
		if h := c.req.From(); h != nil {
			req.AppendHeader(&sip.ToHeader{
				DisplayName: h.DisplayName, Address: h.Address, Params: h.Params,
			})
		}
	case dirOutbound:
		// PBX initiated the call.
		// From = request From (PBX's identity, with PBX's tag)
		// To   = response To (remote's identity, with remote's tag)
		if h := c.req.From(); h != nil {
			req.AppendHeader(&sip.FromHeader{
				DisplayName: h.DisplayName, Address: h.Address, Params: h.Params,
			})
		}
		if h := c.res.To(); h != nil {
			req.AppendHeader(&sip.ToHeader{
				DisplayName: h.DisplayName, Address: h.Address, Params: h.Params,
			})
		}
	}

	// Call-ID must match the dialog.
	if h := c.req.CallID(); h != nil {
		callID := sip.CallIDHeader(*h)
		req.AppendHeader(&callID)
	}

	// CSeq increments monotonically within the dialog (RFC 3261 §12.2.1.1).
	seq := c.cseq.Add(1)
	req.AppendHeader(&sip.CSeqHeader{SeqNo: seq, MethodName: method})

	maxfwd := sip.MaxForwardsHeader(70)
	req.AppendHeader(&maxfwd)

	return req
}

// remoteContactURI returns the remote party's Contact URI for routing in-dialog requests.
func (c *dialogCall) remoteContactURI() sip.Uri {
	switch c.dir {
	case dirInbound:
		// Remote = the caller who sent the INVITE. Their Contact is in the request.
		if contact := c.req.Contact(); contact != nil {
			return contact.Address
		}
		return c.req.From().Address
	case dirOutbound:
		// Remote = the callee who answered. Their Contact is in the 2xx response.
		if contact := c.res.Contact(); contact != nil {
			return contact.Address
		}
		return c.req.Recipient
	}
	return c.req.Recipient
}
