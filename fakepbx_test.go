package fakepbx

import (
	"context"
	"crypto/md5"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// =============================================================================
// Test Client Helpers
// =============================================================================

// testUAC creates a sipgo User Agent + Client for sending SIP requests to a FakePBX.
// The UA and client are cleaned up automatically.
func testUAC(t *testing.T) (*sipgo.Client, *sipgo.UserAgent) {
	t.Helper()
	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent("TestUAC"),
	)
	if err != nil {
		t.Fatalf("testUAC: NewUA: %v", err)
	}
	t.Cleanup(func() { ua.Close() })

	cli, err := sipgo.NewClient(ua,
		sipgo.WithClientHostname("127.0.0.1"),
	)
	if err != nil {
		t.Fatalf("testUAC: NewClient: %v", err)
	}

	return cli, ua
}

// sendRegister sends a REGISTER to the given address and returns the final response.
func sendRegister(t *testing.T, cli *sipgo.Client, addr string) *sip.Response {
	t.Helper()
	host, port, err := sip.ParseAddr(addr)
	if err != nil {
		t.Fatalf("sendRegister: ParseAddr(%q): %v", addr, err)
	}

	req := sip.NewRequest(sip.REGISTER, sip.Uri{
		Scheme: "sip",
		Host:   host,
		Port:   port,
	})
	req.SetTransport("UDP")
	req.AppendHeader(&sip.ContactHeader{
		Address: sip.Uri{Scheme: "sip", User: "test", Host: "127.0.0.1"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res, err := cli.Do(ctx, req)
	if err != nil {
		t.Fatalf("sendRegister: Do: %v", err)
	}
	return res
}

// sendOptions sends an OPTIONS request and returns the final response.
func sendOptions(t *testing.T, cli *sipgo.Client, addr string) *sip.Response {
	t.Helper()
	host, port, err := sip.ParseAddr(addr)
	if err != nil {
		t.Fatalf("sendOptions: ParseAddr(%q): %v", addr, err)
	}

	req := sip.NewRequest(sip.OPTIONS, sip.Uri{
		Scheme: "sip",
		Host:   host,
		Port:   port,
	})
	req.SetTransport("UDP")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res, err := cli.Do(ctx, req)
	if err != nil {
		t.Fatalf("sendOptions: Do: %v", err)
	}
	return res
}

// inviteResult holds the collected responses from an INVITE transaction.
type inviteResult struct {
	Provisionals []*sip.Response
	Final        *sip.Response
	TX           sip.ClientTransaction
	Req          *sip.Request
}

// sendInvite sends an INVITE to the given URI with optional SDP body.
// It collects all provisional responses and the final response.
func sendInvite(t *testing.T, cli *sipgo.Client, targetURI string, sdpBody []byte) inviteResult {
	t.Helper()

	var uri sip.Uri
	if err := sip.ParseUri(targetURI, &uri); err != nil {
		t.Fatalf("sendInvite: ParseUri(%q): %v", targetURI, err)
	}

	req := sip.NewRequest(sip.INVITE, uri)
	req.SetTransport("UDP")
	if sdpBody != nil {
		req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		req.SetBody(sdpBody)
	}
	req.AppendHeader(&sip.ContactHeader{
		Address: sip.Uri{Scheme: "sip", User: "testcaller", Host: "127.0.0.1"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tx, err := cli.TransactionRequest(ctx, req)
	if err != nil {
		t.Fatalf("sendInvite: TransactionRequest: %v", err)
	}

	result := inviteResult{TX: tx, Req: req}
	for {
		select {
		case res := <-tx.Responses():
			if res.IsProvisional() {
				result.Provisionals = append(result.Provisionals, res)
				continue
			}
			result.Final = res
			return result
		case <-tx.Done():
			if result.Final == nil {
				t.Fatalf("sendInvite: transaction ended without final response: %v", tx.Err())
			}
			return result
		case <-ctx.Done():
			t.Fatal("sendInvite: timeout waiting for INVITE response")
			return result
		}
	}
}

// sendACK sends an ACK for a 2xx INVITE response.
func sendACK(t *testing.T, cli *sipgo.Client, invReq *sip.Request, invRes *sip.Response) {
	t.Helper()

	ack := sip.NewRequest(sip.ACK, invReq.Recipient)
	ack.SipVersion = "SIP/2.0"
	ack.SetTransport("UDP")

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

	if err := cli.WriteRequest(ack); err != nil {
		t.Fatalf("sendACK: WriteRequest: %v", err)
	}
}

// sendBye sends a BYE within a dialog established by invReq/invRes.
func sendBye(t *testing.T, cli *sipgo.Client, invReq *sip.Request, invRes *sip.Response) *sip.Response {
	t.Helper()

	bye := sip.NewRequest(sip.BYE, invReq.Recipient)
	bye.SetTransport("UDP")

	if h := invReq.From(); h != nil {
		bye.AppendHeader(sip.HeaderClone(h))
	}
	if h := invRes.To(); h != nil {
		bye.AppendHeader(sip.HeaderClone(h))
	}
	if h := invReq.CallID(); h != nil {
		hdr := sip.CallIDHeader(*h)
		bye.AppendHeader(&hdr)
	}
	bye.AppendHeader(&sip.CSeqHeader{SeqNo: invReq.CSeq().SeqNo + 1, MethodName: sip.BYE})
	bye.SetBody(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res, err := cli.Do(ctx, bye)
	if err != nil {
		t.Fatalf("sendBye: Do: %v", err)
	}
	return res
}

// sendRefer sends a standalone REFER to the given address.
func sendRefer(t *testing.T, cli *sipgo.Client, addr, referTo string) *sip.Response {
	t.Helper()
	host, port, err := sip.ParseAddr(addr)
	if err != nil {
		t.Fatalf("sendRefer: ParseAddr(%q): %v", addr, err)
	}

	req := sip.NewRequest(sip.REFER, sip.Uri{
		Scheme: "sip",
		Host:   host,
		Port:   port,
	})
	req.SetTransport("UDP")
	req.AppendHeader(sip.NewHeader("Refer-To", referTo))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res, err := cli.Do(ctx, req)
	if err != nil {
		t.Fatalf("sendRefer: Do: %v", err)
	}
	return res
}

// =============================================================================
// Phase 1e: SDP Helper Tests
// =============================================================================

func TestCodecConstants(t *testing.T) {
	t.Parallel()

	if PCMU.PayloadType != 0 || PCMU.Name != "PCMU" || PCMU.ClockRate != 8000 {
		t.Errorf("PCMU = %+v, want {0, PCMU, 8000}", PCMU)
	}
	if PCMA.PayloadType != 8 || PCMA.Name != "PCMA" || PCMA.ClockRate != 8000 {
		t.Errorf("PCMA = %+v, want {8, PCMA, 8000}", PCMA)
	}
	if G722.PayloadType != 9 || G722.Name != "G722" || G722.ClockRate != 8000 {
		t.Errorf("G722 = %+v, want {9, G722, 8000}", G722)
	}
}

func TestSDP_SingleCodec(t *testing.T) {
	t.Parallel()
	sdp := string(SDP("127.0.0.1", 20000, PCMU))
	assertSDPLine(t, sdp, "v=0")
	assertSDPContains(t, sdp, "o=", "IN IP4 127.0.0.1")
	assertSDPContains(t, sdp, "c=", "IN IP4 127.0.0.1")
	assertSDPLine(t, sdp, "t=0 0")
	assertSDPContains(t, sdp, "m=audio", "20000 RTP/AVP 0")
	assertSDPLine(t, sdp, "a=rtpmap:0 PCMU/8000")
	if strings.Contains(sdp, "\n") && !strings.Contains(sdp, "\r\n") {
		t.Error("SDP must use \\r\\n line endings")
	}
}

func TestSDP_MultipleCodecs(t *testing.T) {
	t.Parallel()
	sdp := string(SDP("127.0.0.1", 20000, PCMU, PCMA, G722))
	assertSDPContains(t, sdp, "m=audio", "20000 RTP/AVP 0 8 9")
	assertSDPLine(t, sdp, "a=rtpmap:0 PCMU/8000")
	assertSDPLine(t, sdp, "a=rtpmap:8 PCMA/8000")
	assertSDPLine(t, sdp, "a=rtpmap:9 G722/8000")
}

func TestSDP_DefaultCodecs(t *testing.T) {
	t.Parallel()
	sdp := string(SDP("127.0.0.1", 20000))
	assertSDPContains(t, sdp, "m=audio", "20000 RTP/AVP 0")
	assertSDPLine(t, sdp, "a=rtpmap:0 PCMU/8000")
}

func TestSDPWithDirection_Sendonly(t *testing.T) {
	t.Parallel()
	sdp := string(SDPWithDirection("127.0.0.1", 20000, "sendonly", PCMU))
	assertSDPLine(t, sdp, "a=sendonly")
	assertSDPContains(t, sdp, "m=audio", "20000 RTP/AVP 0")
}

func TestSDPWithDirection_Recvonly(t *testing.T) {
	t.Parallel()
	sdp := string(SDPWithDirection("127.0.0.1", 20000, "recvonly", PCMU))
	assertSDPLine(t, sdp, "a=recvonly")
}

func TestSDPWithDirection_Sendrecv(t *testing.T) {
	t.Parallel()
	sdp := string(SDPWithDirection("127.0.0.1", 20000, "sendrecv", PCMU))
	assertSDPLine(t, sdp, "a=sendrecv")
}

// =============================================================================
// Phase 1a: Skeleton + Server Lifecycle Tests
// =============================================================================

func TestNewFakePBX_Starts(t *testing.T) {
	pbx := NewFakePBX(t)
	if pbx == nil {
		t.Fatal("NewFakePBX returned nil")
	}
}

func TestNewFakePBX_Addr(t *testing.T) {
	pbx := NewFakePBX(t)

	addr := pbx.Addr()
	if addr == "" {
		t.Fatal("Addr() returned empty string")
	}

	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatalf("Addr() %q is not a valid UDP address: %v", addr, err)
	}
	if udpAddr.Port == 0 {
		t.Fatal("Addr() has port 0, expected ephemeral port")
	}
	if udpAddr.IP.String() != "127.0.0.1" {
		t.Fatalf("Addr() IP = %s, want 127.0.0.1", udpAddr.IP)
	}
}

func TestNewFakePBX_URI(t *testing.T) {
	pbx := NewFakePBX(t)

	uri := pbx.URI("1002")
	want := fmt.Sprintf("sip:1002@%s", pbx.Addr())
	if uri != want {
		t.Errorf("URI(\"1002\") = %q, want %q", uri, want)
	}

	uri2 := pbx.URI("alice")
	want2 := fmt.Sprintf("sip:alice@%s", pbx.Addr())
	if uri2 != want2 {
		t.Errorf("URI(\"alice\") = %q, want %q", uri2, want2)
	}
}

func TestNewFakePBX_SIPAddr(t *testing.T) {
	pbx := NewFakePBX(t)

	sipAddr := pbx.SIPAddr()
	want := pbx.Addr() + ";transport=udp"
	if sipAddr != want {
		t.Errorf("SIPAddr() = %q, want %q", sipAddr, want)
	}
}

func TestNewFakePBX_Parallel(t *testing.T) {
	pbx1 := NewFakePBX(t)
	pbx2 := NewFakePBX(t)

	if pbx1.Addr() == pbx2.Addr() {
		t.Fatalf("two FakePBX instances got the same address: %s", pbx1.Addr())
	}
}

func TestNewFakePBX_AcceptsConnection(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	res := sendOptions(t, cli, pbx.Addr())
	if res.StatusCode != 200 {
		t.Fatalf("OPTIONS response = %d, want 200", res.StatusCode)
	}
}

func TestNewFakePBX_WithUserAgent(t *testing.T) {
	pbx := NewFakePBX(t, WithUserAgent("CustomUA/1.0"))
	if pbx == nil {
		t.Fatal("NewFakePBX with WithUserAgent returned nil")
	}
}

// =============================================================================
// Phase 1b: REGISTER Handling Tests
// =============================================================================

func TestRegister_DefaultAccept(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	res := sendRegister(t, cli, pbx.Addr())
	if res.StatusCode != 200 {
		t.Fatalf("REGISTER response = %d, want 200", res.StatusCode)
	}
}

func TestRegister_OnRegister_Accept(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	pbx.OnRegister(func(reg *Register) {
		reg.Accept()
	})

	res := sendRegister(t, cli, pbx.Addr())
	if res.StatusCode != 200 {
		t.Fatalf("REGISTER response = %d, want 200", res.StatusCode)
	}
}

func TestRegister_OnRegister_Reject(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	pbx.OnRegister(func(reg *Register) {
		reg.Reject(403, "Forbidden")
	})

	res := sendRegister(t, cli, pbx.Addr())
	if res.StatusCode != 403 {
		t.Fatalf("REGISTER response = %d, want 403", res.StatusCode)
	}
}

func TestRegister_OnRegister_Challenge(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	pbx.OnRegister(func(reg *Register) {
		reg.Challenge("fakepbx", "testnonce123")
	})

	res := sendRegister(t, cli, pbx.Addr())
	if res.StatusCode != 401 {
		t.Fatalf("REGISTER response = %d, want 401", res.StatusCode)
	}

	wwwAuth := res.GetHeader("WWW-Authenticate")
	if wwwAuth == nil {
		t.Fatal("missing WWW-Authenticate header")
	}
	val := wwwAuth.Value()
	if !strings.Contains(val, `realm="fakepbx"`) {
		t.Errorf("WWW-Authenticate missing realm: %s", val)
	}
	if !strings.Contains(val, `nonce="testnonce123"`) {
		t.Errorf("WWW-Authenticate missing nonce: %s", val)
	}
}

func TestRegister_Request(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	var captured *sip.Request
	done := make(chan struct{})
	pbx.OnRegister(func(reg *Register) {
		captured = reg.Request()
		reg.Accept()
		close(done)
	})

	sendRegister(t, cli, pbx.Addr())

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not run")
	}

	if captured == nil {
		t.Fatal("Request() returned nil")
	}
	if captured.Method != sip.REGISTER {
		t.Errorf("Request().Method = %s, want REGISTER", captured.Method)
	}
}

func TestRegister_Count(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	for i := 0; i < 3; i++ {
		sendRegister(t, cli, pbx.Addr())
	}

	if got := pbx.RegisterCount(); got != 3 {
		t.Fatalf("RegisterCount() = %d, want 3", got)
	}
}

func TestRegister_LastRegister(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	sendRegister(t, cli, pbx.Addr())
	sendRegister(t, cli, pbx.Addr())

	last := pbx.LastRegister()
	if last == nil {
		t.Fatal("LastRegister() = nil")
	}
	if last.Method != sip.REGISTER {
		t.Errorf("LastRegister().Method = %s, want REGISTER", last.Method)
	}
}

func TestRegister_WaitForRegister(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	go func() {
		time.Sleep(50 * time.Millisecond)
		sendRegister(t, cli, pbx.Addr())
	}()

	if !pbx.WaitForRegister(1, 2*time.Second) {
		t.Fatal("WaitForRegister timed out")
	}
}

func TestRegister_WaitForRegister_Timeout(t *testing.T) {
	pbx := NewFakePBX(t)

	if pbx.WaitForRegister(1, 50*time.Millisecond) {
		t.Fatal("WaitForRegister should have timed out")
	}
}

// =============================================================================
// Digest Auth Tests
// =============================================================================

// sendRegisterWithAuth sends a REGISTER with a Digest Authorization header.
func sendRegisterWithAuth(t *testing.T, cli *sipgo.Client, addr, username, password, realm, nonce string) *sip.Response {
	t.Helper()
	host, port, err := sip.ParseAddr(addr)
	if err != nil {
		t.Fatalf("sendRegisterWithAuth: ParseAddr(%q): %v", addr, err)
	}

	uri := sip.Uri{Scheme: "sip", Host: host, Port: port}
	req := sip.NewRequest(sip.REGISTER, uri)
	req.SetTransport("UDP")
	req.AppendHeader(&sip.ContactHeader{
		Address: sip.Uri{Scheme: "sip", User: "test", Host: "127.0.0.1"},
	})

	// Compute digest response per RFC 2617.
	digestURI := fmt.Sprintf("sip:%s:%d", host, port)
	ha1 := md5hex(fmt.Sprintf("%s:%s:%s", username, realm, password))
	ha2 := md5hex(fmt.Sprintf("REGISTER:%s", digestURI))
	response := md5hex(fmt.Sprintf("%s:%s:%s", ha1, nonce, ha2))

	authHeader := fmt.Sprintf(
		`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s", algorithm=MD5`,
		username, realm, nonce, digestURI, response,
	)
	req.AppendHeader(sip.NewHeader("Authorization", authHeader))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res, err := cli.Do(ctx, req)
	if err != nil {
		t.Fatalf("sendRegisterWithAuth: Do: %v", err)
	}
	return res
}

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return fmt.Sprintf("%x", h)
}

func TestAuth_ChallengeAndAccept(t *testing.T) {
	pbx := NewFakePBX(t, WithAuth("alice", "secret123"))
	cli, _ := testUAC(t)

	// First REGISTER — should get 401 with WWW-Authenticate.
	res1 := sendRegister(t, cli, pbx.Addr())
	if res1.StatusCode != 401 {
		t.Fatalf("first REGISTER = %d, want 401", res1.StatusCode)
	}

	wwwAuth := res1.GetHeader("WWW-Authenticate")
	if wwwAuth == nil {
		t.Fatal("401 missing WWW-Authenticate header")
	}

	// Extract realm and nonce from the header.
	val := wwwAuth.Value()
	realm := extractParam(val, "realm")
	nonce := extractParam(val, "nonce")
	if realm == "" || nonce == "" {
		t.Fatalf("could not extract realm/nonce from: %s", val)
	}

	// Second REGISTER with correct credentials — should get 200.
	res2 := sendRegisterWithAuth(t, cli, pbx.Addr(), "alice", "secret123", realm, nonce)
	if res2.StatusCode != 200 {
		t.Fatalf("second REGISTER = %d, want 200", res2.StatusCode)
	}
}

func TestAuth_BadPassword(t *testing.T) {
	pbx := NewFakePBX(t, WithAuth("alice", "secret123"))
	cli, _ := testUAC(t)

	// Get the 401 challenge first.
	res1 := sendRegister(t, cli, pbx.Addr())
	if res1.StatusCode != 401 {
		t.Fatalf("first REGISTER = %d, want 401", res1.StatusCode)
	}

	val := res1.GetHeader("WWW-Authenticate").Value()
	realm := extractParam(val, "realm")
	nonce := extractParam(val, "nonce")

	// Send with wrong password.
	res2 := sendRegisterWithAuth(t, cli, pbx.Addr(), "alice", "wrongpassword", realm, nonce)
	if res2.StatusCode != 403 {
		t.Fatalf("REGISTER with bad password = %d, want 403", res2.StatusCode)
	}
}

func TestAuth_CustomHandlerOverrides(t *testing.T) {
	pbx := NewFakePBX(t, WithAuth("alice", "secret123"))
	cli, _ := testUAC(t)

	// Custom handler should override the default auth flow.
	pbx.OnRegister(func(reg *Register) {
		reg.Accept() // accept unconditionally
	})

	res := sendRegister(t, cli, pbx.Addr())
	if res.StatusCode != 200 {
		t.Fatalf("REGISTER with custom handler = %d, want 200 (override auth)", res.StatusCode)
	}
}

// extractParam extracts a quoted parameter value from a Digest header.
// e.g., extractParam(`Digest realm="fakepbx", nonce="abc"`, "realm") → "fakepbx"
func extractParam(header, param string) string {
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

// =============================================================================
// Phase 1c: INVITE Handling Tests
// =============================================================================

func TestInvite_DefaultAutoAnswer(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	result := sendInvite(t, cli, pbx.URI("1002"), nil)
	defer result.TX.Terminate()

	if result.Final.StatusCode != 200 {
		t.Fatalf("INVITE final response = %d, want 200", result.Final.StatusCode)
	}

	// Should have SDP body.
	if len(result.Final.Body()) == 0 {
		t.Fatal("200 OK has no SDP body")
	}

	sendACK(t, cli, result.Req, result.Final)
}

func TestInvite_OnInvite_Answer(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	answerSDP := SDP("127.0.0.1", 30000, PCMU)
	pbx.OnInvite(func(inv *Invite) {
		inv.Trying()
		time.Sleep(10 * time.Millisecond)
		inv.Ringing()
		time.Sleep(10 * time.Millisecond)
		inv.Answer(answerSDP)
	})

	result := sendInvite(t, cli, pbx.URI("1002"), SDP("127.0.0.1", 20000, PCMU))
	defer result.TX.Terminate()

	// Check provisionals: 100 Trying may be absorbed by the transaction layer,
	// but 180 Ringing should arrive.
	has180 := false
	for _, p := range result.Provisionals {
		if p.StatusCode == 180 {
			has180 = true
		}
	}
	if !has180 {
		t.Fatalf("no 180 Ringing in provisionals (got %d responses)", len(result.Provisionals))
	}

	// Check final response.
	if result.Final.StatusCode != 200 {
		t.Fatalf("final response = %d, want 200", result.Final.StatusCode)
	}
	if !strings.Contains(string(result.Final.Body()), "30000") {
		t.Error("200 OK SDP does not contain expected port 30000")
	}

	sendACK(t, cli, result.Req, result.Final)
}

func TestInvite_OnInvite_Reject(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	pbx.OnInvite(func(inv *Invite) {
		inv.Trying()
		inv.Reject(486, "Busy Here")
	})

	result := sendInvite(t, cli, pbx.URI("1002"), nil)
	defer result.TX.Terminate()

	if result.Final.StatusCode != 486 {
		t.Fatalf("final response = %d, want 486", result.Final.StatusCode)
	}
}

func TestInvite_EarlyMedia(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	earlyMediaSDP := SDP("127.0.0.1", 30000, PCMU)
	answerSDP := SDP("127.0.0.1", 30000, PCMU)

	pbx.OnInvite(func(inv *Invite) {
		inv.Trying()
		inv.EarlyMedia(earlyMediaSDP)
		time.Sleep(20 * time.Millisecond) // let provisional propagate
		inv.Answer(answerSDP)
	})

	result := sendInvite(t, cli, pbx.URI("1002"), SDP("127.0.0.1", 20000, PCMU))
	defer result.TX.Terminate()

	// Should have 100, 183 as provisionals.
	has183 := false
	for _, p := range result.Provisionals {
		if p.StatusCode == 183 {
			has183 = true
			if len(p.Body()) == 0 {
				t.Error("183 Session Progress has no SDP body")
			}
		}
	}
	if !has183 {
		t.Fatal("no 183 Session Progress in provisionals")
	}

	if result.Final.StatusCode != 200 {
		t.Fatalf("final response = %d, want 200", result.Final.StatusCode)
	}

	sendACK(t, cli, result.Req, result.Final)
}

func TestInvite_From_To_SDP(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	offerSDP := SDP("127.0.0.1", 20000, PCMU)
	var gotFrom, gotTo string
	var gotSDP []byte
	done := make(chan struct{})

	pbx.OnInvite(func(inv *Invite) {
		gotFrom = inv.From()
		gotTo = inv.To()
		gotSDP = inv.SDP()
		inv.Answer(SDP("127.0.0.1", 30000, PCMU))
		close(done)
	})

	result := sendInvite(t, cli, pbx.URI("1002"), offerSDP)
	defer result.TX.Terminate()
	sendACK(t, cli, result.Req, result.Final)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not run")
	}

	if gotFrom == "" {
		t.Error("From() returned empty string")
	}
	if gotTo == "" {
		t.Error("To() returned empty string")
	}
	if len(gotSDP) == 0 {
		t.Error("SDP() returned empty slice")
	}
}

func TestInvite_Request(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	var captured *sip.Request
	done := make(chan struct{})
	pbx.OnInvite(func(inv *Invite) {
		captured = inv.Request()
		inv.Answer(SDP("127.0.0.1", 30000, PCMU))
		close(done)
	})

	result := sendInvite(t, cli, pbx.URI("1002"), nil)
	defer result.TX.Terminate()
	sendACK(t, cli, result.Req, result.Final)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not run")
	}

	if captured == nil {
		t.Fatal("Request() returned nil")
	}
	if captured.Method != sip.INVITE {
		t.Errorf("Request().Method = %s, want INVITE", captured.Method)
	}
}

func TestInvite_AnswerOnce(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	var ac1, ac2 *ActiveCall
	done := make(chan struct{})
	pbx.OnInvite(func(inv *Invite) {
		inv.Trying()
		ac1 = inv.Answer(SDP("127.0.0.1", 30000, PCMU))
		ac2 = inv.Answer(SDP("127.0.0.1", 30002, PCMU)) // should be no-op
		close(done)
	})

	result := sendInvite(t, cli, pbx.URI("1002"), nil)
	defer result.TX.Terminate()
	sendACK(t, cli, result.Req, result.Final)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not run")
	}

	if ac1 == nil {
		t.Fatal("first Answer() returned nil ActiveCall")
	}
	if ac2 != nil {
		t.Fatal("second Answer() should return nil, got non-nil")
	}
}

func TestInvite_AutoAnswer(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	pbx.AutoAnswer(SDP("127.0.0.1", 30000, PCMU))

	result := sendInvite(t, cli, pbx.URI("1002"), nil)
	defer result.TX.Terminate()

	if result.Final.StatusCode != 200 {
		t.Fatalf("final response = %d, want 200", result.Final.StatusCode)
	}
	if len(result.Final.Body()) == 0 {
		t.Fatal("200 OK has no SDP body")
	}

	sendACK(t, cli, result.Req, result.Final)
}

func TestInvite_AutoBusy(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	pbx.AutoBusy()

	result := sendInvite(t, cli, pbx.URI("1002"), nil)
	defer result.TX.Terminate()

	if result.Final.StatusCode != 486 {
		t.Fatalf("final response = %d, want 486", result.Final.StatusCode)
	}
}

func TestInvite_AutoReject(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	pbx.AutoReject(503, "Service Unavailable")

	result := sendInvite(t, cli, pbx.URI("1002"), nil)
	defer result.TX.Terminate()

	if result.Final.StatusCode != 503 {
		t.Fatalf("final response = %d, want 503", result.Final.StatusCode)
	}
}

func TestInvite_Respond_Redirect(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	pbx.OnInvite(func(inv *Invite) {
		inv.Trying()
		inv.Respond(302, "Moved Temporarily",
			sip.NewHeader("Contact", "<sip:1003@10.0.0.5>"),
		)
	})

	result := sendInvite(t, cli, pbx.URI("1002"), nil)
	defer result.TX.Terminate()

	if result.Final.StatusCode != 302 {
		t.Fatalf("final response = %d, want 302", result.Final.StatusCode)
	}
	contact := result.Final.GetHeader("Contact")
	if contact == nil {
		t.Fatal("302 response missing Contact header")
	}
	if !strings.Contains(contact.Value(), "1003@10.0.0.5") {
		t.Fatalf("Contact = %q, want to contain 1003@10.0.0.5", contact.Value())
	}
}

func TestInvite_Respond_Provisional(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	pbx.OnInvite(func(inv *Invite) {
		inv.Trying()
		inv.Respond(182, "Queued")
		inv.Answer(SDP("127.0.0.1", 20000, PCMU))
	})

	result := sendInvite(t, cli, pbx.URI("1002"), nil)
	sendACK(t, cli, result.Req, result.Final)
	defer result.TX.Terminate()

	if result.Final.StatusCode != 200 {
		t.Fatalf("final response = %d, want 200", result.Final.StatusCode)
	}

	// Should have received 100, 182, 200
	found182 := false
	for _, p := range result.Provisionals {
		if p.StatusCode == 182 {
			found182 = true
		}
	}
	if !found182 {
		t.Fatal("did not receive 182 Queued provisional")
	}
}

func TestInvite_Respond_FinalOnce(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	pbx.OnInvite(func(inv *Invite) {
		inv.Trying()
		inv.Respond(302, "Moved Temporarily",
			sip.NewHeader("Contact", "<sip:1003@10.0.0.5>"),
		)
		// Second final response should be no-op
		inv.Respond(486, "Busy Here")
	})

	result := sendInvite(t, cli, pbx.URI("1002"), nil)
	defer result.TX.Terminate()

	if result.Final.StatusCode != 302 {
		t.Fatalf("final response = %d, want 302 (first wins)", result.Final.StatusCode)
	}
}

func TestInvite_Respond_AfterAnswer(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	pbx.OnInvite(func(inv *Invite) {
		inv.Trying()
		inv.Answer(SDP("127.0.0.1", 20000, PCMU))
		// Respond after Answer should be no-op
		inv.Respond(486, "Busy Here")
	})

	result := sendInvite(t, cli, pbx.URI("1002"), nil)
	sendACK(t, cli, result.Req, result.Final)
	defer result.TX.Terminate()

	if result.Final.StatusCode != 200 {
		t.Fatalf("final response = %d, want 200 (Answer wins)", result.Final.StatusCode)
	}
}

func TestInvite_Count(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	for i := 0; i < 2; i++ {
		result := sendInvite(t, cli, pbx.URI("1002"), nil)
		sendACK(t, cli, result.Req, result.Final)
		result.TX.Terminate()
	}

	if got := pbx.InviteCount(); got != 2 {
		t.Fatalf("InviteCount() = %d, want 2", got)
	}
}

func TestInvite_LastInvite(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	result := sendInvite(t, cli, pbx.URI("1002"), nil)
	sendACK(t, cli, result.Req, result.Final)
	result.TX.Terminate()

	last := pbx.LastInvite()
	if last == nil {
		t.Fatal("LastInvite() = nil")
	}
	if last.Method != sip.INVITE {
		t.Errorf("LastInvite().Method = %s, want INVITE", last.Method)
	}
}

func TestInvite_WaitForInvite(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	go func() {
		time.Sleep(50 * time.Millisecond)
		result := sendInvite(t, cli, pbx.URI("1002"), nil)
		sendACK(t, cli, result.Req, result.Final)
		result.TX.Terminate()
	}()

	if !pbx.WaitForInvite(1, 2*time.Second) {
		t.Fatal("WaitForInvite timed out")
	}
}

// =============================================================================
// Phase 1d: BYE + CANCEL Tests
// =============================================================================

func TestBye_DefaultAccept(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	// Establish a call.
	result := sendInvite(t, cli, pbx.URI("1002"), nil)
	sendACK(t, cli, result.Req, result.Final)
	defer result.TX.Terminate()

	// Send BYE.
	byeRes := sendBye(t, cli, result.Req, result.Final)
	if byeRes.StatusCode != 200 {
		t.Fatalf("BYE response = %d, want 200", byeRes.StatusCode)
	}
}

func TestBye_OnBye_Accept(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	pbx.OnBye(func(bye *Bye) {
		bye.Accept()
	})

	result := sendInvite(t, cli, pbx.URI("1002"), nil)
	sendACK(t, cli, result.Req, result.Final)
	defer result.TX.Terminate()

	byeRes := sendBye(t, cli, result.Req, result.Final)
	if byeRes.StatusCode != 200 {
		t.Fatalf("BYE response = %d, want 200", byeRes.StatusCode)
	}
}

func TestBye_Request(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	var captured *sip.Request
	done := make(chan struct{})
	pbx.OnBye(func(bye *Bye) {
		captured = bye.Request()
		bye.Accept()
		close(done)
	})

	result := sendInvite(t, cli, pbx.URI("1002"), nil)
	sendACK(t, cli, result.Req, result.Final)
	defer result.TX.Terminate()

	sendBye(t, cli, result.Req, result.Final)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not run")
	}

	if captured == nil {
		t.Fatal("bye.Request() returned nil")
	}
	if captured.Method != sip.BYE {
		t.Errorf("bye.Request().Method = %s, want BYE", captured.Method)
	}
}

func TestBye_Count(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	result := sendInvite(t, cli, pbx.URI("1002"), nil)
	sendACK(t, cli, result.Req, result.Final)
	defer result.TX.Terminate()

	sendBye(t, cli, result.Req, result.Final)

	if got := pbx.ByeCount(); got != 1 {
		t.Fatalf("ByeCount() = %d, want 1", got)
	}
}

func TestBye_WaitForBye(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	go func() {
		time.Sleep(50 * time.Millisecond)
		result := sendInvite(t, cli, pbx.URI("1002"), nil)
		sendACK(t, cli, result.Req, result.Final)
		// Small delay to let the server fully process the ACK before sending BYE.
		time.Sleep(50 * time.Millisecond)
		sendBye(t, cli, result.Req, result.Final)
		result.TX.Terminate()
	}()

	if !pbx.WaitForBye(1, 3*time.Second) {
		t.Fatal("WaitForBye timed out")
	}
}

func TestCancel_DuringRinging(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	cancelDetected := make(chan bool, 1)
	pbx.OnInvite(func(inv *Invite) {
		inv.Trying()
		inv.Ringing()
		// Wait for CANCEL — sipgo's transaction layer handles 200 OK + 487 automatically.
		result := inv.WaitForCancel(3 * time.Second)
		cancelDetected <- result
	})

	// Send INVITE using TransactionRequest to control the transaction.
	var uri sip.Uri
	sip.ParseUri(pbx.URI("1002"), &uri)
	req := sip.NewRequest(sip.INVITE, uri)
	req.SetTransport("UDP")
	req.AppendHeader(&sip.ContactHeader{
		Address: sip.Uri{Scheme: "sip", User: "testcaller", Host: "127.0.0.1"},
	})

	ctx, ctxCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ctxCancel()

	tx, err := cli.TransactionRequest(ctx, req)
	if err != nil {
		t.Fatalf("TransactionRequest: %v", err)
	}
	defer tx.Terminate()

	// Wait for a provisional response before sending CANCEL.
	select {
	case <-tx.Responses():
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for provisional")
	}

	// Small delay to ensure server processed the 180 and is waiting for CANCEL.
	time.Sleep(50 * time.Millisecond)

	// Build and send CANCEL with matching Via branch (required for tx matching).
	cancelReq := sip.NewRequest(sip.CANCEL, req.Recipient)
	cancelReq.SetTransport("UDP")
	cancelReq.AppendHeader(sip.HeaderClone(req.Via()))
	cancelReq.AppendHeader(sip.HeaderClone(req.From()))
	cancelReq.AppendHeader(sip.HeaderClone(req.To()))
	hdr := sip.CallIDHeader(*req.CallID())
	cancelReq.AppendHeader(&hdr)
	cancelReq.AppendHeader(&sip.CSeqHeader{SeqNo: req.CSeq().SeqNo, MethodName: sip.CANCEL})
	cancelReq.SetBody(nil)

	cancelTx, err := cli.TransactionRequest(ctx, cancelReq)
	if err != nil {
		t.Fatalf("CANCEL TransactionRequest: %v", err)
	}
	defer cancelTx.Terminate()

	// Wait for the 200 OK to the CANCEL.
	select {
	case res := <-cancelTx.Responses():
		if res.StatusCode != 200 {
			t.Fatalf("CANCEL response = %d, want 200", res.StatusCode)
		}
	case <-cancelTx.Done():
		// Transaction may end quickly — that's OK.
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for CANCEL response")
	}

	// Verify the INVITE handler detected the CANCEL.
	select {
	case detected := <-cancelDetected:
		if !detected {
			t.Fatal("WaitForCancel returned false, expected true")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel not detected in handler")
	}

	// Verify the PBX recorded the CANCEL.
	if !pbx.WaitForCancel(1, time.Second) {
		t.Fatal("PBX WaitForCancel timed out")
	}
}

func TestCancel_WaitForCancel_Timeout(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	timedOut := make(chan bool, 1)
	pbx.OnInvite(func(inv *Invite) {
		inv.Trying()
		result := inv.WaitForCancel(50 * time.Millisecond)
		timedOut <- !result
		if !result {
			inv.Reject(408, "Request Timeout")
		}
	})

	result := sendInvite(t, cli, pbx.URI("1002"), nil)
	defer result.TX.Terminate()

	select {
	case didTimeout := <-timedOut:
		if !didTimeout {
			t.Fatal("WaitForCancel should have timed out")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("test timed out")
	}
}

// =============================================================================
// ACK Tests
// =============================================================================

func TestAck_Recorded(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	result := sendInvite(t, cli, pbx.URI("1002"), nil)
	defer result.TX.Terminate()
	sendACK(t, cli, result.Req, result.Final)

	// Wait for ACK to be recorded.
	if !pbx.WaitForAck(1, time.Second) {
		t.Fatal("WaitForAck timed out")
	}
	if got := pbx.AckCount(); got != 1 {
		t.Fatalf("AckCount() = %d, want 1", got)
	}
}

func TestAck_SDP(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	var gotSDP []byte
	done := make(chan struct{})
	pbx.OnAck(func(ack *Ack) {
		gotSDP = ack.SDP()
		close(done)
	})

	result := sendInvite(t, cli, pbx.URI("1002"), nil)
	defer result.TX.Terminate()

	// Build ACK with SDP body.
	ack := sip.NewRequest(sip.ACK, result.Req.Recipient)
	ack.SipVersion = "SIP/2.0"
	ack.SetTransport("UDP")
	if h := result.Req.From(); h != nil {
		ack.AppendHeader(sip.HeaderClone(h))
	}
	if h := result.Final.To(); h != nil {
		ack.AppendHeader(sip.HeaderClone(h))
	}
	if h := result.Req.CallID(); h != nil {
		hdr := sip.CallIDHeader(*h)
		ack.AppendHeader(&hdr)
	}
	ack.AppendHeader(&sip.CSeqHeader{SeqNo: result.Req.CSeq().SeqNo, MethodName: sip.ACK})
	maxfwd := sip.MaxForwardsHeader(70)
	ack.AppendHeader(&maxfwd)
	ack.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	ack.SetBody(SDP("127.0.0.1", 40000, PCMU))
	cli.WriteRequest(ack)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("OnAck handler did not run")
	}

	if len(gotSDP) == 0 {
		t.Fatal("ack.SDP() returned empty")
	}
	if !strings.Contains(string(gotSDP), "40000") {
		t.Error("ACK SDP missing expected port 40000")
	}
}

func TestAck_WaitForAck(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	go func() {
		time.Sleep(50 * time.Millisecond)
		result := sendInvite(t, cli, pbx.URI("1002"), nil)
		sendACK(t, cli, result.Req, result.Final)
		result.TX.Terminate()
	}()

	if !pbx.WaitForAck(1, 2*time.Second) {
		t.Fatal("WaitForAck timed out")
	}
}

func TestAck_OnAck(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	var captured *sip.Request
	done := make(chan struct{})
	pbx.OnAck(func(ack *Ack) {
		captured = ack.Request()
		close(done)
	})

	result := sendInvite(t, cli, pbx.URI("1002"), nil)
	defer result.TX.Terminate()
	sendACK(t, cli, result.Req, result.Final)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("OnAck handler did not run")
	}

	if captured == nil {
		t.Fatal("ack.Request() returned nil")
	}
	if captured.Method != sip.ACK {
		t.Errorf("ack.Request().Method = %s, want ACK", captured.Method)
	}
}

// =============================================================================
// Phase 1f: ActiveCall Tests
// =============================================================================

// testClientWithServer creates a sipgo UA + Client + Server for tests where the
// PBX needs to send requests back to the client (ActiveCall tests).
// Returns the client, the server's listening address, and cleanup is automatic.
type testClientInfo struct {
	cli  *sipgo.Client
	srv  *sipgo.Server
	ua   *sipgo.UserAgent
	addr string // "127.0.0.1:PORT" — client's listening address
}

func testClientWithServer(t *testing.T) *testClientInfo {
	t.Helper()
	ua, err := sipgo.NewUA(sipgo.WithUserAgent("TestUAC"))
	if err != nil {
		t.Fatalf("testClientWithServer: NewUA: %v", err)
	}

	srv, err := sipgo.NewServer(ua)
	if err != nil {
		ua.Close()
		t.Fatalf("testClientWithServer: NewServer: %v", err)
	}

	cli, err := sipgo.NewClient(ua, sipgo.WithClientHostname("127.0.0.1"))
	if err != nil {
		ua.Close()
		t.Fatalf("testClientWithServer: NewClient: %v", err)
	}

	// Start server on ephemeral port.
	var clientAddr string
	clientReady := make(chan struct{})
	clientCtx, clientCancel := context.WithCancel(context.Background())

	go func() {
		readyFn := sipgo.ListenReadyFuncCtxValue(func(network, addr string) {
			clientAddr = addr
			close(clientReady)
		})
		srv.ListenAndServe(
			context.WithValue(clientCtx, sipgo.ListenReadyCtxKey, readyFn),
			"udp", "127.0.0.1:0",
		)
	}()

	select {
	case <-clientReady:
	case <-time.After(5 * time.Second):
		clientCancel()
		ua.Close()
		t.Fatal("testClientWithServer: server did not start")
	}

	t.Cleanup(func() {
		clientCancel()
		ua.Close()
	})

	return &testClientInfo{cli: cli, srv: srv, ua: ua, addr: clientAddr}
}

// sendInviteWithContact sends an INVITE with a specific Contact address
// (needed so the PBX knows where to send BYE/re-INVITE back).
func sendInviteWithContact(t *testing.T, cli *sipgo.Client, targetURI string, sdpBody []byte, contactAddr string) inviteResult {
	t.Helper()

	var uri sip.Uri
	if err := sip.ParseUri(targetURI, &uri); err != nil {
		t.Fatalf("sendInviteWithContact: ParseUri(%q): %v", targetURI, err)
	}

	host, port, _ := sip.ParseAddr(contactAddr)

	req := sip.NewRequest(sip.INVITE, uri)
	req.SetTransport("UDP")
	if sdpBody != nil {
		req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		req.SetBody(sdpBody)
	}
	req.AppendHeader(&sip.ContactHeader{
		Address: sip.Uri{Scheme: "sip", User: "testcaller", Host: host, Port: port},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tx, err := cli.TransactionRequest(ctx, req)
	if err != nil {
		t.Fatalf("sendInviteWithContact: TransactionRequest: %v", err)
	}

	result := inviteResult{TX: tx, Req: req}
	for {
		select {
		case res := <-tx.Responses():
			if res.IsProvisional() {
				result.Provisionals = append(result.Provisionals, res)
				continue
			}
			result.Final = res
			return result
		case <-tx.Done():
			if result.Final == nil {
				t.Fatalf("sendInviteWithContact: transaction ended without final response: %v", tx.Err())
			}
			return result
		case <-ctx.Done():
			t.Fatal("sendInviteWithContact: timeout")
			return result
		}
	}
}

func TestActiveCall_SendBye(t *testing.T) {
	pbx := NewFakePBX(t)
	tc := testClientWithServer(t)

	byeReceived := make(chan struct{}, 1)
	tc.srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		res := sip.NewResponseFromRequest(req, 200, "OK", nil)
		tx.Respond(res)
		close(byeReceived)
	})

	var activeCall *ActiveCall
	callReady := make(chan struct{})
	pbx.OnInvite(func(inv *Invite) {
		inv.Trying()
		activeCall = inv.Answer(SDP("127.0.0.1", 30000, PCMU))
		close(callReady)
	})

	result := sendInviteWithContact(t, tc.cli, pbx.URI("1002"), SDP("127.0.0.1", 20000, PCMU), tc.addr)
	defer result.TX.Terminate()
	sendACK(t, tc.cli, result.Req, result.Final)

	select {
	case <-callReady:
	case <-time.After(2 * time.Second):
		t.Fatal("call not established")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := activeCall.SendBye(ctx)
	if err != nil {
		t.Fatalf("SendBye: %v", err)
	}

	select {
	case <-byeReceived:
	case <-time.After(2 * time.Second):
		t.Fatal("client did not receive BYE")
	}
}

func TestActiveCall_SendReInvite(t *testing.T) {
	pbx := NewFakePBX(t)
	tc := testClientWithServer(t)

	reinviteReceived := make(chan []byte, 1)
	tc.srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		reinviteReceived <- req.Body()
		res := sip.NewResponseFromRequest(req, 200, "OK", nil)
		tx.Respond(res)
	})

	var activeCall *ActiveCall
	callReady := make(chan struct{})
	pbx.OnInvite(func(inv *Invite) {
		inv.Trying()
		activeCall = inv.Answer(SDP("127.0.0.1", 30000, PCMU))
		close(callReady)
	})

	result := sendInviteWithContact(t, tc.cli, pbx.URI("1002"), SDP("127.0.0.1", 20000, PCMU), tc.addr)
	defer result.TX.Terminate()
	sendACK(t, tc.cli, result.Req, result.Final)

	select {
	case <-callReady:
	case <-time.After(2 * time.Second):
		t.Fatal("call not established")
	}

	holdSDP := SDPWithDirection("127.0.0.1", 30000, "sendonly", PCMU)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := activeCall.SendReInvite(ctx, holdSDP)
	if err != nil {
		t.Fatalf("SendReInvite: %v", err)
	}

	select {
	case body := <-reinviteReceived:
		if !strings.Contains(string(body), "sendonly") {
			t.Error("re-INVITE body missing sendonly direction")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client did not receive re-INVITE")
	}
}

func TestActiveCall_SendNotify(t *testing.T) {
	pbx := NewFakePBX(t)
	tc := testClientWithServer(t)

	notifyReceived := make(chan *sip.Request, 1)
	tc.srv.OnNotify(func(req *sip.Request, tx sip.ServerTransaction) {
		notifyReceived <- req
		res := sip.NewResponseFromRequest(req, 200, "OK", nil)
		tx.Respond(res)
	})

	var activeCall *ActiveCall
	callReady := make(chan struct{})
	pbx.OnInvite(func(inv *Invite) {
		inv.Trying()
		activeCall = inv.Answer(SDP("127.0.0.1", 30000, PCMU))
		close(callReady)
	})

	result := sendInviteWithContact(t, tc.cli, pbx.URI("1002"), SDP("127.0.0.1", 20000, PCMU), tc.addr)
	defer result.TX.Terminate()
	sendACK(t, tc.cli, result.Req, result.Final)

	select {
	case <-callReady:
	case <-time.After(2 * time.Second):
		t.Fatal("call not established")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := activeCall.SendNotify(ctx, "refer", "SIP/2.0 200 OK\r\n")
	if err != nil {
		t.Fatalf("SendNotify: %v", err)
	}

	select {
	case req := <-notifyReceived:
		event := req.GetHeader("Event")
		if event == nil || event.Value() != "refer" {
			t.Errorf("NOTIFY Event header = %v, want 'refer'", event)
		}
		if !strings.Contains(string(req.Body()), "200 OK") {
			t.Error("NOTIFY body missing '200 OK'")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client did not receive NOTIFY")
	}
}

func TestActiveCall_SendBye_CancelledContext(t *testing.T) {
	pbx := NewFakePBX(t)
	tc := testClientWithServer(t)

	var activeCall *ActiveCall
	callReady := make(chan struct{})
	pbx.OnInvite(func(inv *Invite) {
		inv.Trying()
		activeCall = inv.Answer(SDP("127.0.0.1", 30000, PCMU))
		close(callReady)
	})

	result := sendInviteWithContact(t, tc.cli, pbx.URI("1002"), SDP("127.0.0.1", 20000, PCMU), tc.addr)
	defer result.TX.Terminate()
	sendACK(t, tc.cli, result.Req, result.Final)

	select {
	case <-callReady:
	case <-time.After(2 * time.Second):
		t.Fatal("call not established")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := activeCall.SendBye(ctx)
	if err == nil {
		t.Fatal("SendBye with cancelled context should return error")
	}
}

func TestActiveCall_CSeq_Increments(t *testing.T) {
	pbx := NewFakePBX(t)
	tc := testClientWithServer(t)

	var cseqs []uint32
	var mu sync.Mutex
	tc.srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		mu.Lock()
		cseqs = append(cseqs, req.CSeq().SeqNo)
		mu.Unlock()
		res := sip.NewResponseFromRequest(req, 200, "OK", nil)
		tx.Respond(res)
	})

	var activeCall *ActiveCall
	callReady := make(chan struct{})
	pbx.OnInvite(func(inv *Invite) {
		inv.Trying()
		activeCall = inv.Answer(SDP("127.0.0.1", 30000, PCMU))
		close(callReady)
	})

	result := sendInviteWithContact(t, tc.cli, pbx.URI("1002"), SDP("127.0.0.1", 20000, PCMU), tc.addr)
	defer result.TX.Terminate()
	sendACK(t, tc.cli, result.Req, result.Final)

	select {
	case <-callReady:
	case <-time.After(2 * time.Second):
		t.Fatal("call not established")
	}

	ctx := context.Background()

	// Send two re-INVITEs — CSeq must increment each time.
	holdSDP := SDPWithDirection("127.0.0.1", 30000, "sendonly", PCMU)
	if err := activeCall.SendReInvite(ctx, holdSDP); err != nil {
		t.Fatalf("first SendReInvite: %v", err)
	}
	unholdSDP := SDPWithDirection("127.0.0.1", 30000, "sendrecv", PCMU)
	if err := activeCall.SendReInvite(ctx, unholdSDP); err != nil {
		t.Fatalf("second SendReInvite: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(cseqs) != 2 {
		t.Fatalf("got %d re-INVITEs, want 2", len(cseqs))
	}
	if cseqs[0] >= cseqs[1] {
		t.Fatalf("CSeq did not increment: first=%d, second=%d", cseqs[0], cseqs[1])
	}
}

func TestActiveCall_CSeq_AcrossMethods(t *testing.T) {
	pbx := NewFakePBX(t)
	tc := testClientWithServer(t)

	var cseqs []uint32
	var mu sync.Mutex
	recordCSeq := func(req *sip.Request, tx sip.ServerTransaction) {
		mu.Lock()
		cseqs = append(cseqs, req.CSeq().SeqNo)
		mu.Unlock()
		res := sip.NewResponseFromRequest(req, 200, "OK", nil)
		tx.Respond(res)
	}
	tc.srv.OnInvite(recordCSeq)
	tc.srv.OnNotify(recordCSeq)
	tc.srv.OnBye(recordCSeq)

	var activeCall *ActiveCall
	callReady := make(chan struct{})
	pbx.OnInvite(func(inv *Invite) {
		inv.Trying()
		activeCall = inv.Answer(SDP("127.0.0.1", 30000, PCMU))
		close(callReady)
	})

	result := sendInviteWithContact(t, tc.cli, pbx.URI("1002"), SDP("127.0.0.1", 20000, PCMU), tc.addr)
	defer result.TX.Terminate()
	sendACK(t, tc.cli, result.Req, result.Final)

	select {
	case <-callReady:
	case <-time.After(2 * time.Second):
		t.Fatal("call not established")
	}

	ctx := context.Background()

	// re-INVITE, then NOTIFY, then BYE — CSeq must increment across all.
	if err := activeCall.SendReInvite(ctx, SDPWithDirection("127.0.0.1", 30000, "sendonly", PCMU)); err != nil {
		t.Fatalf("SendReInvite: %v", err)
	}
	if err := activeCall.SendNotify(ctx, "refer", "SIP/2.0 200 OK\r\n"); err != nil {
		t.Fatalf("SendNotify: %v", err)
	}
	if err := activeCall.SendBye(ctx); err != nil {
		t.Fatalf("SendBye: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(cseqs) != 3 {
		t.Fatalf("got %d requests, want 3", len(cseqs))
	}
	for i := 1; i < len(cseqs); i++ {
		if cseqs[i] <= cseqs[i-1] {
			t.Fatalf("CSeq not strictly increasing: %v", cseqs)
		}
	}
}

// =============================================================================
// Phase 1g: REFER + OPTIONS Tests
// =============================================================================

func TestRefer_DefaultAccept(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	res := sendRefer(t, cli, pbx.Addr(), "sip:1003@example.com")
	if res.StatusCode != 202 {
		t.Fatalf("REFER response = %d, want 202", res.StatusCode)
	}
}

func TestRefer_OnRefer_Accept(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	pbx.OnRefer(func(ref *Refer) {
		ref.Accept()
	})

	res := sendRefer(t, cli, pbx.Addr(), "sip:1003@example.com")
	if res.StatusCode != 202 {
		t.Fatalf("REFER response = %d, want 202", res.StatusCode)
	}
}

func TestRefer_OnRefer_Reject(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	pbx.OnRefer(func(ref *Refer) {
		ref.Reject(403, "Forbidden")
	})

	res := sendRefer(t, cli, pbx.Addr(), "sip:1003@example.com")
	if res.StatusCode != 403 {
		t.Fatalf("REFER response = %d, want 403", res.StatusCode)
	}
}

func TestRefer_ReferTo(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	var got string
	done := make(chan struct{})
	pbx.OnRefer(func(ref *Refer) {
		got = ref.ReferTo()
		ref.Accept()
		close(done)
	})

	sendRefer(t, cli, pbx.Addr(), "sip:1003@example.com")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not run")
	}

	if got != "sip:1003@example.com" {
		t.Errorf("ReferTo() = %q, want %q", got, "sip:1003@example.com")
	}
}

func TestRefer_Count(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	sendRefer(t, cli, pbx.Addr(), "sip:1003@example.com")
	sendRefer(t, cli, pbx.Addr(), "sip:1004@example.com")

	if got := pbx.ReferCount(); got != 2 {
		t.Fatalf("ReferCount() = %d, want 2", got)
	}
}

func TestOptions_DefaultAccept(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	res := sendOptions(t, cli, pbx.Addr())
	if res.StatusCode != 200 {
		t.Fatalf("OPTIONS response = %d, want 200", res.StatusCode)
	}
}

func TestOptions_OnOptions_Accept(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	pbx.OnOptions(func(opt *Options) {
		opt.Accept()
	})

	res := sendOptions(t, cli, pbx.Addr())
	if res.StatusCode != 200 {
		t.Fatalf("OPTIONS response = %d, want 200", res.StatusCode)
	}
}

func TestOptions_Request(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	var captured *sip.Request
	done := make(chan struct{})
	pbx.OnOptions(func(opt *Options) {
		captured = opt.Request()
		opt.Accept()
		close(done)
	})

	sendOptions(t, cli, pbx.Addr())

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not run")
	}

	if captured == nil {
		t.Fatal("Request() returned nil")
	}
	if captured.Method != sip.OPTIONS {
		t.Errorf("Request().Method = %s, want OPTIONS", captured.Method)
	}
}

// =============================================================================
// INFO, MESSAGE, SUBSCRIBE Tests
// =============================================================================

// sendRequest sends a generic SIP request to the given address with an optional body.
func sendRequest(t *testing.T, cli *sipgo.Client, addr string, method sip.RequestMethod, hdrs []sip.Header, body []byte) *sip.Response {
	t.Helper()
	host, port, err := sip.ParseAddr(addr)
	if err != nil {
		t.Fatalf("sendRequest: ParseAddr(%q): %v", addr, err)
	}

	req := sip.NewRequest(method, sip.Uri{Scheme: "sip", Host: host, Port: port})
	req.SetTransport("UDP")
	for _, h := range hdrs {
		req.AppendHeader(h)
	}
	if body != nil {
		req.SetBody(body)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res, err := cli.Do(ctx, req)
	if err != nil {
		t.Fatalf("sendRequest(%s): Do: %v", method, err)
	}
	return res
}

func TestInfo_DefaultAccept(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	res := sendRequest(t, cli, pbx.Addr(), sip.INFO, nil, nil)
	if res.StatusCode != 200 {
		t.Fatalf("INFO response = %d, want 200", res.StatusCode)
	}
}

func TestInfo_OnInfo(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	var gotBody string
	done := make(chan struct{})
	pbx.OnInfo(func(info *Info) {
		gotBody = string(info.Body())
		info.Accept()
		close(done)
	})

	body := []byte("Signal=1\r\nDuration=160\r\n")
	hdrs := []sip.Header{sip.NewHeader("Content-Type", "application/dtmf-relay")}
	res := sendRequest(t, cli, pbx.Addr(), sip.INFO, hdrs, body)
	if res.StatusCode != 200 {
		t.Fatalf("INFO response = %d, want 200", res.StatusCode)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("OnInfo handler did not run")
	}

	if !strings.Contains(gotBody, "Signal=1") {
		t.Errorf("info.Body() = %q, want to contain Signal=1", gotBody)
	}
}

func TestInfo_Count(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	sendRequest(t, cli, pbx.Addr(), sip.INFO, nil, nil)
	sendRequest(t, cli, pbx.Addr(), sip.INFO, nil, nil)

	if got := pbx.InfoCount(); got != 2 {
		t.Fatalf("InfoCount() = %d, want 2", got)
	}
}

func TestMessage_DefaultAccept(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	hdrs := []sip.Header{sip.NewHeader("Content-Type", "text/plain")}
	res := sendRequest(t, cli, pbx.Addr(), sip.MESSAGE, hdrs, []byte("Hello!"))
	if res.StatusCode != 200 {
		t.Fatalf("MESSAGE response = %d, want 200", res.StatusCode)
	}
}

func TestMessage_OnMessage(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	var gotBody []byte
	done := make(chan struct{})
	pbx.OnMessage(func(msg *Message) {
		gotBody = msg.Body()
		msg.Accept()
		close(done)
	})

	hdrs := []sip.Header{sip.NewHeader("Content-Type", "text/plain")}
	sendRequest(t, cli, pbx.Addr(), sip.MESSAGE, hdrs, []byte("Hello!"))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("OnMessage handler did not run")
	}

	if string(gotBody) != "Hello!" {
		t.Errorf("msg.Body() = %q, want %q", gotBody, "Hello!")
	}
}

func TestMessage_Count(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	hdrs := []sip.Header{sip.NewHeader("Content-Type", "text/plain")}
	sendRequest(t, cli, pbx.Addr(), sip.MESSAGE, hdrs, []byte("hi"))
	sendRequest(t, cli, pbx.Addr(), sip.MESSAGE, hdrs, []byte("hi"))

	if got := pbx.MessageCount(); got != 2 {
		t.Fatalf("MessageCount() = %d, want 2", got)
	}
}

func TestSubscribe_DefaultAccept(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	hdrs := []sip.Header{sip.NewHeader("Event", "presence")}
	res := sendRequest(t, cli, pbx.Addr(), sip.SUBSCRIBE, hdrs, nil)
	if res.StatusCode != 200 {
		t.Fatalf("SUBSCRIBE response = %d, want 200", res.StatusCode)
	}
}

func TestSubscribe_OnSubscribe(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	var gotEvent string
	done := make(chan struct{})
	pbx.OnSubscribe(func(sub *Subscribe) {
		gotEvent = sub.Event()
		sub.Accept()
		close(done)
	})

	hdrs := []sip.Header{sip.NewHeader("Event", "dialog")}
	sendRequest(t, cli, pbx.Addr(), sip.SUBSCRIBE, hdrs, nil)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("OnSubscribe handler did not run")
	}

	if gotEvent != "dialog" {
		t.Errorf("sub.Event() = %q, want %q", gotEvent, "dialog")
	}
}

func TestSubscribe_Count(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	hdrs := []sip.Header{sip.NewHeader("Event", "presence")}
	sendRequest(t, cli, pbx.Addr(), sip.SUBSCRIBE, hdrs, nil)

	if got := pbx.SubscribeCount(); got != 1 {
		t.Fatalf("SubscribeCount() = %d, want 1", got)
	}
}

// =============================================================================
// Phase 1h: Assertion Helper Tests
// =============================================================================

func TestRequests_AllMethods(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	// REGISTER
	sendRegister(t, cli, pbx.Addr())

	// INVITE + ACK + BYE
	result := sendInvite(t, cli, pbx.URI("1002"), nil)
	sendACK(t, cli, result.Req, result.Final)
	sendBye(t, cli, result.Req, result.Final)
	result.TX.Terminate()

	regs := pbx.Requests(sip.REGISTER)
	if len(regs) != 1 {
		t.Errorf("Requests(REGISTER) = %d, want 1", len(regs))
	}
	invites := pbx.Requests(sip.INVITE)
	if len(invites) != 1 {
		t.Errorf("Requests(INVITE) = %d, want 1", len(invites))
	}
	byes := pbx.Requests(sip.BYE)
	if len(byes) != 1 {
		t.Errorf("Requests(BYE) = %d, want 1", len(byes))
	}

	// All should have non-zero timestamps.
	for _, r := range regs {
		if r.Timestamp.IsZero() {
			t.Error("RecordedRequest has zero Timestamp")
		}
	}
}

func TestRequests_Empty(t *testing.T) {
	pbx := NewFakePBX(t)

	reqs := pbx.Requests(sip.INVITE)
	if reqs == nil {
		t.Fatal("Requests() returned nil, want empty slice")
	}
	if len(reqs) != 0 {
		t.Fatalf("Requests() = %d entries, want 0", len(reqs))
	}
}

func TestRequests_Timestamps(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	sendRegister(t, cli, pbx.Addr())
	time.Sleep(50 * time.Millisecond)
	sendRegister(t, cli, pbx.Addr())

	regs := pbx.Requests(sip.REGISTER)
	if len(regs) < 2 {
		t.Fatalf("got %d REGISTERs, want >= 2", len(regs))
	}
	if !regs[1].Timestamp.After(regs[0].Timestamp) {
		t.Errorf("second timestamp %v should be after first %v", regs[1].Timestamp, regs[0].Timestamp)
	}
}

func TestWaitForInvite_MultipleN(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(50 * time.Millisecond)
			result := sendInvite(t, cli, pbx.URI("1002"), nil)
			sendACK(t, cli, result.Req, result.Final)
			result.TX.Terminate()
		}()
	}

	if !pbx.WaitForInvite(3, 3*time.Second) {
		t.Fatal("WaitForInvite(3) timed out")
	}
	wg.Wait()
}

func TestWaitForBye_Timeout(t *testing.T) {
	pbx := NewFakePBX(t)

	if pbx.WaitForBye(1, 50*time.Millisecond) {
		t.Fatal("WaitForBye should have timed out")
	}
}

func TestWaitForRegister_AlreadyMet(t *testing.T) {
	pbx := NewFakePBX(t)
	cli, _ := testUAC(t)

	sendRegister(t, cli, pbx.Addr())
	sendRegister(t, cli, pbx.Addr())

	// Count already met — should return immediately.
	start := time.Now()
	if !pbx.WaitForRegister(2, time.Second) {
		t.Fatal("WaitForRegister should have returned true")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("WaitForRegister took %v, expected immediate return", elapsed)
	}
}

// =============================================================================
// Phase 2: Outbound INVITE (UAC) Tests
// =============================================================================

// testUAS creates a sipgo UA + Server + Client for receiving SIP requests.
// Handlers must be registered on uas.srv BEFORE calling uas.start(t).
// This avoids a data race with sipgo's internal handler map.
type testUASInfo struct {
	cli  *sipgo.Client
	srv  *sipgo.Server
	ua   *sipgo.UserAgent
	addr string // "127.0.0.1:PORT" — set after start()
}

func testUAS(t *testing.T) *testUASInfo {
	t.Helper()
	ua, err := sipgo.NewUA(sipgo.WithUserAgent("TestUAS"))
	if err != nil {
		t.Fatalf("testUAS: NewUA: %v", err)
	}

	srv, err := sipgo.NewServer(ua)
	if err != nil {
		ua.Close()
		t.Fatalf("testUAS: NewServer: %v", err)
	}

	cli, err := sipgo.NewClient(ua, sipgo.WithClientHostname("127.0.0.1"))
	if err != nil {
		ua.Close()
		t.Fatalf("testUAS: NewClient: %v", err)
	}

	return &testUASInfo{cli: cli, srv: srv, ua: ua}
}

// start begins listening on an ephemeral port. Must be called after all
// handlers are registered on u.srv.
func (u *testUASInfo) start(t *testing.T) {
	t.Helper()
	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		readyFn := sipgo.ListenReadyFuncCtxValue(func(network, a string) {
			u.addr = a
			close(ready)
		})
		u.srv.ListenAndServe(
			context.WithValue(ctx, sipgo.ListenReadyCtxKey, readyFn),
			"udp", "127.0.0.1:0",
		)
	}()

	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		cancel()
		u.ua.Close()
		t.Fatal("testUAS: server did not start")
	}

	t.Cleanup(func() {
		cancel()
		u.ua.Close()
	})
}

// uasURI returns a SIP URI for the test UAS: "sip:EXT@127.0.0.1:PORT"
func (u *testUASInfo) uasURI(ext string) string {
	return fmt.Sprintf("sip:%s@%s", ext, u.addr)
}

// autoAnswer makes the test UAS auto-answer INVITEs with 200 OK + SDP.
// Must be called before start().
func (u *testUASInfo) autoAnswer(answerSDP []byte) {
	u.srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		host, port, _ := sip.ParseAddr(u.addr)
		res := sip.NewResponseFromRequest(req, 200, "OK", nil)
		res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		res.AppendHeader(&sip.ContactHeader{
			Address: sip.Uri{Scheme: "sip", Host: host, Port: port},
		})
		res.SetBody(answerSDP)
		tx.Respond(res)
	})
}

func TestSendInvite_Basic(t *testing.T) {
	pbx := NewFakePBX(t)
	uas := testUAS(t)
	uas.autoAnswer(SDP("127.0.0.1", 40000, PCMU))
	uas.start(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	call, err := pbx.SendInvite(ctx, uas.uasURI("alice"), SDP("127.0.0.1", 20000, PCMU))
	if err != nil {
		t.Fatalf("SendInvite: %v", err)
	}
	if call == nil {
		t.Fatal("SendInvite returned nil OutboundCall")
	}

	// Verify we can inspect the response.
	if call.Response().StatusCode != 200 {
		t.Errorf("Response().StatusCode = %d, want 200", call.Response().StatusCode)
	}
	if len(call.Response().Body()) == 0 {
		t.Error("Response() has no SDP body")
	}
	if call.Request() == nil {
		t.Error("Request() returned nil")
	}
}

func TestSendInvite_Rejected(t *testing.T) {
	pbx := NewFakePBX(t)
	uas := testUAS(t)
	uas.srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		res := sip.NewResponseFromRequest(req, 486, "Busy Here", nil)
		tx.Respond(res)
	})
	uas.start(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	call, err := pbx.SendInvite(ctx, uas.uasURI("alice"), SDP("127.0.0.1", 20000, PCMU))
	if err == nil {
		t.Fatal("SendInvite should return error on rejection")
	}
	if call != nil {
		t.Fatal("SendInvite should return nil OutboundCall on rejection")
	}
	if !strings.Contains(err.Error(), "486") {
		t.Errorf("error = %q, want to contain '486'", err)
	}
}

func TestSendInvite_InvalidURI(t *testing.T) {
	pbx := NewFakePBX(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := pbx.SendInvite(ctx, "not-a-sip-uri", nil)
	if err == nil {
		t.Fatal("SendInvite should fail on invalid URI")
	}
}

func TestSendInvite_CancelledContext(t *testing.T) {
	pbx := NewFakePBX(t)
	uas := testUAS(t)
	// UAS delays answer — context will be cancelled before it responds.
	uas.srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		time.Sleep(2 * time.Second)
		res := sip.NewResponseFromRequest(req, 200, "OK", nil)
		tx.Respond(res)
	})
	uas.start(t)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := pbx.SendInvite(ctx, uas.uasURI("alice"), SDP("127.0.0.1", 20000, PCMU))
	if err == nil {
		t.Fatal("SendInvite should fail with cancelled context")
	}
}

func TestSendInvite_SendBye(t *testing.T) {
	pbx := NewFakePBX(t)
	uas := testUAS(t)
	uas.autoAnswer(SDP("127.0.0.1", 40000, PCMU))
	byeReceived := make(chan struct{}, 1)
	uas.srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		res := sip.NewResponseFromRequest(req, 200, "OK", nil)
		tx.Respond(res)
		close(byeReceived)
	})
	uas.start(t)

	ctx := context.Background()

	call, err := pbx.SendInvite(ctx, uas.uasURI("alice"), SDP("127.0.0.1", 20000, PCMU))
	if err != nil {
		t.Fatalf("SendInvite: %v", err)
	}

	if err := call.SendBye(ctx); err != nil {
		t.Fatalf("SendBye: %v", err)
	}

	select {
	case <-byeReceived:
	case <-time.After(2 * time.Second):
		t.Fatal("UAS did not receive BYE")
	}
}

func TestSendInvite_SendReInvite(t *testing.T) {
	pbx := NewFakePBX(t)
	uas := testUAS(t)

	reinviteReceived := make(chan []byte, 1)
	var first sync.Once
	uas.srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		isFirst := false
		first.Do(func() { isFirst = true })
		if isFirst {
			// Initial INVITE — answer normally.
			host, port, _ := sip.ParseAddr(uas.addr)
			res := sip.NewResponseFromRequest(req, 200, "OK", nil)
			res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
			res.AppendHeader(&sip.ContactHeader{
				Address: sip.Uri{Scheme: "sip", Host: host, Port: port},
			})
			res.SetBody(SDP("127.0.0.1", 40000, PCMU))
			tx.Respond(res)
		} else {
			// re-INVITE
			reinviteReceived <- req.Body()
			res := sip.NewResponseFromRequest(req, 200, "OK", nil)
			tx.Respond(res)
		}
	})
	uas.start(t)

	ctx := context.Background()

	call, err := pbx.SendInvite(ctx, uas.uasURI("alice"), SDP("127.0.0.1", 20000, PCMU))
	if err != nil {
		t.Fatalf("SendInvite: %v", err)
	}

	holdSDP := SDPWithDirection("127.0.0.1", 20000, "sendonly", PCMU)
	if err := call.SendReInvite(ctx, holdSDP); err != nil {
		t.Fatalf("SendReInvite: %v", err)
	}

	select {
	case body := <-reinviteReceived:
		if !strings.Contains(string(body), "sendonly") {
			t.Error("re-INVITE body missing sendonly direction")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("UAS did not receive re-INVITE")
	}
}

func TestSendInvite_RemoteBye(t *testing.T) {
	pbx := NewFakePBX(t)
	uas := testUAS(t)
	uas.autoAnswer(SDP("127.0.0.1", 40000, PCMU))
	uas.start(t)

	ctx := context.Background()

	call, err := pbx.SendInvite(ctx, uas.uasURI("alice"), SDP("127.0.0.1", 20000, PCMU))
	if err != nil {
		t.Fatalf("SendInvite: %v", err)
	}

	// Remote (UAS) sends BYE to the PBX. The target is the PBX's Contact
	// from the INVITE (the PBX's listening address).
	pbxContact := call.Request().Contact()
	if pbxContact == nil {
		t.Fatal("INVITE has no Contact header")
	}
	bye := sip.NewRequest(sip.BYE, pbxContact.Address)
	bye.SetTransport("UDP")
	// In BYE from UAS→PBX: From=UAS (response To), To=PBX (response From)
	if h := call.Response().To(); h != nil {
		bye.AppendHeader(&sip.FromHeader{
			DisplayName: h.DisplayName,
			Address:     h.Address,
			Params:      h.Params,
		})
	}
	if h := call.Response().From(); h != nil {
		bye.AppendHeader(&sip.ToHeader{
			DisplayName: h.DisplayName,
			Address:     h.Address,
			Params:      h.Params,
		})
	}
	if h := call.Request().CallID(); h != nil {
		hdr := sip.CallIDHeader(*h)
		bye.AppendHeader(&hdr)
	}
	bye.AppendHeader(&sip.CSeqHeader{SeqNo: 1, MethodName: sip.BYE})

	byeCtx, byeCancel := context.WithTimeout(ctx, 2*time.Second)
	defer byeCancel()
	res, err := uas.cli.Do(byeCtx, bye)
	if err != nil {
		t.Fatalf("UAS SendBye: %v", err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("BYE response = %d, want 200", res.StatusCode)
	}

	// Verify PBX recorded the BYE.
	if !pbx.WaitForBye(1, time.Second) {
		t.Fatal("PBX did not record BYE")
	}
}

func TestSendInvite_Concurrent(t *testing.T) {
	pbx := NewFakePBX(t)
	uas := testUAS(t)
	uas.autoAnswer(SDP("127.0.0.1", 40000, PCMU))
	uas.start(t)

	const numCalls = 5
	calls := make([]*OutboundCall, numCalls)
	errs := make([]error, numCalls)

	var wg sync.WaitGroup
	for i := 0; i < numCalls; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			calls[idx], errs[idx] = pbx.SendInvite(ctx, uas.uasURI(fmt.Sprintf("ext%d", idx)), SDP("127.0.0.1", 20000+idx*2, PCMU))
		}(i)
	}
	wg.Wait()

	// All calls should succeed.
	for i := 0; i < numCalls; i++ {
		if errs[i] != nil {
			t.Errorf("call %d failed: %v", i, errs[i])
		}
		if calls[i] == nil {
			t.Errorf("call %d returned nil", i)
			continue
		}
		if calls[i].Response().StatusCode != 200 {
			t.Errorf("call %d status = %d, want 200", i, calls[i].Response().StatusCode)
		}
	}

	// Hang up all calls concurrently.
	for i := 0; i < numCalls; i++ {
		if calls[i] == nil {
			continue
		}
		wg.Add(1)
		go func(call *OutboundCall) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			call.SendBye(ctx)
		}(calls[i])
	}
	wg.Wait()
}

func TestSendInvite_WithProvisionals(t *testing.T) {
	pbx := NewFakePBX(t)
	uas := testUAS(t)
	// UAS sends 100 + 180 + 200.
	uas.srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		res100 := sip.NewResponseFromRequest(req, 100, "Trying", nil)
		tx.Respond(res100)

		time.Sleep(10 * time.Millisecond)

		res180 := sip.NewResponseFromRequest(req, 180, "Ringing", nil)
		tx.Respond(res180)

		time.Sleep(10 * time.Millisecond)

		host, port, _ := sip.ParseAddr(uas.addr)
		res200 := sip.NewResponseFromRequest(req, 200, "OK", nil)
		res200.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		res200.AppendHeader(&sip.ContactHeader{
			Address: sip.Uri{Scheme: "sip", Host: host, Port: port},
		})
		res200.SetBody(SDP("127.0.0.1", 40000, PCMU))
		tx.Respond(res200)
	})
	uas.start(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	call, err := pbx.SendInvite(ctx, uas.uasURI("alice"), SDP("127.0.0.1", 20000, PCMU))
	if err != nil {
		t.Fatalf("SendInvite: %v", err)
	}
	if call.Response().StatusCode != 200 {
		t.Fatalf("final response = %d, want 200", call.Response().StatusCode)
	}
}

func TestSendInvite_CSeq_Increments(t *testing.T) {
	pbx := NewFakePBX(t)
	uas := testUAS(t)
	uas.autoAnswer(SDP("127.0.0.1", 40000, PCMU))
	var cseqs []uint32
	var mu sync.Mutex
	recordCSeq := func(req *sip.Request, tx sip.ServerTransaction) {
		mu.Lock()
		cseqs = append(cseqs, req.CSeq().SeqNo)
		mu.Unlock()
		res := sip.NewResponseFromRequest(req, 200, "OK", nil)
		tx.Respond(res)
	}
	uas.srv.OnNotify(recordCSeq)
	uas.srv.OnBye(recordCSeq)
	uas.start(t)

	ctx := context.Background()

	call, err := pbx.SendInvite(ctx, uas.uasURI("alice"), SDP("127.0.0.1", 20000, PCMU))
	if err != nil {
		t.Fatalf("SendInvite: %v", err)
	}

	if err := call.SendNotify(ctx, "refer", "SIP/2.0 200 OK\r\n"); err != nil {
		t.Fatalf("SendNotify: %v", err)
	}
	if err := call.SendBye(ctx); err != nil {
		t.Fatalf("SendBye: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(cseqs) != 2 {
		t.Fatalf("got %d requests, want 2", len(cseqs))
	}
	if cseqs[0] >= cseqs[1] {
		t.Fatalf("CSeq not strictly increasing: %v", cseqs)
	}
}

// =============================================================================
// Phase 3: In-Dialog REFER (UAC) Tests
// =============================================================================

func TestSendRefer_Basic(t *testing.T) {
	pbx := NewFakePBX(t)
	uas := testUAS(t)
	uas.autoAnswer(SDP("127.0.0.1", 40000, PCMU))
	gotReferTo := make(chan string, 1)
	uas.srv.OnRefer(func(req *sip.Request, tx sip.ServerTransaction) {
		val := ""
		if h := req.GetHeader("Refer-To"); h != nil {
			val = h.Value()
		}
		res := sip.NewResponseFromRequest(req, 202, "Accepted", nil)
		tx.Respond(res)
		gotReferTo <- val
	})
	uas.start(t)

	ctx := context.Background()

	call, err := pbx.SendInvite(ctx, uas.uasURI("alice"), SDP("127.0.0.1", 20000, PCMU))
	if err != nil {
		t.Fatalf("SendInvite: %v", err)
	}

	target := "sip:bob@192.168.1.100"
	if err := call.SendRefer(ctx, target); err != nil {
		t.Fatalf("SendRefer: %v", err)
	}

	select {
	case val := <-gotReferTo:
		if !strings.Contains(val, target) {
			t.Errorf("Refer-To = %q, want to contain %q", val, target)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("UAS did not receive REFER")
	}
}

func TestSendRefer_Rejected(t *testing.T) {
	pbx := NewFakePBX(t)
	uas := testUAS(t)
	uas.autoAnswer(SDP("127.0.0.1", 40000, PCMU))
	uas.srv.OnRefer(func(req *sip.Request, tx sip.ServerTransaction) {
		res := sip.NewResponseFromRequest(req, 603, "Declined", nil)
		tx.Respond(res)
	})
	uas.start(t)

	ctx := context.Background()

	call, err := pbx.SendInvite(ctx, uas.uasURI("alice"), SDP("127.0.0.1", 20000, PCMU))
	if err != nil {
		t.Fatalf("SendInvite: %v", err)
	}

	err = call.SendRefer(ctx, "sip:bob@192.168.1.100")
	if err == nil {
		t.Fatal("SendRefer should fail on rejection")
	}
	if !strings.Contains(err.Error(), "603") {
		t.Errorf("error = %q, want to contain '603'", err)
	}
}

func TestSendRefer_Inbound(t *testing.T) {
	pbx := NewFakePBX(t)

	referDone := make(chan error, 1)
	pbx.OnInvite(func(inv *Invite) {
		inv.Trying()
		inv.Ringing()
		ac := inv.Answer(SDP("127.0.0.1", 20000, PCMU))

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		referDone <- ac.SendRefer(ctx, "sip:charlie@10.0.0.1")
	})

	// Use testUAS as the caller — it has both client and server,
	// so in-dialog REFER from PBX can be received.
	caller := testUAS(t)
	caller.srv.OnRefer(func(req *sip.Request, tx sip.ServerTransaction) {
		res := sip.NewResponseFromRequest(req, 202, "Accepted", nil)
		tx.Respond(res)
	})
	caller.start(t)

	result := sendInviteWithContact(t, caller.cli, pbx.URI("alice"), SDP("127.0.0.1", 30000, PCMU), caller.addr)
	defer result.TX.Terminate()
	if result.Final.StatusCode != 200 {
		t.Fatalf("INVITE final = %d, want 200", result.Final.StatusCode)
	}

	if err := <-referDone; err != nil {
		t.Fatalf("SendRefer from ActiveCall: %v", err)
	}
}

// =============================================================================
// Phase 4: Out-of-Dialog MESSAGE and OPTIONS (UAC) Tests
// =============================================================================

func TestSendMessage_Basic(t *testing.T) {
	pbx := NewFakePBX(t)
	uas := testUAS(t)

	type msgResult struct {
		body        string
		contentType string
	}
	got := make(chan msgResult, 1)
	uas.srv.OnMessage(func(req *sip.Request, tx sip.ServerTransaction) {
		r := msgResult{body: string(req.Body())}
		if h := req.GetHeader("Content-Type"); h != nil {
			r.contentType = h.Value()
		}
		res := sip.NewResponseFromRequest(req, 200, "OK", nil)
		tx.Respond(res)
		got <- r
	})
	uas.start(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := pbx.SendMessage(ctx, uas.uasURI("alice"), "text/plain", []byte("Hello, Alice!"))
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	select {
	case r := <-got:
		if r.body != "Hello, Alice!" {
			t.Errorf("body = %q, want %q", r.body, "Hello, Alice!")
		}
		if r.contentType != "text/plain" {
			t.Errorf("Content-Type = %q, want text/plain", r.contentType)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("UAS did not receive MESSAGE")
	}
}

func TestSendMessage_Rejected(t *testing.T) {
	pbx := NewFakePBX(t)
	uas := testUAS(t)
	uas.srv.OnMessage(func(req *sip.Request, tx sip.ServerTransaction) {
		res := sip.NewResponseFromRequest(req, 403, "Forbidden", nil)
		tx.Respond(res)
	})
	uas.start(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := pbx.SendMessage(ctx, uas.uasURI("alice"), "text/plain", []byte("blocked"))
	if err == nil {
		t.Fatal("SendMessage should fail on rejection")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error = %q, want to contain '403'", err)
	}
}

func TestSendMessage_InvalidURI(t *testing.T) {
	pbx := NewFakePBX(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := pbx.SendMessage(ctx, "not-a-uri", "text/plain", []byte("test"))
	if err == nil {
		t.Fatal("SendMessage should fail on invalid URI")
	}
}

func TestSendOptions_Basic(t *testing.T) {
	pbx := NewFakePBX(t)
	uas := testUAS(t)
	uas.srv.OnOptions(func(req *sip.Request, tx sip.ServerTransaction) {
		res := sip.NewResponseFromRequest(req, 200, "OK", nil)
		res.AppendHeader(sip.NewHeader("Allow", "INVITE, ACK, BYE, CANCEL, OPTIONS"))
		tx.Respond(res)
	})
	uas.start(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res, err := pbx.SendOptions(ctx, uas.uasURI("alice"))
	if err != nil {
		t.Fatalf("SendOptions: %v", err)
	}
	if res.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", res.StatusCode)
	}
	if allow := res.GetHeader("Allow"); allow == nil {
		t.Error("response missing Allow header")
	} else if !strings.Contains(allow.Value(), "INVITE") {
		t.Errorf("Allow = %q, want to contain INVITE", allow.Value())
	}
}

func TestSendOptions_Rejected(t *testing.T) {
	pbx := NewFakePBX(t)
	uas := testUAS(t)
	uas.srv.OnOptions(func(req *sip.Request, tx sip.ServerTransaction) {
		res := sip.NewResponseFromRequest(req, 405, "Method Not Allowed", nil)
		tx.Respond(res)
	})
	uas.start(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res, err := pbx.SendOptions(ctx, uas.uasURI("alice"))
	if err != nil {
		t.Fatalf("SendOptions should not error on non-2xx: %v", err)
	}
	if res.StatusCode != 405 {
		t.Errorf("StatusCode = %d, want 405", res.StatusCode)
	}
}

func TestSendOptions_InvalidURI(t *testing.T) {
	pbx := NewFakePBX(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := pbx.SendOptions(ctx, "not-a-uri")
	if err == nil {
		t.Fatal("SendOptions should fail on invalid URI")
	}
}

// =============================================================================
// SDP test helpers
// =============================================================================

func assertSDPLine(t *testing.T, sdp, line string) {
	t.Helper()
	for _, l := range strings.Split(sdp, "\r\n") {
		if strings.TrimSpace(l) == line {
			return
		}
	}
	t.Errorf("SDP missing line %q\n--- SDP ---\n%s", line, sdp)
}

func assertSDPContains(t *testing.T, sdp, prefix, substr string) {
	t.Helper()
	for _, l := range strings.Split(sdp, "\r\n") {
		if strings.HasPrefix(l, prefix) && strings.Contains(l, substr) {
			return
		}
	}
	t.Errorf("SDP missing line with prefix %q containing %q\n--- SDP ---\n%s", prefix, substr, sdp)
}
