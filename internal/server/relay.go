package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/nrmnqdds/gomaluum/internal/constants"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// imaluumHost is the only host routed through the egress relay.
var imaluumHost = mustHost(constants.ImaluumPage)

func mustHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		panic(fmt.Sprintf("invalid i-Ma'luum URL constant %q: %v", rawURL, err))
	}
	return u.Hostname()
}

// relayTransport routes i-Ma'luum requests through one of several URL-rewriting
// relays (Cloudflare Workers listed in IMALUUM_PROXY_PREFIX) whose IPs — unlike
// our datacenter host — aren't blocked by IIUM. It prepends a prefix to the
// i-Ma'luum URL:
//
//	https://worker.dev/------https://imaluum.iium.edu.my/MyAcademic/schedule
//
// Requests are round-robined across the prefixes to spread load and gain some
// egress-IP diversity. Only i-Ma'luum requests are rewritten; the relay pre-wraps
// redirect Location headers, so redirect follow-ups already target the relay host
// and pass through this RoundTripper untouched (keeping a redirect chain on the
// relay that started it).
type relayTransport struct {
	prefixes []string
	next     atomic.Uint64
	base     http.RoundTripper
}

func (t *relayTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Hostname() != imaluumHost {
		return t.base.RoundTrip(req)
	}
	prefix := t.prefixes[t.next.Add(1)%uint64(len(t.prefixes))]
	relayed, err := url.Parse(prefix + req.URL.String())
	if err != nil {
		return nil, fmt.Errorf("building relay URL: %w", err)
	}

	// Record which relay handled the request: a trace-correlated debug log and a
	// span attribute, so the chosen egress is visible per request.
	slog.DebugContext(req.Context(), "i-Ma'luum request via relay",
		"relay", relayed.Host, "target", req.URL.String())
	trace.SpanFromContext(req.Context()).SetAttributes(
		attribute.String("imaluum.relay", relayed.Host))

	// Clone so the caller's request (which the http.Client may reuse for
	// redirects/retries) is not mutated.
	r := req.Clone(req.Context())
	r.URL = relayed
	r.Host = relayed.Host
	return t.base.RoundTrip(r)
}

// newImaluumRelay wraps base so i-Ma'luum requests are routed through the relay
// prefixes (a comma-separated list). Returns base unchanged when the list is
// empty (relay disabled).
func newImaluumRelay(rawPrefixes string, base http.RoundTripper) http.RoundTripper {
	prefixes := parseRelayPrefixes(rawPrefixes)
	if len(prefixes) == 0 {
		return base
	}
	return &relayTransport{prefixes: prefixes, base: base}
}

// parseRelayPrefixes splits a comma-separated prefix list, trimming blanks.
func parseRelayPrefixes(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
