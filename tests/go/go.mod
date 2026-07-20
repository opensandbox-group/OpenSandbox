module github.com/alibaba/OpenSandbox/tests/go

go 1.20

require (
	github.com/alibaba/OpenSandbox/sdks/sandbox/go v0.0.0
	github.com/alibaba/OpenSandbox/sdks/sandbox/go/poolredis v0.0.0
	github.com/redis/go-redis/v9 v9.7.3
	github.com/stretchr/testify v1.11.1
)

require (
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	golang.org/x/sync v0.7.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/alibaba/OpenSandbox/sdks/sandbox/go => ../../sdks/sandbox/go

replace github.com/alibaba/OpenSandbox/sdks/sandbox/go/poolredis => ../../sdks/sandbox/go/poolredis
