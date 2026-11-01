package wire

import (
	"encoding/json"
	"testing"
)

// The device-link fields are additive and optional: an empty value must be
// ABSENT on the wire, so an older cloud or app sees the exact body it always
// did, and a set value must round-trip under its documented key.
func TestDeviceLinkFieldsAreOmitEmptyAndRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		empty   any
		emptyIs string
		full    any
		fullIs  string
		decode  func([]byte) (any, error)
	}{
		{
			name:    "DeviceVoucherRequest",
			empty:   DeviceVoucherRequest{InviteCode: "INV1-CODE"},
			emptyIs: `{"invite_code":"INV1-CODE"}`,
			full:    DeviceVoucherRequest{Recovery: true, DeviceID: "dev-1"},
			fullIs:  `{"recovery":true,"device_id":"dev-1"}`,
			decode: func(b []byte) (any, error) {
				var v DeviceVoucherRequest
				err := json.Unmarshal(b, &v)
				return v, err
			},
		},
		{
			name:    "EnrollRedeemRequest",
			empty:   EnrollRedeemRequest{Code: "AAAA-BBBB", AgentID: "agent-1"},
			emptyIs: `{"code":"AAAA-BBBB","agent_id":"agent-1"}`,
			full:    EnrollRedeemRequest{Code: "AAAA-BBBB", AgentID: "agent-1", DeviceID: "dev-1"},
			fullIs:  `{"code":"AAAA-BBBB","agent_id":"agent-1","device_id":"dev-1"}`,
			decode: func(b []byte) (any, error) {
				var v EnrollRedeemRequest
				err := json.Unmarshal(b, &v)
				return v, err
			},
		},
		{
			name:    "MemberDeviceInfo",
			empty:   MemberDeviceInfo{BearerID: "b1", CreatedAt: "2026-11-01T00:00:00Z"},
			emptyIs: `{"bearer_id":"b1","created_at":"2026-11-01T00:00:00Z","current":false}`,
			full:    MemberDeviceInfo{BearerID: "b1", CreatedAt: "2026-11-01T00:00:00Z", RelayID: "r1", RelayDeviceID: "dev-1"},
			fullIs:  `{"bearer_id":"b1","created_at":"2026-11-01T00:00:00Z","current":false,"relay_id":"r1","relay_device_id":"dev-1"}`,
			decode: func(b []byte) (any, error) {
				var v MemberDeviceInfo
				err := json.Unmarshal(b, &v)
				return v, err
			},
		},
		{
			name:    "AppBearerRelayDeviceRequest",
			empty:   AppBearerRelayDeviceRequest{},
			emptyIs: `{"relay_device_id":""}`, // required field: not omitempty
			full:    AppBearerRelayDeviceRequest{RelayDeviceID: "dev-1"},
			fullIs:  `{"relay_device_id":"dev-1"}`,
			decode: func(b []byte) (any, error) {
				var v AppBearerRelayDeviceRequest
				err := json.Unmarshal(b, &v)
				return v, err
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, tc := range []struct {
				v    any
				want string
			}{{c.empty, c.emptyIs}, {c.full, c.fullIs}} {
				b, err := json.Marshal(tc.v)
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				if string(b) != tc.want {
					t.Errorf("marshal = %s, want %s", b, tc.want)
				}
				back, err := c.decode(b)
				if err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if back != tc.v {
					t.Errorf("round trip = %+v, want %+v", back, tc.v)
				}
			}
		})
	}
}
