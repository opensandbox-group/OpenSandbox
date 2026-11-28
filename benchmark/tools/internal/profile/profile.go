// Copyright 2026 The OpenSandbox Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package profile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/profiles"
)

const (
	APIVersionV1Alpha1 = "bench.opensandbox.io/v1alpha1"
	KindBenchmark      = "BenchmarkProfile"

	RuntimeFastSandboxOverlayBD = "fast-sandbox-overlaybd"
	PoolModeResourceAware       = "ResourceAware"
	PoolModeLegacySlot          = "LegacySlot"

	DefaultProfileName      = "fast-sandbox-overlaybd-ref"
	DefaultRegistryHostPath = "/var/lib/opensandbox-bench/registry"
	DefaultPoolNetworkCIDR  = "172.16.0.0/20"
)

// BenchmarkProfile defines the full-stack environment, runtime configuration,
// and workload suite parameters for an opensandbox-bench run.
type BenchmarkProfile struct {
	APIVersion  string          `yaml:"apiVersion" json:"apiVersion"`
	Kind        string          `yaml:"kind" json:"kind"`
	Metadata    Metadata        `yaml:"metadata" json:"metadata"`
	Runtime     string          `yaml:"runtime,omitempty" json:"runtime,omitempty"`
	Environment EnvironmentSpec `yaml:"environment" json:"environment"`
	Stack       StackSpec       `yaml:"stack" json:"stack"`
	Suites      SuitesSpec      `yaml:"suites" json:"suites"`
	Extensions  map[string]any  `yaml:"extensions,omitempty" json:"extensions,omitempty"`
}

type Metadata struct {
	Name   string            `yaml:"name" json:"name"`
	Labels map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
}

type EnvironmentSpec struct {
	SystemNamespace     string       `yaml:"systemNamespace" json:"systemNamespace"`
	ResourceNamespace   string       `yaml:"resourceNamespace" json:"resourceNamespace"`
	BenchNamespace      string       `yaml:"benchNamespace" json:"benchNamespace"`
	PrepareNodes        bool         `yaml:"prepareNodes" json:"prepareNodes"`
	Runner              RunnerSpec   `yaml:"runner" json:"runner"`
	ImageRegistryMirror string       `yaml:"imageRegistryMirror,omitempty" json:"imageRegistryMirror,omitempty"`
	Registry            RegistrySpec `yaml:"registry" json:"registry"`
	Redis                 RedisSpec    `yaml:"redis" json:"redis"`
	Tracing               TracingSpec  `yaml:"tracing" json:"tracing"`
	LeakAuditGraceSeconds *int         `yaml:"leakAuditGraceSeconds,omitempty" json:"leakAuditGraceSeconds,omitempty"`
}

type RunnerSpec struct {
	BaseImage   string `yaml:"baseImage" json:"baseImage"`
	CPULimit    string `yaml:"cpuLimit" json:"cpuLimit"`
	MemoryLimit string `yaml:"memoryLimit" json:"memoryLimit"`
}

type RegistrySpec struct {
	EphemeralInCluster bool   `yaml:"ephemeralInCluster" json:"ephemeralInCluster"`
	Image              string `yaml:"image" json:"image"`
	StorageSize        string `yaml:"storageSize" json:"storageSize"`
	StorageClass       string `yaml:"storageClass,omitempty" json:"storageClass,omitempty"`
	HostPath           string `yaml:"hostPath,omitempty" json:"hostPath,omitempty"`
}

type RedisSpec struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
}

type TracingSpec struct {
	InCluster         bool    `yaml:"inCluster" json:"inCluster"`
	Backend           string  `yaml:"backend" json:"backend"` // jaeger | tempo
	Endpoint          string  `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	Sampler           string  `yaml:"sampler" json:"sampler"`
	DefaultSamplerArg float64 `yaml:"defaultSamplerArg" json:"defaultSamplerArg"`
}

type StackSpec struct {
	ChartPath   string          `yaml:"chartPath" json:"chartPath"`
	FastSandbox FastSandboxSpec `yaml:"fastSandbox" json:"fastSandbox"`
	Template    TemplateSpec    `yaml:"template" json:"template"`
	HelmValues  map[string]any  `yaml:"helmValues,omitempty" json:"helmValues,omitempty"`
}

type FastSandboxSpec struct {
	Runtime FastSandboxRuntimeSpec `yaml:"runtime" json:"runtime"`
	Pool    FastSandboxPoolSpec    `yaml:"pool" json:"pool"`
}

type FastSandboxRuntimeSpec struct {
	OCI OCIRuntimeSpec `yaml:"oci" json:"oci"`
}

type OCIRuntimeSpec struct {
	Enabled     bool `yaml:"enabled" json:"enabled"`
	CacheSizeGB int  `yaml:"cacheSizeGB" json:"cacheSizeGB"`
	PoolLow     int  `yaml:"poolLow" json:"poolLow"`
	PoolHigh    int  `yaml:"poolHigh" json:"poolHigh"`
	PoolSizeGB  int  `yaml:"poolSizeGB" json:"poolSizeGB"`
}

type ResourceQuantitiesSpec struct {
	CPU    string `yaml:"cpu,omitempty" json:"cpu,omitempty"`
	Memory string `yaml:"memory,omitempty" json:"memory,omitempty"`
}

type ContainerResourcesSpec struct {
	Requests ResourceQuantitiesSpec `yaml:"requests,omitempty" json:"requests,omitempty"`
	Limits   ResourceQuantitiesSpec `yaml:"limits,omitempty" json:"limits,omitempty"`
}

type FastSandboxPoolSpec struct {
	Mode                  string                  `yaml:"mode" json:"mode"`
	MinFastlets           int                     `yaml:"minFastlets" json:"minFastlets"`
	MaxFastlets           int                     `yaml:"maxFastlets" json:"maxFastlets"`
	MaxSandboxesPerPod    int                     `yaml:"maxSandboxesPerPod,omitempty" json:"maxSandboxesPerPod,omitempty"`
	OvercommitRatio       OvercommitRatioSpec     `yaml:"overcommitRatio" json:"overcommitRatio"`
	EgressEnabled         bool                    `yaml:"egressEnabled" json:"egressEnabled"`
	EgressResources       *ContainerResourcesSpec `yaml:"egressResources,omitempty" json:"egressResources,omitempty"`
	FastletProxyResources *ContainerResourcesSpec `yaml:"fastletProxyResources,omitempty" json:"fastletProxyResources,omitempty"`
	PodAntiAffinity       *bool                   `yaml:"podAntiAffinity,omitempty" json:"podAntiAffinity,omitempty"`
	SpillDir              string                  `yaml:"spillDir,omitempty" json:"spillDir,omitempty"`
	BlobCacheMaxBytes     int64                   `yaml:"blobCacheMaxBytes,omitempty" json:"blobCacheMaxBytes,omitempty"`
	FastletImage          string                  `yaml:"fastletImage,omitempty" json:"fastletImage,omitempty"`
	EgressImage           string                  `yaml:"egressImage,omitempty" json:"egressImage,omitempty"`
	NetworkCIDR           string                  `yaml:"networkCIDR,omitempty" json:"networkCIDR,omitempty"`
	ImagePullSecrets      []string                `yaml:"imagePullSecrets,omitempty" json:"imagePullSecrets,omitempty"`
}

type OvercommitRatioSpec struct {
	CPU    float64 `yaml:"cpu" json:"cpu"`
	Memory float64 `yaml:"memory" json:"memory"`
}

type TemplateSpec struct {
	BaseImage        string `yaml:"baseImage" json:"baseImage"`
	ExecdImage       string `yaml:"execdImage,omitempty" json:"execdImage,omitempty"`
	Publish          string `yaml:"publish,omitempty" json:"publish,omitempty"`
	PublishSecretRef string `yaml:"publishSecretRef,omitempty" json:"publishSecretRef,omitempty"`
	VCPUs            int    `yaml:"vcpus" json:"vcpus"`
	MemoryMiB        int    `yaml:"memoryMiB" json:"memoryMiB"`
	DiskGiB          int    `yaml:"diskGiB" json:"diskGiB"`
}

type SuitesSpec struct {
	ScheduleBurst       *ScheduleBurstSuiteSpec       `yaml:"scheduleBurst,omitempty" json:"scheduleBurst,omitempty"`
	SteadyChurn         *SteadyChurnSuiteSpec         `yaml:"steadyChurn,omitempty" json:"steadyChurn,omitempty"`
	PauseResume         *PauseResumeSuiteSpec         `yaml:"pauseResume,omitempty" json:"pauseResume,omitempty"`
	RootfsOnlyRestore   *RootfsOnlyRestoreSuiteSpec   `yaml:"rootfsOnlyRestore,omitempty" json:"rootfsOnlyRestore,omitempty"`
	WakeStorm           *WakeStormSuiteSpec           `yaml:"wakeStorm,omitempty" json:"wakeStorm,omitempty"`
	HighDensitySoak     *HighDensitySoakSuiteSpec     `yaml:"highDensitySoak,omitempty" json:"highDensitySoak,omitempty"`
	DensityRamp         *DensityRampSuiteSpec         `yaml:"densityRamp,omitempty" json:"densityRamp,omitempty"`
	DataplaneMicrobench *DataplaneMicrobenchSuiteSpec `yaml:"dataplaneMicrobench,omitempty" json:"dataplaneMicrobench,omitempty"`
	NoisyNeighbor       *NoisyNeighborSuiteSpec       `yaml:"noisyNeighbor,omitempty" json:"noisyNeighbor,omitempty"`
	RolloutDisruption   *RolloutDisruptionSuiteSpec   `yaml:"rolloutDisruption,omitempty" json:"rolloutDisruption,omitempty"`
	Custom              map[string]map[string]any     `yaml:"custom,omitempty" json:"custom,omitempty"`
}

type ScheduleBurstSuiteSpec struct {
	Concurrencies    []int    `yaml:"concurrencies" json:"concurrencies"`
	AttemptsPerLevel int      `yaml:"attemptsPerLevel" json:"attemptsPerLevel"`
	ReadinessPollMs  int      `yaml:"readinessPollMs" json:"readinessPollMs"`
	TraceSamplerArg  *float64 `yaml:"traceSamplerArg,omitempty" json:"traceSamplerArg,omitempty"`
}

type SteadyChurnSuiteSpec struct {
	TargetRPS       float64  `yaml:"targetRps" json:"targetRps"`
	DurationSeconds int      `yaml:"durationSeconds" json:"durationSeconds"`
	ReadinessPollMs int      `yaml:"readinessPollMs,omitempty" json:"readinessPollMs,omitempty"`
	TraceSamplerArg *float64 `yaml:"traceSamplerArg,omitempty" json:"traceSamplerArg,omitempty"`
}

type PauseResumeSuiteSpec struct {
	Modes                        []string `yaml:"modes" json:"modes"`
	DirtyUpperMiB                []int    `yaml:"dirtyUpperMiB" json:"dirtyUpperMiB"`
	ChainSquashRounds            int      `yaml:"chainSquashRounds" json:"chainSquashRounds"`
	ChainSquashTemplateMemoryMiB int      `yaml:"chainSquashTemplateMemoryMiB" json:"chainSquashTemplateMemoryMiB"`
	TraceSamplerArg              *float64 `yaml:"traceSamplerArg,omitempty" json:"traceSamplerArg,omitempty"`
}

type ResourceOverrideSpec struct {
	VCPUs      int      `yaml:"vcpus" json:"vcpus"`
	MemoryMiB  int      `yaml:"memoryMiB" json:"memoryMiB"`
	Entrypoint []string `yaml:"entrypoint,omitempty" json:"entrypoint,omitempty"`
}

type RootfsOnlyRestoreSuiteSpec struct {
	DirtyUpperMiB       []int                  `yaml:"dirtyUpperMiB" json:"dirtyUpperMiB"`
	FanoutConcurrencies []int                  `yaml:"fanoutConcurrencies" json:"fanoutConcurrencies"`
	CrossNode           bool                   `yaml:"crossNode" json:"crossNode"`
	EvictCacheForCold   *bool                  `yaml:"evictCacheForCold,omitempty" json:"evictCacheForCold,omitempty"`
	ResourceOverrides   []ResourceOverrideSpec `yaml:"resourceOverrides,omitempty" json:"resourceOverrides,omitempty"`
	TraceSamplerArg     *float64               `yaml:"traceSamplerArg,omitempty" json:"traceSamplerArg,omitempty"`
}

type WakeStormSuiteSpec struct {
	FleetSize            int      `yaml:"fleetSize" json:"fleetSize"`
	IdleThresholdSeconds int      `yaml:"idleThresholdSeconds" json:"idleThresholdSeconds"`
	FanoutConcurrency    int      `yaml:"fanoutConcurrency" json:"fanoutConcurrency"`
	SingleflightBurst    int      `yaml:"singleflightBurst" json:"singleflightBurst"`
	TraceSamplerArg      *float64 `yaml:"traceSamplerArg,omitempty" json:"traceSamplerArg,omitempty"`
}

type HighDensitySoakSuiteSpec struct {
	OversellTiers          []float64 `yaml:"oversellTiers" json:"oversellTiers"`
	DurationSecondsPerTier int       `yaml:"durationSecondsPerTier" json:"durationSecondsPerTier"`
	MeasureRecovery        bool      `yaml:"measureRecovery" json:"measureRecovery"`
	MinStateRootFreeGiB    int       `yaml:"minStateRootFreeGiB" json:"minStateRootFreeGiB"`
	SamplerIntervalSeconds int       `yaml:"samplerIntervalSeconds" json:"samplerIntervalSeconds"`
	NodeMemoryMiB          int       `yaml:"nodeMemoryMiB,omitempty" json:"nodeMemoryMiB,omitempty"`
	EgressProbeURL         string    `yaml:"egressProbeUrl,omitempty" json:"egressProbeUrl,omitempty"`
	TraceSamplerArg        *float64  `yaml:"traceSamplerArg,omitempty" json:"traceSamplerArg,omitempty"`
}

type DensityRampSuiteSpec struct {
	Steps                  []int    `yaml:"steps" json:"steps"`
	BatchCreateConcurrency int      `yaml:"batchCreateConcurrency,omitempty" json:"batchCreateConcurrency,omitempty"`
	MaxBatchFailureRate    float64  `yaml:"maxBatchFailureRate,omitempty" json:"maxBatchFailureRate,omitempty"`
	PingSampleSize         int      `yaml:"pingSampleSize,omitempty" json:"pingSampleSize,omitempty"`
	ReadinessPollMs        int      `yaml:"readinessPollMs,omitempty" json:"readinessPollMs,omitempty"`
	SandboxTimeoutSeconds  int      `yaml:"sandboxTimeoutSeconds,omitempty" json:"sandboxTimeoutSeconds,omitempty"`
	TraceSamplerArg        *float64 `yaml:"traceSamplerArg,omitempty" json:"traceSamplerArg,omitempty"`
}

type DataplaneMicrobenchSuiteSpec struct {
	ConcurrentSandboxes   int      `yaml:"concurrentSandboxes,omitempty" json:"concurrentSandboxes,omitempty"`
	CPUSeconds            int      `yaml:"cpuSeconds,omitempty" json:"cpuSeconds,omitempty"`
	StorageFileSizeMiB    int      `yaml:"storageFileSizeMiB,omitempty" json:"storageFileSizeMiB,omitempty"`
	NetworkTargetURL      string   `yaml:"networkTargetUrl,omitempty" json:"networkTargetUrl,omitempty"`
	Iperf3Server          string   `yaml:"iperf3Server,omitempty" json:"iperf3Server,omitempty"`
	NetworkRequests       int      `yaml:"networkRequests,omitempty" json:"networkRequests,omitempty"`
	NetworkConcurrency    int      `yaml:"networkConcurrency,omitempty" json:"networkConcurrency,omitempty"`
	ReadinessPollMs       int      `yaml:"readinessPollMs,omitempty" json:"readinessPollMs,omitempty"`
	SandboxTimeoutSeconds int      `yaml:"sandboxTimeoutSeconds,omitempty" json:"sandboxTimeoutSeconds,omitempty"`
	TraceSamplerArg       *float64 `yaml:"traceSamplerArg,omitempty" json:"traceSamplerArg,omitempty"`
}

type NoisyNeighborSuiteSpec struct {
	FleetSize             int      `yaml:"fleetSize,omitempty" json:"fleetSize,omitempty"`
	AggressorRatio        float64  `yaml:"aggressorRatio,omitempty" json:"aggressorRatio,omitempty"`
	ProbeRounds           int      `yaml:"probeRounds,omitempty" json:"probeRounds,omitempty"`
	StressModes           []string `yaml:"stressModes,omitempty" json:"stressModes,omitempty"`
	StressDurationSeconds int      `yaml:"stressDurationSeconds,omitempty" json:"stressDurationSeconds,omitempty"`
	ReadinessPollMs       int      `yaml:"readinessPollMs,omitempty" json:"readinessPollMs,omitempty"`
	SandboxTimeoutSeconds int      `yaml:"sandboxTimeoutSeconds,omitempty" json:"sandboxTimeoutSeconds,omitempty"`
	TraceSamplerArg       *float64 `yaml:"traceSamplerArg,omitempty" json:"traceSamplerArg,omitempty"`
}

type RolloutDisruptionSuiteSpec struct {
	Scenarios               []string `yaml:"scenarios,omitempty" json:"scenarios,omitempty"`
	TargetRPS               float64  `yaml:"targetRps,omitempty" json:"targetRps,omitempty"`
	BaselineSeconds         int      `yaml:"baselineSeconds,omitempty" json:"baselineSeconds,omitempty"`
	PostRolloutSeconds      int      `yaml:"postRolloutSeconds,omitempty" json:"postRolloutSeconds,omitempty"`
	RolloutTimeoutSeconds   int      `yaml:"rolloutTimeoutSeconds,omitempty" json:"rolloutTimeoutSeconds,omitempty"`
	ColocationHoldSeconds   int      `yaml:"colocationHoldSeconds,omitempty" json:"colocationHoldSeconds,omitempty"`
	SurvivorCount           int      `yaml:"survivorCount,omitempty" json:"survivorCount,omitempty"`
	SurvivorProbeIntervalMs int      `yaml:"survivorProbeIntervalMs,omitempty" json:"survivorProbeIntervalMs,omitempty"`
	PoolName                string   `yaml:"poolName,omitempty" json:"poolName,omitempty"`
	DaemonSetName           string   `yaml:"daemonSetName,omitempty" json:"daemonSetName,omitempty"`
	ReadinessPollMs         int      `yaml:"readinessPollMs,omitempty" json:"readinessPollMs,omitempty"`
	TraceSamplerArg         *float64 `yaml:"traceSamplerArg,omitempty" json:"traceSamplerArg,omitempty"`
}

// Load reads and validates a BenchmarkProfile from a file path.
// If path is empty, it loads the embedded default reference profile.
func Load(path string) (*BenchmarkProfile, error) {
	if strings.TrimSpace(path) == "" {
		return LoadEmbedded(DefaultProfileName)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read profile %q: %w", path, err)
	}
	return Parse(data)
}

// LoadEmbedded loads a named embedded profile (without the .yaml suffix).
func LoadEmbedded(name string) (*BenchmarkProfile, error) {
	data, err := EmbeddedRaw(name)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// EmbeddedRaw returns the raw YAML bytes of an embedded reference profile.
func EmbeddedRaw(name string) ([]byte, error) {
	if name == "" {
		name = DefaultProfileName
	}
	fileName := fmt.Sprintf("%s.yaml", strings.TrimSuffix(name, ".yaml"))
	data, err := profiles.FS.ReadFile(fileName)
	if err != nil {
		return nil, fmt.Errorf("load embedded profile %q: %w", name, err)
	}
	return data, nil
}

// Parse decodes YAML bytes into a BenchmarkProfile with strict field checking,
// applies defaults, and validates invariants.
func Parse(data []byte) (*BenchmarkProfile, error) {
	var p BenchmarkProfile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse profile yaml: %w", err)
	}
	p.ApplyDefaults()
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// ApplyDefaults populates safe defaults for omitted optional fields.
func (p *BenchmarkProfile) ApplyDefaults() {
	if p.Runtime == "" {
		p.Runtime = RuntimeFastSandboxOverlayBD
	}
	if p.Runtime == RuntimeFastSandboxOverlayBD {
		pool := &p.Stack.FastSandbox.Pool
		if pool.Mode == "" {
			pool.Mode = PoolModeResourceAware
		}
		if pool.MaxSandboxesPerPod == 0 {
			pool.MaxSandboxesPerPod = 500
		}
		if pool.OvercommitRatio.CPU == 0 {
			pool.OvercommitRatio.CPU = 1
		}
		if pool.OvercommitRatio.Memory == 0 {
			pool.OvercommitRatio.Memory = 1
		}
		if pool.PodAntiAffinity == nil {
			v := true
			pool.PodAntiAffinity = &v
		}
		if strings.TrimSpace(pool.NetworkCIDR) == "" {
			pool.NetworkCIDR = DefaultPoolNetworkCIDR
		}
	}
	if p.Environment.SystemNamespace == "" {
		p.Environment.SystemNamespace = "opensandbox-system"
	}
	if p.Environment.ResourceNamespace == "" {
		p.Environment.ResourceNamespace = "opensandbox-dataplane"
	}
	if p.Environment.BenchNamespace == "" {
		p.Environment.BenchNamespace = "opensandbox-bench"
	}
	if p.Environment.Runner.BaseImage == "" {
		p.Environment.Runner.BaseImage = "alpine:3.20"
	}
	if p.Environment.Registry.EphemeralInCluster &&
		strings.TrimSpace(p.Environment.Registry.HostPath) == "" &&
		strings.TrimSpace(p.Environment.Registry.StorageClass) == "" {
		p.Environment.Registry.HostPath = DefaultRegistryHostPath
	}
	if p.Environment.Tracing.Backend == "" {
		p.Environment.Tracing.Backend = "jaeger"
	}
	if p.Environment.Tracing.Sampler == "" {
		p.Environment.Tracing.Sampler = "parentbased_traceidratio"
	}
	if p.Suites.ScheduleBurst != nil && p.Suites.ScheduleBurst.ReadinessPollMs == 0 {
		p.Suites.ScheduleBurst.ReadinessPollMs = 10
	}
	if p.Suites.SteadyChurn != nil && p.Suites.SteadyChurn.ReadinessPollMs == 0 {
		p.Suites.SteadyChurn.ReadinessPollMs = 10
	}
	if p.Suites.HighDensitySoak != nil && p.Suites.HighDensitySoak.SamplerIntervalSeconds == 0 {
		p.Suites.HighDensitySoak.SamplerIntervalSeconds = 2
	}
	if p.Suites.DensityRamp != nil {
		if p.Suites.DensityRamp.ReadinessPollMs == 0 {
			p.Suites.DensityRamp.ReadinessPollMs = 10
		}
		if p.Suites.DensityRamp.BatchCreateConcurrency == 0 {
			p.Suites.DensityRamp.BatchCreateConcurrency = 32
		}
		if p.Suites.DensityRamp.MaxBatchFailureRate == 0 {
			p.Suites.DensityRamp.MaxBatchFailureRate = 0.05
		}
	}
	if p.Suites.DataplaneMicrobench != nil {
		if p.Suites.DataplaneMicrobench.ConcurrentSandboxes == 0 {
			p.Suites.DataplaneMicrobench.ConcurrentSandboxes = 4
		}
		if p.Suites.DataplaneMicrobench.CPUSeconds == 0 {
			p.Suites.DataplaneMicrobench.CPUSeconds = 5
		}
		if p.Suites.DataplaneMicrobench.StorageFileSizeMiB == 0 {
			p.Suites.DataplaneMicrobench.StorageFileSizeMiB = 64
		}
		if p.Suites.DataplaneMicrobench.NetworkRequests == 0 {
			p.Suites.DataplaneMicrobench.NetworkRequests = 50
		}
		if p.Suites.DataplaneMicrobench.NetworkConcurrency == 0 {
			p.Suites.DataplaneMicrobench.NetworkConcurrency = 4
		}
		if p.Suites.DataplaneMicrobench.ReadinessPollMs == 0 {
			p.Suites.DataplaneMicrobench.ReadinessPollMs = 10
		}
	}
	if p.Suites.NoisyNeighbor != nil {
		if p.Suites.NoisyNeighbor.FleetSize == 0 {
			p.Suites.NoisyNeighbor.FleetSize = 20
		}
		if p.Suites.NoisyNeighbor.AggressorRatio == 0 {
			p.Suites.NoisyNeighbor.AggressorRatio = 0.10
		}
		if p.Suites.NoisyNeighbor.ProbeRounds == 0 {
			p.Suites.NoisyNeighbor.ProbeRounds = 20
		}
		if len(p.Suites.NoisyNeighbor.StressModes) == 0 {
			p.Suites.NoisyNeighbor.StressModes = []string{"cpu", "disk", "network"}
		}
		if p.Suites.NoisyNeighbor.StressDurationSeconds == 0 {
			p.Suites.NoisyNeighbor.StressDurationSeconds = 30
		}
		if p.Suites.NoisyNeighbor.ReadinessPollMs == 0 {
			p.Suites.NoisyNeighbor.ReadinessPollMs = 10
		}
	}
	if p.Suites.RolloutDisruption != nil {
		rd := p.Suites.RolloutDisruption
		if len(rd.Scenarios) == 0 {
			rd.Scenarios = []string{"pool_rollout", "multi_fastlet_colocation", "daemonset_rollout"}
		}
		if rd.TargetRPS == 0 {
			rd.TargetRPS = 2.0
		}
		if rd.BaselineSeconds == 0 {
			rd.BaselineSeconds = 10
		}
		if rd.PostRolloutSeconds == 0 {
			rd.PostRolloutSeconds = 10
		}
		if rd.RolloutTimeoutSeconds == 0 {
			rd.RolloutTimeoutSeconds = 180
		}
		if rd.ColocationHoldSeconds == 0 {
			rd.ColocationHoldSeconds = 10
		}
		if rd.SurvivorCount == 0 {
			rd.SurvivorCount = 4
		}
		if rd.SurvivorProbeIntervalMs == 0 {
			rd.SurvivorProbeIntervalMs = 500
		}
		if strings.TrimSpace(rd.PoolName) == "" {
			rd.PoolName = "opensandbox-bench-pool"
		}
		if strings.TrimSpace(rd.DaemonSetName) == "" {
			rd.DaemonSetName = "firecracker-runtime"
		}
		if rd.ReadinessPollMs == 0 {
			rd.ReadinessPollMs = 10
		}
	}
}

// Validate enforces fail-fast profile invariants from OSEP-0034.
func (p *BenchmarkProfile) Validate() error {
	var errs []error

	if p.APIVersion != APIVersionV1Alpha1 {
		errs = append(errs, fmt.Errorf("unsupported apiVersion %q (expected %q)", p.APIVersion, APIVersionV1Alpha1))
	}
	if p.Kind != KindBenchmark {
		errs = append(errs, fmt.Errorf("unsupported kind %q (expected %q)", p.Kind, KindBenchmark))
	}
	if strings.TrimSpace(p.Metadata.Name) == "" {
		errs = append(errs, errors.New("metadata.name must be non-empty"))
	}

	if p.Environment.Tracing.DefaultSamplerArg < 0 || p.Environment.Tracing.DefaultSamplerArg > 1 {
		errs = append(errs, fmt.Errorf("environment.tracing.defaultSamplerArg must be in [0, 1], got %v", p.Environment.Tracing.DefaultSamplerArg))
	}
	switch p.Environment.Tracing.Backend {
	case "jaeger", "tempo":
	default:
		errs = append(errs, fmt.Errorf("environment.tracing.backend must be jaeger or tempo, got %q", p.Environment.Tracing.Backend))
	}

	if p.Runtime == RuntimeFastSandboxOverlayBD {
		oci := p.Stack.FastSandbox.Runtime.OCI
		if !oci.Enabled {
			errs = append(errs, errors.New("stack.fastSandbox.runtime.oci.enabled must be true for fast-sandbox-overlaybd runtime"))
		}
		if oci.PoolSizeGB <= 0 {
			errs = append(errs, fmt.Errorf("stack.fastSandbox.runtime.oci.poolSizeGB must be > 0, got %d", oci.PoolSizeGB))
		}
		if oci.PoolLow < 0 || oci.PoolHigh < oci.PoolLow {
			errs = append(errs, fmt.Errorf("invalid ublk pool bounds: poolLow=%d poolHigh=%d", oci.PoolLow, oci.PoolHigh))
		}
		if p.Stack.Template.DiskGiB <= 0 {
			errs = append(errs, fmt.Errorf("stack.template.diskGiB must be > 0, got %d", p.Stack.Template.DiskGiB))
		}
		// OSEP-0034 invariant: overlaybd-ublkd warm pool devices only match when poolSizeGB == template.diskGiB.
		if oci.PoolSizeGB > 0 && p.Stack.Template.DiskGiB > 0 && oci.PoolSizeGB != p.Stack.Template.DiskGiB {
			errs = append(errs, fmt.Errorf("stack.template.diskGiB (%d) must strictly equal stack.fastSandbox.runtime.oci.poolSizeGB (%d) to prevent silent ublk warm-pool fallback", p.Stack.Template.DiskGiB, oci.PoolSizeGB))
		}
		pool := p.Stack.FastSandbox.Pool
		switch pool.Mode {
		case PoolModeResourceAware, PoolModeLegacySlot:
		default:
			errs = append(errs, fmt.Errorf("stack.fastSandbox.pool.mode must be %q or %q, got %q", PoolModeResourceAware, PoolModeLegacySlot, pool.Mode))
		}
		if pool.MinFastlets <= 0 || pool.MaxFastlets < pool.MinFastlets {
			errs = append(errs, fmt.Errorf("invalid fastlet bounds: minFastlets=%d maxFastlets=%d", pool.MinFastlets, pool.MaxFastlets))
		}
		if pool.MaxSandboxesPerPod < 0 {
			errs = append(errs, fmt.Errorf("stack.fastSandbox.pool.maxSandboxesPerPod must be >= 0, got %d", pool.MaxSandboxesPerPod))
		}
		if cidr := strings.TrimSpace(pool.NetworkCIDR); cidr != "" {
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				errs = append(errs, fmt.Errorf("stack.fastSandbox.pool.networkCIDR %q is invalid: %w", cidr, err))
			}
		}
		if pool.Mode == PoolModeResourceAware {
			if pool.OvercommitRatio.CPU < 1 {
				errs = append(errs, fmt.Errorf("stack.fastSandbox.pool.overcommitRatio.cpu must be >= 1, got %v", pool.OvercommitRatio.CPU))
			}
			if pool.OvercommitRatio.Memory < 1 {
				errs = append(errs, fmt.Errorf("stack.fastSandbox.pool.overcommitRatio.memory must be >= 1, got %v", pool.OvercommitRatio.Memory))
			}
		}
	}

	if p.Suites.WakeStorm != nil && !p.Environment.Redis.Enabled {
		errs = append(errs, errors.New("suites.wakeStorm requires environment.redis.enabled=true"))
	}

	validateSampler := func(name string, v *float64) {
		if v != nil && (*v < 0 || *v > 1) {
			errs = append(errs, fmt.Errorf("suites.%s.traceSamplerArg must be in [0, 1], got %v", name, *v))
		}
	}
	if p.Suites.ScheduleBurst != nil {
		validateSampler("scheduleBurst", p.Suites.ScheduleBurst.TraceSamplerArg)
	}
	if p.Suites.SteadyChurn != nil {
		validateSampler("steadyChurn", p.Suites.SteadyChurn.TraceSamplerArg)
	}
	if p.Suites.PauseResume != nil {
		validateSampler("pauseResume", p.Suites.PauseResume.TraceSamplerArg)
	}
	if p.Suites.RootfsOnlyRestore != nil {
		validateSampler("rootfsOnlyRestore", p.Suites.RootfsOnlyRestore.TraceSamplerArg)
		for i, ov := range p.Suites.RootfsOnlyRestore.ResourceOverrides {
			if ov.VCPUs < 0 {
				errs = append(errs, fmt.Errorf("suites.rootfsOnlyRestore.resourceOverrides[%d].vcpus must be >= 0, got %d", i, ov.VCPUs))
			}
			if ov.MemoryMiB < 0 {
				errs = append(errs, fmt.Errorf("suites.rootfsOnlyRestore.resourceOverrides[%d].memoryMiB must be >= 0, got %d", i, ov.MemoryMiB))
			}
		}
	}
	if p.Suites.WakeStorm != nil {
		validateSampler("wakeStorm", p.Suites.WakeStorm.TraceSamplerArg)
	}
	if p.Environment.LeakAuditGraceSeconds != nil && *p.Environment.LeakAuditGraceSeconds < 0 {
		errs = append(errs, fmt.Errorf("environment.leakAuditGraceSeconds must be >= 0, got %d", *p.Environment.LeakAuditGraceSeconds))
	}
	if p.Suites.HighDensitySoak != nil {
		validateSampler("highDensitySoak", p.Suites.HighDensitySoak.TraceSamplerArg)
	}
	if p.Suites.DensityRamp != nil {
		validateSampler("densityRamp", p.Suites.DensityRamp.TraceSamplerArg)
		if p.Suites.DensityRamp.BatchCreateConcurrency < 0 {
			errs = append(errs, fmt.Errorf("suites.densityRamp.batchCreateConcurrency must be >= 0, got %d", p.Suites.DensityRamp.BatchCreateConcurrency))
		}
		if p.Suites.DensityRamp.MaxBatchFailureRate < 0 || p.Suites.DensityRamp.MaxBatchFailureRate > 1 {
			errs = append(errs, fmt.Errorf("suites.densityRamp.maxBatchFailureRate must be in [0, 1], got %v", p.Suites.DensityRamp.MaxBatchFailureRate))
		}
	}
	if p.Suites.DataplaneMicrobench != nil {
		validateSampler("dataplaneMicrobench", p.Suites.DataplaneMicrobench.TraceSamplerArg)
		if p.Suites.DataplaneMicrobench.ConcurrentSandboxes < 0 {
			errs = append(errs, fmt.Errorf("suites.dataplaneMicrobench.concurrentSandboxes must be >= 0, got %d", p.Suites.DataplaneMicrobench.ConcurrentSandboxes))
		}
		if p.Suites.DataplaneMicrobench.CPUSeconds < 0 {
			errs = append(errs, fmt.Errorf("suites.dataplaneMicrobench.cpuSeconds must be >= 0, got %d", p.Suites.DataplaneMicrobench.CPUSeconds))
		}
		if p.Suites.DataplaneMicrobench.StorageFileSizeMiB < 0 {
			errs = append(errs, fmt.Errorf("suites.dataplaneMicrobench.storageFileSizeMiB must be >= 0, got %d", p.Suites.DataplaneMicrobench.StorageFileSizeMiB))
		}
	}
	if p.Suites.NoisyNeighbor != nil {
		validateSampler("noisyNeighbor", p.Suites.NoisyNeighbor.TraceSamplerArg)
		if p.Suites.NoisyNeighbor.FleetSize < 0 {
			errs = append(errs, fmt.Errorf("suites.noisyNeighbor.fleetSize must be >= 0, got %d", p.Suites.NoisyNeighbor.FleetSize))
		}
		if p.Suites.NoisyNeighbor.AggressorRatio <= 0 || p.Suites.NoisyNeighbor.AggressorRatio >= 1 {
			errs = append(errs, fmt.Errorf("suites.noisyNeighbor.aggressorRatio must be in (0, 1), got %v", p.Suites.NoisyNeighbor.AggressorRatio))
		}
		if p.Suites.NoisyNeighbor.ProbeRounds < 0 {
			errs = append(errs, fmt.Errorf("suites.noisyNeighbor.probeRounds must be >= 0, got %d", p.Suites.NoisyNeighbor.ProbeRounds))
		}
		if p.Suites.NoisyNeighbor.StressDurationSeconds < 0 {
			errs = append(errs, fmt.Errorf("suites.noisyNeighbor.stressDurationSeconds must be >= 0, got %d", p.Suites.NoisyNeighbor.StressDurationSeconds))
		}
		for _, mode := range p.Suites.NoisyNeighbor.StressModes {
			switch mode {
			case "cpu", "disk", "network":
			default:
				errs = append(errs, fmt.Errorf("suites.noisyNeighbor.stressModes contains unsupported mode %q (expected cpu, disk, or network)", mode))
			}
		}
	}
	if p.Suites.RolloutDisruption != nil {
		rd := p.Suites.RolloutDisruption
		validateSampler("rolloutDisruption", rd.TraceSamplerArg)
		if rd.TargetRPS < 0 {
			errs = append(errs, fmt.Errorf("suites.rolloutDisruption.targetRps must be >= 0, got %v", rd.TargetRPS))
		}
		if rd.BaselineSeconds < 0 {
			errs = append(errs, fmt.Errorf("suites.rolloutDisruption.baselineSeconds must be >= 0, got %d", rd.BaselineSeconds))
		}
		if rd.PostRolloutSeconds < 0 {
			errs = append(errs, fmt.Errorf("suites.rolloutDisruption.postRolloutSeconds must be >= 0, got %d", rd.PostRolloutSeconds))
		}
		if rd.RolloutTimeoutSeconds < 0 {
			errs = append(errs, fmt.Errorf("suites.rolloutDisruption.rolloutTimeoutSeconds must be >= 0, got %d", rd.RolloutTimeoutSeconds))
		}
		if rd.ColocationHoldSeconds < 0 {
			errs = append(errs, fmt.Errorf("suites.rolloutDisruption.colocationHoldSeconds must be >= 0, got %d", rd.ColocationHoldSeconds))
		}
		if rd.SurvivorCount < 0 {
			errs = append(errs, fmt.Errorf("suites.rolloutDisruption.survivorCount must be >= 0, got %d", rd.SurvivorCount))
		}
		if rd.SurvivorProbeIntervalMs < 0 {
			errs = append(errs, fmt.Errorf("suites.rolloutDisruption.survivorProbeIntervalMs must be >= 0, got %d", rd.SurvivorProbeIntervalMs))
		}
		for _, sc := range rd.Scenarios {
			switch sc {
			case "pool_rollout", "multi_fastlet_colocation", "daemonset_rollout":
			default:
				errs = append(errs, fmt.Errorf("suites.rolloutDisruption.scenarios contains unsupported scenario %q (expected pool_rollout, multi_fastlet_colocation, or daemonset_rollout)", sc))
			}
		}
	}

	return errors.Join(errs...)
}

// EffectiveSamplerArg returns the trace head-sampling ratio for a given suite.
func (p *BenchmarkProfile) EffectiveSamplerArg(suiteName string) float64 {
	switch suiteName {
	case "schedule-burst":
		if p.Suites.ScheduleBurst != nil && p.Suites.ScheduleBurst.TraceSamplerArg != nil {
			return *p.Suites.ScheduleBurst.TraceSamplerArg
		}
	case "steady-churn":
		if p.Suites.SteadyChurn != nil && p.Suites.SteadyChurn.TraceSamplerArg != nil {
			return *p.Suites.SteadyChurn.TraceSamplerArg
		}
	case "pause-resume":
		if p.Suites.PauseResume != nil && p.Suites.PauseResume.TraceSamplerArg != nil {
			return *p.Suites.PauseResume.TraceSamplerArg
		}
	case "rootfs-only-restore":
		if p.Suites.RootfsOnlyRestore != nil && p.Suites.RootfsOnlyRestore.TraceSamplerArg != nil {
			return *p.Suites.RootfsOnlyRestore.TraceSamplerArg
		}
	case "wake-storm":
		if p.Suites.WakeStorm != nil && p.Suites.WakeStorm.TraceSamplerArg != nil {
			return *p.Suites.WakeStorm.TraceSamplerArg
		}
	case "high-density-soak":
		if p.Suites.HighDensitySoak != nil && p.Suites.HighDensitySoak.TraceSamplerArg != nil {
			return *p.Suites.HighDensitySoak.TraceSamplerArg
		}
	case "density-ramp":
		if p.Suites.DensityRamp != nil && p.Suites.DensityRamp.TraceSamplerArg != nil {
			return *p.Suites.DensityRamp.TraceSamplerArg
		}
	case "dataplane-microbench":
		if p.Suites.DataplaneMicrobench != nil && p.Suites.DataplaneMicrobench.TraceSamplerArg != nil {
			return *p.Suites.DataplaneMicrobench.TraceSamplerArg
		}
	case "noisy-neighbor":
		if p.Suites.NoisyNeighbor != nil && p.Suites.NoisyNeighbor.TraceSamplerArg != nil {
			return *p.Suites.NoisyNeighbor.TraceSamplerArg
		}
	case "rollout-disruption":
		if p.Suites.RolloutDisruption != nil && p.Suites.RolloutDisruption.TraceSamplerArg != nil {
			return *p.Suites.RolloutDisruption.TraceSamplerArg
		}
	}
	return p.Environment.Tracing.DefaultSamplerArg
}
