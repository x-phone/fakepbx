// Package fakepbx provides an in-process SIP server for testing.
//
// It wraps [sipgo] to create a real SIP server bound to 127.0.0.1 on an
// ephemeral port. Tests get full programmatic control over SIP call flows
// — INVITE, REGISTER, BYE, CANCEL, REFER, OPTIONS, INFO, MESSAGE,
// SUBSCRIBE — without Docker,
// external processes, or hardcoded ports.
//
// FakePBX works as both UAS (receiving calls) and UAC (initiating calls):
//
//	pbx := fakepbx.NewFakePBX(t)
//
//	// UAS: handle incoming INVITEs from the device under test
//	pbx.OnInvite(func(inv *fakepbx.Invite) {
//	    inv.Trying()
//	    inv.Ringing()
//	    inv.Answer(fakepbx.SDP("127.0.0.1", 20000, fakepbx.PCMU))
//	})
//
//	// UAC: send an INVITE to the device under test
//	call, err := pbx.SendInvite(ctx, "sip:alice@127.0.0.1:5060",
//	    fakepbx.SDP("127.0.0.1", 20000, fakepbx.PCMU))
//
// The server is stopped automatically via [testing.TB.Cleanup].
//
// [sipgo]: https://github.com/emiago/sipgo
package fakepbx

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// FakePBX is an in-process SIP server for testing.
type FakePBX struct {
	t   testing.TB
	cfg config

	ua  *sipgo.UserAgent
	srv *sipgo.Server
	cli *sipgo.Client
	ctx context.Context

	addr string // "127.0.0.1:PORT"

	mu         sync.Mutex
	onRegister func(*Register)
	onInvite   func(*Invite)
	onBye      func(*Bye)
	onCancel   func(*Cancel)
	onAck      func(*Ack)
	onRefer     func(*Refer)
	onOptions   func(*Options)
	onInfo      func(*Info)
	onMessage   func(*Message)
	onSubscribe func(*Subscribe)

	recorded   map[sip.RequestMethod][]RecordedRequest
	authNonces map[string]bool // valid nonces for digest auth
}

type config struct {
	transport string
	username  string
	password  string
	userAgent string
}

// Option configures FakePBX.
type Option func(*config)

// WithTransport sets the SIP transport. Default: "udp".
func WithTransport(transport string) Option {
	return func(c *config) { c.transport = transport }
}

// WithAuth sets digest auth credentials the PBX expects.
// If not set, REGISTER is accepted unconditionally.
func WithAuth(username, password string) Option {
	return func(c *config) {
		c.username = username
		c.password = password
	}
}

// WithUserAgent sets the User-Agent header. Default: "FakePBX/test".
func WithUserAgent(ua string) Option {
	return func(c *config) { c.userAgent = ua }
}

// NewFakePBX creates and starts a FakePBX on 127.0.0.1:0 (UDP).
// The server is stopped automatically via t.Cleanup.
func NewFakePBX(t testing.TB, opts ...Option) *FakePBX {
	t.Helper()

	cfg := config{
		transport: "udp",
		userAgent: "FakePBX/test",
	}
	for _, o := range opts {
		o(&cfg)
	}

	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent(cfg.userAgent),
	)
	if err != nil {
		t.Fatalf("fakepbx: NewUA: %v", err)
	}

	srv, err := sipgo.NewServer(ua)
	if err != nil {
		ua.Close()
		t.Fatalf("fakepbx: NewServer: %v", err)
	}

	cli, err := sipgo.NewClient(ua)
	if err != nil {
		ua.Close()
		t.Fatalf("fakepbx: NewClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	pbx := &FakePBX{
		t:        t,
		cfg:      cfg,
		ua:       ua,
		srv:      srv,
		cli:      cli,
		ctx:      ctx,
		recorded:   make(map[sip.RequestMethod][]RecordedRequest),
		authNonces: make(map[string]bool),
	}

	// Register default handlers on sipgo server.
	pbx.registerSIPHandlers()

	// Start listening on ephemeral port.
	serverReady := make(chan struct{})
	go func() {
		readyFn := sipgo.ListenReadyFuncCtxValue(func(network, addr string) {
			pbx.addr = addr
			close(serverReady)
		})
		listenCtx := context.WithValue(ctx, sipgo.ListenReadyCtxKey, readyFn)
		if err := srv.ListenAndServe(listenCtx, cfg.transport, "127.0.0.1:0"); err != nil {
			if !errors.Is(err, net.ErrClosed) && !errors.Is(err, context.Canceled) {
				// Only log unexpected errors.
				t.Logf("fakepbx: server stopped: %v", err)
			}
		}
	}()

	// Wait for server to be ready.
	select {
	case <-serverReady:
	case <-time.After(5 * time.Second):
		cancel()
		ua.Close()
		t.Fatal("fakepbx: server did not start within 5s")
	}

	t.Cleanup(func() {
		cancel()
		ua.Close()
	})

	return pbx
}

// Addr returns the bound address ("127.0.0.1:PORT").
func (pbx *FakePBX) Addr() string {
	return pbx.addr
}

// URI returns a SIP URI for an extension: "sip:1002@127.0.0.1:PORT"
func (pbx *FakePBX) URI(extension string) string {
	return fmt.Sprintf("sip:%s@%s", extension, pbx.addr)
}

// SIPAddr returns the address in SIP format: "127.0.0.1:PORT;transport=udp"
func (pbx *FakePBX) SIPAddr() string {
	return pbx.addr + ";transport=" + pbx.cfg.transport
}

// OnRegister sets the handler for incoming REGISTER requests.
func (pbx *FakePBX) OnRegister(h func(*Register)) {
	pbx.mu.Lock()
	defer pbx.mu.Unlock()
	pbx.onRegister = h
}

// OnInvite sets the handler for incoming INVITE requests.
func (pbx *FakePBX) OnInvite(h func(*Invite)) {
	pbx.mu.Lock()
	defer pbx.mu.Unlock()
	pbx.onInvite = h
}

// OnBye sets the handler for incoming BYE requests.
func (pbx *FakePBX) OnBye(h func(*Bye)) {
	pbx.mu.Lock()
	defer pbx.mu.Unlock()
	pbx.onBye = h
}

// OnCancel sets the handler for incoming CANCEL requests.
func (pbx *FakePBX) OnCancel(h func(*Cancel)) {
	pbx.mu.Lock()
	defer pbx.mu.Unlock()
	pbx.onCancel = h
}

// OnAck sets the handler for incoming ACK requests.
func (pbx *FakePBX) OnAck(h func(*Ack)) {
	pbx.mu.Lock()
	defer pbx.mu.Unlock()
	pbx.onAck = h
}

// OnRefer sets the handler for incoming REFER requests.
func (pbx *FakePBX) OnRefer(h func(*Refer)) {
	pbx.mu.Lock()
	defer pbx.mu.Unlock()
	pbx.onRefer = h
}

// OnOptions sets the handler for incoming OPTIONS requests.
func (pbx *FakePBX) OnOptions(h func(*Options)) {
	pbx.mu.Lock()
	defer pbx.mu.Unlock()
	pbx.onOptions = h
}

// OnInfo sets the handler for incoming INFO requests.
func (pbx *FakePBX) OnInfo(h func(*Info)) {
	pbx.mu.Lock()
	defer pbx.mu.Unlock()
	pbx.onInfo = h
}

// OnMessage sets the handler for incoming MESSAGE requests.
func (pbx *FakePBX) OnMessage(h func(*Message)) {
	pbx.mu.Lock()
	defer pbx.mu.Unlock()
	pbx.onMessage = h
}

// OnSubscribe sets the handler for incoming SUBSCRIBE requests.
func (pbx *FakePBX) OnSubscribe(h func(*Subscribe)) {
	pbx.mu.Lock()
	defer pbx.mu.Unlock()
	pbx.onSubscribe = h
}

// AutoAnswer makes the PBX answer all INVITEs with: 100 -> 180 -> 200 OK + SDP.
func (pbx *FakePBX) AutoAnswer(sdp []byte) {
	pbx.OnInvite(func(inv *Invite) {
		inv.Trying()
		inv.Ringing()
		inv.Answer(sdp)
	})
}

// AutoBusy makes the PBX reject all INVITEs with: 100 -> 486 Busy Here.
func (pbx *FakePBX) AutoBusy() {
	pbx.OnInvite(func(inv *Invite) {
		inv.Trying()
		inv.Reject(486, "Busy Here")
	})
}

// AutoReject makes the PBX reject all INVITEs with the given code and reason.
func (pbx *FakePBX) AutoReject(code int, reason string) {
	pbx.OnInvite(func(inv *Invite) {
		inv.Trying()
		inv.Reject(code, reason)
	})
}

// SendInvite initiates an outbound call from the PBX to the given SIP URI.
// It sends an INVITE with the provided SDP, collects responses, and on 2xx
// sends ACK automatically. Returns an [OutboundCall] for mid-call control,
// or an error if the call was rejected or failed.
func (pbx *FakePBX) SendInvite(ctx context.Context, target string, sdp []byte) (*OutboundCall, error) {
	var uri sip.Uri
	if err := sip.ParseUri(target, &uri); err != nil {
		return nil, fmt.Errorf("fakepbx: invalid target URI %q: %w", target, err)
	}

	req := sip.NewRequest(sip.INVITE, uri)
	req.SetTransport(pbx.cfg.transport)

	// Contact: PBX's address so in-dialog requests route back to us.
	contactURI := pbx.contactURI()
	req.AppendHeader(&sip.ContactHeader{Address: contactURI})

	if sdp != nil {
		req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		req.SetBody(sdp)
	}

	tx, err := pbx.cli.TransactionRequest(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("fakepbx: INVITE transaction: %w", err)
	}

	for {
		select {
		case res := <-tx.Responses():
			if res.IsProvisional() {
				continue
			}
			if res.IsSuccess() {
				pbx.sendACK(req, res)
				tx.Terminate() // Free transaction resources immediately.
				call := &OutboundCall{dialogCall{
					pbx: pbx,
					req: req,
					res: res,
					dir: dirOutbound,
				}}
				// Initialize CSeq from the INVITE so subsequent requests increment from here.
				call.cseq.Store(req.CSeq().SeqNo)
				return call, nil
			}
			// Non-2xx final response — call rejected.
			tx.Terminate()
			return nil, fmt.Errorf("fakepbx: INVITE rejected: %d %s", res.StatusCode, res.Reason)
		case <-tx.Done():
			return nil, fmt.Errorf("fakepbx: INVITE transaction ended: %w", tx.Err())
		case <-ctx.Done():
			tx.Terminate()
			return nil, ctx.Err()
		}
	}
}

// SendMessage sends an out-of-dialog SIP MESSAGE to the given target URI.
func (pbx *FakePBX) SendMessage(ctx context.Context, target string, contentType string, body []byte) error {
	var uri sip.Uri
	if err := sip.ParseUri(target, &uri); err != nil {
		return fmt.Errorf("fakepbx: invalid target URI %q: %w", target, err)
	}

	req := sip.NewRequest(sip.MESSAGE, uri)
	req.SetTransport(pbx.cfg.transport)
	req.AppendHeader(&sip.ContactHeader{Address: pbx.contactURI()})
	if contentType != "" {
		req.AppendHeader(sip.NewHeader("Content-Type", contentType))
	}
	if body != nil {
		req.SetBody(body)
	}

	res, err := pbx.cli.Do(ctx, req)
	if err != nil {
		return fmt.Errorf("fakepbx: MESSAGE: %w", err)
	}
	if !res.IsSuccess() {
		return fmt.Errorf("fakepbx: MESSAGE rejected: %d %s", res.StatusCode, res.Reason)
	}
	return nil
}

// SendOptions sends an out-of-dialog SIP OPTIONS to the given target URI.
// Returns the response so callers can inspect capabilities (Allow, Supported, etc.).
func (pbx *FakePBX) SendOptions(ctx context.Context, target string) (*sip.Response, error) {
	var uri sip.Uri
	if err := sip.ParseUri(target, &uri); err != nil {
		return nil, fmt.Errorf("fakepbx: invalid target URI %q: %w", target, err)
	}

	req := sip.NewRequest(sip.OPTIONS, uri)
	req.SetTransport(pbx.cfg.transport)

	res, err := pbx.cli.Do(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("fakepbx: OPTIONS: %w", err)
	}
	return res, nil
}

// sendACK sends an ACK for a 2xx INVITE response (RFC 3261 §13.2.2.4).
func (pbx *FakePBX) sendACK(invReq *sip.Request, invRes *sip.Response) {
	ack := sip.NewRequest(sip.ACK, invReq.Recipient)
	ack.SipVersion = "SIP/2.0"
	ack.SetTransport(pbx.cfg.transport)

	if h := invReq.From(); h != nil {
		ack.AppendHeader(sip.HeaderClone(h))
	}
	if h := invRes.To(); h != nil {
		ack.AppendHeader(sip.HeaderClone(h))
	}
	if h := invReq.CallID(); h != nil {
		hdr := sip.CallIDHeader(*h)
		ack.AppendHeader(&hdr)
	}
	ack.AppendHeader(&sip.CSeqHeader{SeqNo: invReq.CSeq().SeqNo, MethodName: sip.ACK})
	maxfwd := sip.MaxForwardsHeader(70)
	ack.AppendHeader(&maxfwd)
	ack.SetBody(nil)

	if err := pbx.cli.WriteRequest(ack); err != nil {
		pbx.t.Logf("fakepbx: sendACK: %v", err)
	}
}

// contactURI returns a SIP URI for the PBX's listening address.
func (pbx *FakePBX) contactURI() sip.Uri {
	contactURI := sip.Uri{Scheme: "sip", Host: "127.0.0.1"}
	if host, portStr, err := net.SplitHostPort(pbx.addr); err == nil {
		contactURI.Host = host
		if p, err := strconv.Atoi(portStr); err == nil {
			contactURI.Port = p
		}
	}
	return contactURI
}

// record saves a request for later inspection.
func (pbx *FakePBX) record(method sip.RequestMethod, req *sip.Request) {
	pbx.mu.Lock()
	defer pbx.mu.Unlock()
	pbx.recorded[method] = append(pbx.recorded[method], RecordedRequest{
		Request:   req,
		Timestamp: time.Now(),
	})
}

// handleAuthRegister implements the default digest auth flow for REGISTER
// when WithAuth is configured. First request gets 401 with a challenge;
// subsequent requests with a valid Authorization header get 200 OK.
func (pbx *FakePBX) handleAuthRegister(reg *Register) {
	authHeader := reg.req.GetHeader("Authorization")
	if authHeader == nil {
		// No credentials — send challenge.
		nonce := pbx.generateNonce()
		reg.Challenge("fakepbx", nonce)
		return
	}

	// Parse and verify the digest response.
	val := authHeader.Value()
	username := extractDigestParam(val, "username")
	realm := extractDigestParam(val, "realm")
	nonce := extractDigestParam(val, "nonce")
	uri := extractDigestParam(val, "uri")
	response := extractDigestParam(val, "response")

	// Verify nonce is one we issued.
	pbx.mu.Lock()
	validNonce := pbx.authNonces[nonce]
	if validNonce {
		delete(pbx.authNonces, nonce) // single-use
	}
	pbx.mu.Unlock()

	if !validNonce {
		reg.Reject(403, "Forbidden")
		return
	}

	// Verify realm matches the one we issued in the challenge.
	if realm != "fakepbx" {
		reg.Reject(403, "Forbidden")
		return
	}

	// Compute expected digest response (RFC 2617).
	ha1 := fmt.Sprintf("%x", md5.Sum([]byte(fmt.Sprintf("%s:%s:%s", username, realm, pbx.cfg.password))))
	ha2 := fmt.Sprintf("%x", md5.Sum([]byte(fmt.Sprintf("REGISTER:%s", uri))))
	expected := fmt.Sprintf("%x", md5.Sum([]byte(fmt.Sprintf("%s:%s:%s", ha1, nonce, ha2))))

	if username == pbx.cfg.username && response == expected {
		reg.Accept()
	} else {
		reg.Reject(403, "Forbidden")
	}
}

// generateNonce creates a random nonce and stores it for later verification.
func (pbx *FakePBX) generateNonce() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		pbx.t.Fatalf("fakepbx: crypto/rand.Read failed: %v", err)
	}
	nonce := fmt.Sprintf("%x", b)
	pbx.mu.Lock()
	pbx.authNonces[nonce] = true
	pbx.mu.Unlock()
	return nonce
}

// extractDigestParam extracts a quoted parameter value from a Digest auth header.
func extractDigestParam(header, param string) string {
	prefix := param + `="`
	idx := strings.Index(header, prefix)
	if idx < 0 {
		return ""
	}
	start := idx + len(prefix)
	end := strings.Index(header[start:], `"`)
	if end < 0 {
		return ""
	}
	return header[start : start+end]
}

// registerSIPHandlers wires up sipgo server handlers for all supported methods.
//
// IMPORTANT: All user handlers are called synchronously (not in goroutines).
// sipgo's Server.handleRequest calls tx.TerminateGracefully() after the handler
// returns, which terminates the server transaction if no final response was sent.
// Running handlers synchronously ensures the transaction stays alive for the full
// duration of the handler (e.g., an INVITE handler waiting for CANCEL).
// Since sipgo dispatches each incoming request in its own goroutine, synchronous
// handlers do not block other requests.
func (pbx *FakePBX) registerSIPHandlers() {
	// REGISTER
	pbx.srv.OnRegister(func(req *sip.Request, tx sip.ServerTransaction) {
		pbx.record(sip.REGISTER, req)

		pbx.mu.Lock()
		handler := pbx.onRegister
		pbx.mu.Unlock()

		reg := &Register{req: req, tx: tx}
		if handler != nil {
			handler(reg)
		} else if pbx.cfg.username != "" {
			pbx.handleAuthRegister(reg)
		} else {
			reg.Accept()
		}
	})

	// INVITE
	pbx.srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		pbx.record(sip.INVITE, req)

		pbx.mu.Lock()
		handler := pbx.onInvite
		pbx.mu.Unlock()

		inv := &Invite{
			pbx:      pbx,
			req:      req,
			tx:       tx,
			cancelCh: make(chan struct{}),
		}

		// Wire up CANCEL detection on the INVITE transaction.
		// When sipgo's transaction layer matches a CANCEL to this INVITE tx,
		// it auto-responds 200 OK to the CANCEL and 487 to the INVITE,
		// then calls this callback.
		tx.OnCancel(func(cancelReq *sip.Request) {
			pbx.record(sip.CANCEL, cancelReq)
			inv.cancelOnce.Do(func() { close(inv.cancelCh) })

			pbx.mu.Lock()
			cancelHandler := pbx.onCancel
			pbx.mu.Unlock()
			if cancelHandler != nil {
				cancelHandler(&Cancel{req: cancelReq})
			}
		})

		if handler != nil {
			handler(inv)
		} else {
			// Default: auto-answer with default SDP.
			inv.Trying()
			inv.Answer(SDP("127.0.0.1", 20000, PCMU))
		}
	})

	// ACK
	pbx.srv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {
		pbx.record(sip.ACK, req)

		pbx.mu.Lock()
		handler := pbx.onAck
		pbx.mu.Unlock()

		if handler != nil {
			handler(&Ack{req: req})
		}
	})

	// BYE
	pbx.srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		pbx.record(sip.BYE, req)

		pbx.mu.Lock()
		handler := pbx.onBye
		pbx.mu.Unlock()

		bye := &Bye{req: req, tx: tx}
		if handler != nil {
			handler(bye)
		} else {
			bye.Accept()
		}
	})

	// CANCEL — this fires only for CANCELs that don't match an existing INVITE tx.
	// Matched CANCELs are handled by tx.OnCancel in the INVITE handler above.
	pbx.srv.OnCancel(func(req *sip.Request, tx sip.ServerTransaction) {
		pbx.record(sip.CANCEL, req)

		pbx.mu.Lock()
		handler := pbx.onCancel
		pbx.mu.Unlock()

		c := &Cancel{req: req}
		if handler != nil {
			handler(c)
		}

		res := sip.NewResponseFromRequest(req, 200, "OK", nil)
		tx.Respond(res)
	})

	// REFER
	pbx.srv.OnRefer(func(req *sip.Request, tx sip.ServerTransaction) {
		pbx.record(sip.REFER, req)

		pbx.mu.Lock()
		handler := pbx.onRefer
		pbx.mu.Unlock()

		ref := &Refer{req: req, tx: tx}
		if handler != nil {
			handler(ref)
		} else {
			ref.Accept()
		}
	})

	// OPTIONS
	pbx.srv.OnOptions(func(req *sip.Request, tx sip.ServerTransaction) {
		pbx.record(sip.OPTIONS, req)

		pbx.mu.Lock()
		handler := pbx.onOptions
		pbx.mu.Unlock()

		opt := &Options{req: req, tx: tx}
		if handler != nil {
			handler(opt)
		} else {
			opt.Accept()
		}
	})

	// INFO
	pbx.srv.OnInfo(func(req *sip.Request, tx sip.ServerTransaction) {
		pbx.record(sip.INFO, req)

		pbx.mu.Lock()
		handler := pbx.onInfo
		pbx.mu.Unlock()

		info := &Info{req: req, tx: tx}
		if handler != nil {
			handler(info)
		} else {
			info.Accept()
		}
	})

	// MESSAGE
	pbx.srv.OnMessage(func(req *sip.Request, tx sip.ServerTransaction) {
		pbx.record(sip.MESSAGE, req)

		pbx.mu.Lock()
		handler := pbx.onMessage
		pbx.mu.Unlock()

		msg := &Message{req: req, tx: tx}
		if handler != nil {
			handler(msg)
		} else {
			msg.Accept()
		}
	})

	// SUBSCRIBE — sipgo has no OnSubscribe, use generic OnRequest.
	pbx.srv.OnRequest(sip.SUBSCRIBE, func(req *sip.Request, tx sip.ServerTransaction) {
		pbx.record(sip.SUBSCRIBE, req)

		pbx.mu.Lock()
		handler := pbx.onSubscribe
		pbx.mu.Unlock()

		sub := &Subscribe{req: req, tx: tx}
		if handler != nil {
			handler(sub)
		} else {
			sub.Accept()
		}
	})
}
