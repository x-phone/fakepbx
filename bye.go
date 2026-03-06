package fakepbx

import (
	"sync"

	"github.com/emiago/sipgo/sip"
)

// Bye is the handle passed to OnBye handlers.
type Bye struct {
	req       *sip.Request
	tx        sip.ServerTransaction
	responded sync.Once
}

// Request returns the original SIP BYE request.
func (b *Bye) Request() *sip.Request { return b.req }

// Accept sends 200 OK.
func (b *Bye) Accept() {
	b.responded.Do(func() {
		res := sip.NewResponseFromRequest(b.req, 200, "OK", nil)
		b.tx.Respond(res)
	})
}

// Reject sends a non-2xx final response (e.g. 481 Call/Transaction Does Not Exist).
func (b *Bye) Reject(code int, reason string) {
	b.responded.Do(func() {
		res := sip.NewResponseFromRequest(b.req, code, reason, nil)
		b.tx.Respond(res)
	})
}
