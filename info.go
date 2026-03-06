package fakepbx

import (
	"sync"

	"github.com/emiago/sipgo/sip"
)

// Info is the handle passed to OnInfo handlers.
type Info struct {
	req       *sip.Request
	tx        sip.ServerTransaction
	responded sync.Once
}

// Request returns the original SIP INFO request.
func (i *Info) Request() *sip.Request { return i.req }

// Body returns the INFO body as raw bytes.
func (i *Info) Body() []byte { return i.req.Body() }

// Accept sends 200 OK.
func (i *Info) Accept() {
	i.responded.Do(func() {
		res := sip.NewResponseFromRequest(i.req, 200, "OK", nil)
		i.tx.Respond(res)
	})
}

// Reject sends a non-2xx final response.
func (i *Info) Reject(code int, reason string) {
	i.responded.Do(func() {
		res := sip.NewResponseFromRequest(i.req, code, reason, nil)
		i.tx.Respond(res)
	})
}
