package chattoid

import (
	"testing"

	"maunium.net/go/mautrix/bridgev2/networkid"
)

func TestIDsRoundTrip(t *testing.T) {
	if srv, uid := ParseUserID(MakeUserID("home", "Uabc")); srv != "home" || uid != "Uabc" {
		t.Errorf("user ID: %q %q", srv, uid)
	}
	if srv, uid := ParseUserLoginID(MakeUserLoginID("home", "Uabc")); srv != "home" || uid != "Uabc" {
		t.Errorf("login ID: %q %q", srv, uid)
	}
	if srv, eid := ParseMessageID(MakeMessageID("home", "Eabc")); srv != "home" || eid != "Eabc" {
		t.Errorf("message ID: %q %q", srv, eid)
	}
	for _, tc := range []struct {
		id       networkid.PortalID
		srv      string
		kind     PortalKind
		chattoID string
	}{
		{MakePortalID("home", "Rabc"), "home", PortalRoom, "Rabc"},
		{MakeGroupPortalID("home", "Gabc"), "home", PortalGroup, "Gabc"},
		{"Rabc", "", PortalInvalid, ""},
		{"home.", "", PortalInvalid, ""},
		{".Rabc", "", PortalInvalid, ""},
		{"home.group:", "", PortalInvalid, ""},
	} {
		srv, kind, chattoID := ParsePortalID(tc.id)
		if srv != tc.srv || kind != tc.kind || chattoID != tc.chattoID {
			t.Errorf("ParsePortalID(%q) = %q %v %q", tc.id, srv, kind, chattoID)
		}
	}
}

func TestUnprefixedIDsAreRejected(t *testing.T) {
	if srv, uid := ParseUserID("Uabc"); srv != "" || uid != "" {
		t.Errorf("bare user ID parsed as %q %q", srv, uid)
	}
	if srv, eid := ParseMessageID("Eabc"); srv != "" || eid != "" {
		t.Errorf("bare message ID parsed as %q %q", srv, eid)
	}
}

func TestNoticeIDs(t *testing.T) {
	id := MakeNoticeMessageID("home", "Rabc", "123")
	if !IsNoticeMessageID(id) {
		t.Fatalf("%q is not a notice", id)
	}
	if srv, eid := ParseMessageID(id); srv != "" || eid != "" {
		t.Errorf("notice parsed as Chatto message %q %q", srv, eid)
	}
	if IsNoticeMessageID(MakeMessageID("home", "Eabc")) {
		t.Error("a Chatto message is a notice")
	}
}
