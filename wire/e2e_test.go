package wire

import (
	"encoding/json"
	"strings"
	"testing"
)

// The end-to-end remote URLs (poolpilot-relay#80) are additive contract
// fields the cloud imports: pin their JSON names, and pin that every one is
// omitempty — an unset field must not appear on the wire, so the parity
// fixture round-trips unchanged and old clients see the old shape.
func TestE2EFieldsJSONNames(t *testing.T) {
	const ctrl, api = "https://g-e2e.remote.example", "https://g-api-e2e.remote.example"
	cases := []struct {
		name string
		in   any
		want []string
	}{
		{"ControllerConfigResponse", ControllerConfigResponse{RemoteE2EURL: ctrl, RemoteAPIE2EURL: api},
			[]string{`"remote_e2e_url":"` + ctrl + `"`, `"remote_api_e2e_url":"` + api + `"`}},
		{"ControllerInfo", ControllerInfo{RemoteE2EURL: ctrl, RemoteAPIE2EURL: api},
			[]string{`"remote_e2e_url":"` + ctrl + `"`, `"remote_api_e2e_url":"` + api + `"`}},
		{"CloudControllerInfo", CloudControllerInfo{RemoteE2EURL: ctrl, RemoteAPIE2EURL: api},
			[]string{`"remote_e2e_url":"` + ctrl + `"`, `"remote_api_e2e_url":"` + api + `"`}},
		{"ControllerStatus", ControllerStatus{RemoteE2EURL: ctrl, RemoteAPIE2EURL: api},
			[]string{`"remote_e2e_url":"` + ctrl + `"`, `"remote_api_e2e_url":"` + api + `"`}},
		{"WebSessionResponse", WebSessionResponse{SessionE2EURL: ctrl + "/__session?t=x"},
			[]string{`"session_e2e_url":"` + ctrl + `/__session?t=x"`}},
	}
	for _, tc := range cases {
		raw, err := json.Marshal(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range tc.want {
			if !strings.Contains(string(raw), w) {
				t.Errorf("%s: %s lacks %s", tc.name, raw, w)
			}
		}
	}
}

func TestE2EFieldsOmitEmpty(t *testing.T) {
	for name, in := range map[string]any{
		"ControllerConfigResponse": ControllerConfigResponse{},
		"ControllerInfo":           ControllerInfo{},
		"CloudControllerInfo":      CloudControllerInfo{},
		"ControllerStatus":         ControllerStatus{},
		"WebSessionResponse":       WebSessionResponse{},
	} {
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "e2e") {
			t.Errorf("%s: zero value leaks an e2e field: %s", name, raw)
		}
	}
}

// The label grammar the relay and the cloud's frps plugin share: GUIDs are
// 32 hex, so neither suffix can collide with a GUID, and "<guid>-api-e2e"
// stays one DNS label (40 chars).
func TestE2ESubdomainSuffixes(t *testing.T) {
	if E2ESubdomainSuffix != "-e2e" || APIE2ESubdomainSuffix != "-api-e2e" {
		t.Fatalf("suffixes = %q, %q", E2ESubdomainSuffix, APIE2ESubdomainSuffix)
	}
	if APIE2ESubdomainSuffix != APISubdomainSuffix+E2ESubdomainSuffix {
		t.Fatalf("api-e2e suffix must be the api suffix plus the e2e suffix")
	}
	if E2ECtrlProxyPrefix != "e2e-" || E2EAPIProxyPrefix != "e2eapi-" {
		t.Fatalf("proxy prefixes = %q, %q", E2ECtrlProxyPrefix, E2EAPIProxyPrefix)
	}
}
