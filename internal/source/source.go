package source

import "context"

// Video is the small normalized input that a remote source adapter provides
// to the acquisition pipeline. URL is intentionally short-lived: consumers
// must not persist it or expose it as a public API field.
type Video struct {
	URL         string
	Name        string
	Size        int64
	Fingerprint string
	Metadata    map[string]any
}

// Resolver is the seam between acquisition and a remote file provider. The
// processor receives a short-lived URL and wraps it in a RangeReader. The
// entire video does not need to be copied locally.
type Resolver interface {
	ResolveVideo(context.Context, string, string) (Video, error)
}
