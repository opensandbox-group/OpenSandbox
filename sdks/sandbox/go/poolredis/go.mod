module github.com/alibaba/OpenSandbox/sdks/sandbox/go/poolredis

go 1.20

require (
	github.com/alibaba/OpenSandbox/sdks/sandbox/go v0.0.0
	github.com/redis/go-redis/v9 v9.7.3
)

require (
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	golang.org/x/sync v0.7.0 // indirect
)

replace github.com/alibaba/OpenSandbox/sdks/sandbox/go => ../
