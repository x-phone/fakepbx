package fakepbx

import (
	"strings"
	"sync"

	"github.com/emiago/sipgo/sip"
)

// Refer is the handle passed to OnRefer handlers.
type Refer struct {
	req       *sip.Request
	tx        sip.ServerTransaction
	responded sync.Once
}

// Request returns the original SIP REFER request.
func (r *Refer) Request() *sip.Request { return r.req }

// ReferTo returns the value of the Refer-To header.
// Angle brackets are stripped if present.
func (r *Refer) ReferTo() string {
	if h := r.req.GetHeader("Refer-To"); h != nil {
		v := h.Value()
		v = strings.TrimPrefix(v, "<")
		v = strings.TrimSuffix(v, ">")
		return v
	}
	return ""
}

// Accept sends 202 Accepted.
func (r *Refer) Accept() {
	r.responded.Do(func() {
		res := sip.NewResponseFromRequest(r.req, 202, "Accepted", nil)
		r.tx.Respond(res)
	})
}

// Reject sends a non-2xx final response.
func (r *Refer) Reject(code int, reason string) {
	r.responded.Do(func() {
		res := sip.NewResponseFromRequest(r.req, code, reason, nil)
		r.tx.Respond(res)
	})
}
