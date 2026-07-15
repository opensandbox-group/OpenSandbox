package proxy

import (
	"errors"
	"net/http"

	"github.com/alibaba/opensandbox/ingress/pkg/sandbox"
	"github.com/alibaba/opensandbox/ingress/pkg/signature"
)

func ingressRouteErrHTTPStatus(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, sandbox.ErrSandboxNotFound) {
		return http.StatusNotFound
	}
	return signature.HTTPStatusForIngressErr(err)
}

func providerErrHTTPStatus(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, sandbox.ErrSandboxNotFound) {
		return http.StatusNotFound
	}

	if errors.Is(err, sandbox.ErrSandboxNotReady) {
		return http.StatusServiceUnavailable
	}

	return http.StatusBadGateway
}
