package api

// Version is the Engine API version this daemon implements.
// Docker uses semantic versions like "1.43"; we start at "1.0".
// Clients negotiate via GET /_ping, which returns this in the
// "Api-Version" header.
const Version = "1.0"

// MinVersion is the oldest API version this daemon will accept
// requests for. Docker lets old clients talk to new daemons by
// falling back to this value. For now we only support one version.
const MinVersion = "1.0"

// PingResponse is what GET /_ping returns.
//
// NOTE: Docker's /_ping returns the plain-text body "OK" and puts
// the interesting data in response headers (Api-Version, OSType,
// Docker-Experimental, etc.). We do the same. This struct exists
// for the few fields we want the client to access programmatically
// when we add GET /version in a later step.
type PingResponse struct {
	APIVersion string
	OSType     string
	Server     string

	// BuilderVersion advertises build support. Empty string means "this
	// daemon does not implement /build". A newer CLI reads this from the
	// ping response to decide whether to even show `mydocker build` in
	// --help.
	BuilderVersion string
}
