// Package version carries the build version of the gateway.
//
// Release builds override Version with:
//
//	go build -ldflags "-X github.com/oyyyyy61/model-gateway/internal/version.Version=vX.Y.Z"
package version

// Version is the human readable gateway version reported by /metrics and
// /admin/status.
var Version = "0.2.0"
