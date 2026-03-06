package fakepbx

import (
	"sync"

	"github.com/emiago/sipgo/sip"
)

// Message is the handle passed to OnMessage handlers.
type Message struct {
	req       *sip.Request
	tx        sip.ServerTransaction
	responded sync.Once
}

// Request returns the original SIP MESSAGE request.
func (m *Message) Request() *sip.Request { return m.req }

// Body returns the message body as raw bytes.
func (m *Message) Body() []byte { return m.req.Body() }

// Accept sends 200 OK.
func (m *Message) Accept() {
	m.responded.Do(func() {
		res := sip.NewResponseFromRequest(m.req, 200, "OK", nil)
		m.tx.Respond(res)
	})
}

// Reject sends a non-2xx final response.
func (m *Message) Reject(code int, reason string) {
	m.responded.Do(func() {
		res := sip.NewResponseFromRequest(m.req, code, reason, nil)
		m.tx.Respond(res)
	})
}
