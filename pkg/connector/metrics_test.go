package connector

import (
	"testing"

	dto "github.com/prometheus/client_model/go"
	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/elsbrock/mautrix-chatto/pkg/bridgemetrics"
)

func TestLoginGaugeIsPerServer(t *testing.T) {
	c := &Connector{Config: Config{Servers: []ServerConfig{{ID: "lg-a"}, {ID: "lg-b"}}}}
	logins := func(server string, state status.BridgeStateEvent) float64 {
		var m dto.Metric
		if err := bridgemetrics.Logins.WithLabelValues(server, string(state)).Write(&m); err != nil {
			t.Fatal(err)
		}
		return m.GetGauge().GetValue()
	}

	c.recordLoginState("lg-a", "a1", status.StateConnected)
	c.recordLoginState("lg-a", "a2", status.StateBadCredentials)
	c.recordLoginState("lg-b", "b1", status.StateConnected)
	if got := logins("lg-a", status.StateConnected); got != 1 {
		t.Errorf("lg-a connected = %v, want 1", got)
	}
	if got := logins("lg-a", status.StateBadCredentials); got != 1 {
		t.Errorf("lg-a bad credentials = %v, want 1", got)
	}
	if got := logins("lg-b", status.StateConnected); got != 1 {
		t.Errorf("lg-b connected = %v, want 1", got)
	}

	// Removing a server's last login zeroes its series.
	c.recordLoginState("lg-b", "b1", "")
	if got := logins("lg-b", status.StateConnected); got != 0 {
		t.Errorf("lg-b connected after logout = %v, want 0", got)
	}
}
