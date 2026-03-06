package fakepbx

import (
	"sync"

	"github.com/emiago/sipgo/sip"
)

// Options is the handle passed to OnOptions handlers.
type Options struct {
	req       *sip.Request
	tx        sip.ServerTransaction
	responded sync.Once
}

// Request returns the original SIP OPTIONS request.
func (o *Options) Request() *sip.Request { return o.req }

// Accept sends 200 OK.
func (o *Options) Accept() {
	o.responded.Do(func() {
		res := sip.NewResponseFromRequest(o.req, 200, "OK", nil)
		o.tx.Respond(res)
	})
}

// Reject sends a non-2xx final response.
func (o *Options) Reject(code int, reason string) {
	o.responded.Do(func() {
		res := sip.NewResponseFromRequest(o.req, code, reason, nil)
		o.tx.Respond(res)
	})
}
