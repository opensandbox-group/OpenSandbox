module github.com/alibaba/OpenSandbox/examples/client-pool-resize

go 1.20

require github.com/alibaba/OpenSandbox/sdks/sandbox/go v1.1.0

require golang.org/x/sync v0.7.0 // indirect

// The repository example tracks the SDK in this checkout. Remove this replace
// when using a released SDK version in an application.
replace github.com/alibaba/OpenSandbox/sdks/sandbox/go => ../../sdks/sandbox/go
