package proxy

import (
	"mime"
	"net/http"
	"strings"
)

// isGRPCRequest reports whether r is native gRPC: HTTP/2 with an
// application/grpc or application/grpc+<codec> content type.
func isGRPCRequest(r *http.Request) bool {
	if r.ProtoMajor != 2 {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return false
	}
	return mediaType == "application/grpc" || strings.HasPrefix(mediaType, "application/grpc+")
}
