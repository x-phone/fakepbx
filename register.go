package fakepbx

import (
	"fmt"
	"sync"

	"github.com/emiago/sipgo/sip"
)

// Register is the handle passed to OnRegister handlers.
type Register struct {
	req       *sip.Request
	tx        sip.ServerTransaction
	responded sync.Once
}

// Request returns the original SIP REGISTER request.
func (r *Register) Request() *sip.Request { return r.req }

// Accept sends 200 OK.
func (r *Register) Accept() {
	r.responded.Do(func() {
		res := sip.NewResponseFromRequest(r.req, 200, "OK", nil)
		r.tx.Respond(res)
	})
}

// Challenge sends 401 Unauthorized with a WWW-Authenticate header.
func (r *Register) Challenge(realm, nonce string) {
	r.responded.Do(func() {
		res := sip.NewResponseFromRequest(r.req, 401, "Unauthorized", nil)
		wwwAuth := fmt.Sprintf(`Digest realm="%s", nonce="%s", algorithm=MD5`, realm, nonce)
		res.AppendHeader(sip.NewHeader("WWW-Authenticate", wwwAuth))
		r.tx.Respond(res)
	})
}

// Reject sends a non-2xx final response.
func (r *Register) Reject(code int, reason string) {
	r.responded.Do(func() {
		res := sip.NewResponseFromRequest(r.req, code, reason, nil)
		r.tx.Respond(res)
	})
}
