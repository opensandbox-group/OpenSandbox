package sandbox

// EndpointInfo is the single lookup result used by ingress routing.
type EndpointInfo struct {
	// Endpoint is the resolved upstream endpoint (IP/FQDN) for this sandbox.
	Endpoint string

	// SecureAccessToken is the trimmed annotation opensandbox.io/secure-access-token value.
	// Empty means secure access is not required.
	SecureAccessToken string
}

func (i EndpointInfo) AccessVerificationRequired() bool {
	return i.SecureAccessToken != ""
}
