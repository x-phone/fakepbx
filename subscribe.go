package fakepbx

import (
	"sync"

	"github.com/emiago/sipgo/sip"
)

// Subscribe is the handle passed to OnSubscribe handlers.
type Subscribe struct {
	req       *sip.Request
	tx        sip.ServerTransaction
	responded sync.Once
}

// Request returns the original SIP SUBSCRIBE request.
func (s *Subscribe) Request() *sip.Request { return s.req }

// Event returns the value of the Event header.
func (s *Subscribe) Event() string {
	if h := s.req.GetHeader("Event"); h != nil {
		return h.Value()
	}
	return ""
}

// Accept sends 200 OK.
func (s *Subscribe) Accept() {
	s.responded.Do(func() {
		res := sip.NewResponseFromRequest(s.req, 200, "OK", nil)
		s.tx.Respond(res)
	})
}

// Reject sends a non-2xx final response.
func (s *Subscribe) Reject(code int, reason string) {
	s.responded.Do(func() {
		res := sip.NewResponseFromRequest(s.req, code, reason, nil)
		s.tx.Respond(res)
	})
}
