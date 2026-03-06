package fakepbx

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/emiago/sipgo/sip"
)

// ActiveCall is returned by Invite.Answer().
// It lets tests simulate PBX-side mid-call actions.
type ActiveCall struct {
	pbx  *FakePBX
	req  *sip.Request  // original INVITE request
	res  *sip.Response // the 200 OK response sent
	cseq atomic.Uint32 // monotonically increasing CSeq for dialog requests
}

// SendBye makes the PBX hang up the call.
func (c *ActiveCall) SendBye(ctx context.Context) error {
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
func (c *ActiveCall) SendReInvite(ctx context.Context, sdp []byte) error {
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

// SendNotify sends a NOTIFY (e.g., after REFER to report transfer status).
func (c *ActiveCall) SendNotify(ctx context.Context, eventType, body string) error {
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
// For PBX-initiated requests, From/To are swapped relative to the original INVITE.
func (c *ActiveCall) newDialogRequest(method sip.RequestMethod) *sip.Request {
	// Target URI: use Contact from original INVITE if present, else From address.
	targetURI := c.contactURI()

	req := sip.NewRequest(method, targetURI)
	req.SetTransport(c.pbx.cfg.transport)

	// For PBX-initiated requests, the PBX (UAS) is the sender.
	// From = response To (PBX's identity, with PBX's tag)
	// To = request From (caller's identity, with caller's tag)
	if h := c.res.To(); h != nil {
		from := &sip.FromHeader{
			DisplayName: h.DisplayName,
			Address:     h.Address,
			Params:      h.Params,
		}
		req.AppendHeader(from)
	}
	if h := c.req.From(); h != nil {
		to := &sip.ToHeader{
			DisplayName: h.DisplayName,
			Address:     h.Address,
			Params:      h.Params,
		}
		req.AppendHeader(to)
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

// contactURI extracts the Contact URI from the original INVITE request.
func (c *ActiveCall) contactURI() sip.Uri {
	if contact := c.req.Contact(); contact != nil {
		return contact.Address
	}
	// Fallback to From address.
	return c.req.From().Address
}
