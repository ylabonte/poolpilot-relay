package useragent

import (
	"net/http"
	"testing"
)

func TestSetVersionFormatsTheUserAgent(t *testing.T) {
	t.Cleanup(func() { SetVersion("") })
	cases := []struct{ in, want string }{
		{"v0.10.0", "PoolPilot-Relay/0.10.0 (+https://poolpilot.eu)"},
		{"v0.11.0-rc.1", "PoolPilot-Relay/0.11.0-rc.1 (+https://poolpilot.eu)"},
		{"1.2.3", "PoolPilot-Relay/1.2.3 (+https://poolpilot.eu)"},
		{"dev", "PoolPilot-Relay/dev (+https://poolpilot.eu)"},
		{"", "PoolPilot-Relay/dev (+https://poolpilot.eu)"},
		{"  v1.0.0 ", "PoolPilot-Relay/1.0.0 (+https://poolpilot.eu)"},
		// Not a valid HTTP token — must never corrupt the header.
		{"1.0 beta", "PoolPilot-Relay/dev (+https://poolpilot.eu)"},
		{"1.0\r\nX-Evil: 1", "PoolPilot-Relay/dev (+https://poolpilot.eu)"},
	}
	for _, c := range cases {
		SetVersion(c.in)
		if got := String(); got != c.want {
			t.Errorf("SetVersion(%q): String() = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDefaultIsDevBeforeSetVersion(t *testing.T) {
	// init() seeds "dev"; restore that state explicitly in case another test ran first.
	SetVersion("")
	if got, want := String(), "PoolPilot-Relay/dev (+https://poolpilot.eu)"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestSetOverridesAnyUserAgent(t *testing.T) {
	h := http.Header{}
	h.Set("User-Agent", "Mozilla/5.0")
	Set(h)
	if got := h.Get("User-Agent"); got != String() {
		t.Errorf("Set: User-Agent = %q, want %q", got, String())
	}
}

func TestSetDefaultKeepsARealClientUserAgent(t *testing.T) {
	cases := []struct {
		name    string
		present bool
		in      string
		want    string // "" = expect the relay's UA
	}{
		{"absent", false, "", ""},
		{"empty", true, "", ""},
		{"blank", true, "   ", ""},
		{"go default", true, "Go-http-client/1.1", ""},
		{"go default h2", true, "Go-http-client/2.0", ""},
		{"webview", true, "Mozilla/5.0 (Linux; Android 15) AppleWebKit/537.36", "Mozilla/5.0 (Linux; Android 15) AppleWebKit/537.36"},
		{"okhttp", true, "okhttp/5.1.0", "okhttp/5.1.0"},
		{"already ours", true, "PoolPilot-Relay/0.9.0 (+https://poolpilot.eu)", "PoolPilot-Relay/0.9.0 (+https://poolpilot.eu)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := http.Header{}
			if c.present {
				h["User-Agent"] = []string{c.in}
			}
			SetDefault(h)
			want := c.want
			if want == "" {
				want = String()
			}
			if got := h.Get("User-Agent"); got != want {
				t.Errorf("SetDefault(%q): User-Agent = %q, want %q", c.in, got, want)
			}
		})
	}
}
