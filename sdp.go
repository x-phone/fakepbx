package fakepbx

import (
	"fmt"
	"strings"
)

// Codec represents an RTP codec for SDP generation.
type Codec struct {
	PayloadType int
	Name        string
	ClockRate   int
}

// Common codecs.
var (
	PCMU = Codec{PayloadType: 0, Name: "PCMU", ClockRate: 8000}
	PCMA = Codec{PayloadType: 8, Name: "PCMA", ClockRate: 8000}
	G722 = Codec{PayloadType: 9, Name: "G722", ClockRate: 8000}
)

// SDP returns a minimal valid SDP body for test responses.
// If no codecs are specified, defaults to PCMU.
func SDP(ip string, port int, codecs ...Codec) []byte {
	return SDPWithDirection(ip, port, "", codecs...)
}

// SDPWithDirection returns SDP with a specific media direction attribute.
// Direction should be "sendonly", "recvonly", "sendrecv", or "" for no direction line.
func SDPWithDirection(ip string, port int, direction string, codecs ...Codec) []byte {
	if len(codecs) == 0 {
		codecs = []Codec{PCMU}
	}

	// Build payload type list for m= line
	pts := make([]string, len(codecs))
	for i, c := range codecs {
		pts[i] = fmt.Sprintf("%d", c.PayloadType)
	}

	var b strings.Builder
	b.WriteString("v=0\r\n")
	b.WriteString(fmt.Sprintf("o=- 0 0 IN IP4 %s\r\n", ip))
	b.WriteString("s=fakepbx\r\n")
	b.WriteString(fmt.Sprintf("c=IN IP4 %s\r\n", ip))
	b.WriteString("t=0 0\r\n")
	b.WriteString(fmt.Sprintf("m=audio %d RTP/AVP %s\r\n", port, strings.Join(pts, " ")))

	for _, c := range codecs {
		b.WriteString(fmt.Sprintf("a=rtpmap:%d %s/%d\r\n", c.PayloadType, c.Name, c.ClockRate))
	}

	if direction != "" {
		b.WriteString(fmt.Sprintf("a=%s\r\n", direction))
	}

	return []byte(b.String())
}
