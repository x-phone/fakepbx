package fakepbx

// ActiveCall is returned by [Invite.Answer].
// It lets tests simulate PBX-side mid-call actions (BYE, re-INVITE, NOTIFY).
type ActiveCall struct {
	dialogCall
}
