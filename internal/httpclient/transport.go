package httpclient

import "net/http"

// BearerTransport injects an Authorization header when Token is set.
type BearerTransport struct {
	Token string
	Base  http.RoundTripper
}

func (t *BearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.Token != "" {
		// RoundTripper contract forbids mutating the original request; clone first.
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+t.Token)
	}
	return t.Base.RoundTrip(r)
}
