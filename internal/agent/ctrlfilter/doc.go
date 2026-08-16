// Package ctrlfilter is the relay's tunnel-facing gate in front of a pool
// controller. It terminates the frp ctrl-<GUID> proxy: the proxy's LocalAddr
// points at a Server (a local net/http server, not the controller itself),
// which resolves the controller from the Host header, requires a paired-app
// credential (issue #27 — a leaked tunnel GUID alone is not enough), strips
// the relay's own credentials, and reverse-proxies the request to the
// controller's real LAN address.
//
// Authenticated callers get FULL read+write access to the controller
// (transparent proxy). Remote access is app-only and already gated to paired
// devices: the session cookie is mintable only with the pairing bearer, and
// the bearer only comes from the LAN pairing ceremony — so anything that
// clears the credential gate is a device the owner deliberately paired, and is
// granted the same access it has on the LAN. The earlier "view but don't
// touch" write deny-list (issue #27's interim measure) was removed by owner
// decision: it also blocked the owner's own paired app, and the app-paired
// gate — not a per-path allow/deny list — is the intended control point. The
// canonical-path hardening (canonical.go), the credential gate (server.go),
// and credential stripping all stay.
package ctrlfilter
