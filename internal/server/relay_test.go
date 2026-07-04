package server

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeRoundTripper records the URL it was asked to send.
type fakeRoundTripper struct{ lastURL string }

func (f *fakeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	f.lastURL = r.URL.String()
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
}

func TestImaluumRelay(t *testing.T) {
	const prefix = "https://proxy.qaguradev.workers.dev/------"

	t.Run("empty prefix returns base unchanged", func(t *testing.T) {
		base := &fakeRoundTripper{}
		require.Same(t, base, newImaluumRelay("", base))
		require.Same(t, base, newImaluumRelay("  ,  ", base)) // only blanks
	})

	t.Run("round-robins across multiple prefixes", func(t *testing.T) {
		base := &fakeRoundTripper{}
		rt := newImaluumRelay("https://p1.dev/------, https://p2.dev/------ ,https://p3.dev/------", base)

		seen := map[string]bool{}
		for range 6 {
			req, _ := http.NewRequest("GET", "https://imaluum.iium.edu.my/Profile", nil)
			_, err := rt.RoundTrip(req)
			require.NoError(t, err)
			seen[base.lastURL] = true
		}
		require.Equal(t, map[string]bool{
			"https://p1.dev/------https://imaluum.iium.edu.my/Profile": true,
			"https://p2.dev/------https://imaluum.iium.edu.my/Profile": true,
			"https://p3.dev/------https://imaluum.iium.edu.my/Profile": true,
		}, seen)
	})

	t.Run("rewrites i-Ma'luum requests, preserving path and query", func(t *testing.T) {
		base := &fakeRoundTripper{}
		rt := newImaluumRelay(prefix, base)

		req, _ := http.NewRequest("GET", "https://imaluum.iium.edu.my/MyAcademic/schedule?ses=2024/2025&sem=1", nil)
		_, err := rt.RoundTrip(req)
		require.NoError(t, err)
		require.Equal(t, prefix+"https://imaluum.iium.edu.my/MyAcademic/schedule?ses=2024/2025&sem=1", base.lastURL)

		// The caller's request must not be mutated (the client reuses it on redirect).
		require.Equal(t, "imaluum.iium.edu.my", req.URL.Hostname())
	})

	t.Run("leaves non-i-Ma'luum hosts direct", func(t *testing.T) {
		base := &fakeRoundTripper{}
		rt := newImaluumRelay(prefix, base)

		req, _ := http.NewRequest("GET", "https://souq.iium.edu.my/embeded", nil)
		_, err := rt.RoundTrip(req)
		require.NoError(t, err)
		require.Equal(t, "https://souq.iium.edu.my/embeded", base.lastURL)
	})
}
