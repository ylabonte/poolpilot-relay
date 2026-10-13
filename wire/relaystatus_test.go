package wire

import (
	"encoding/json"
	"testing"
)

// The POST /relay/status body (poolpilot-cloud#242) is a contract with the
// cloud, which imports this type: pin its JSON names and the omitempty reason.
func TestRelayStatusResponseJSON(t *testing.T) {
	cases := []struct {
		in   RelayStatusResponse
		want string
	}{
		{RelayStatusResponse{State: RelayStateActive}, `{"state":"active"}`},
		{RelayStatusResponse{State: RelayStateSuspended, Reason: RelayReasonLapsed}, `{"state":"suspended","reason":"lapsed"}`},
		{RelayStatusResponse{State: RelayStateSuspended, Reason: RelayReasonFree}, `{"state":"suspended","reason":"free"}`},
		{RelayStatusResponse{State: RelayStateSuspended, Reason: RelayReasonRevoked}, `{"state":"suspended","reason":"revoked"}`},
		{RelayStatusResponse{State: RelayStateDeleted, Reason: RelayReasonHouseholdDeleted}, `{"state":"deleted","reason":"household_deleted"}`},
	}
	for _, tc := range cases {
		raw, err := json.Marshal(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != tc.want {
			t.Errorf("marshal %+v = %s, want %s", tc.in, raw, tc.want)
		}
		var back RelayStatusResponse
		if err := json.Unmarshal(raw, &back); err != nil || back != tc.in {
			t.Errorf("round trip %s = %+v (%v)", raw, back, err)
		}
	}
}
