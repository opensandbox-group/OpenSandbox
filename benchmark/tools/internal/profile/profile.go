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
	"embed"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	APIVersionV1Alpha1 = "bench.opensandbox.io/v1alpha1"
	KindBenchmark      = "BenchmarkProfile"

	RuntimeFastSandboxOverlayBD = "fast-sandbox-overlaybd"
	PoolModeResourceAware       = "ResourceAware"

	DefaultProfileName = "fast-sandbox-overlaybd-ref"
)

//go:embed profiles/*.yaml
var embeddedProfiles embed.FS

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
	Redis               RedisSpec    `yaml:"redis" json:"redis"`
	Tracing             TracingSpec  `yaml:"tracing" json:"tracing"`
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

type FastSandboxPoolSpec struct {
	Mode              string              `yaml:"mode" json:"mode"`
	MinFastlets       int                 `yaml:"minFastlets" json:"minFastlets"`
	MaxFastlets       int                 `yaml:"maxFastlets" json:"maxFastlets"`
	OvercommitRatio   OvercommitRatioSpec `yaml:"overcommitRatio" json:"overcommitRatio"`
	EgressEnabled     bool                `yaml:"egressEnabled" json:"egressEnabled"`
	SpillDir          string              `yaml:"spillDir,omitempty" json:"spillDir,omitempty"`
	BlobCacheMaxBytes int64               `yaml:"blobCacheMaxBytes,omitempty" json:"blobCacheMaxBytes,omitempty"`
}

type OvercommitRatioSpec struct {
	CPU    float64 `yaml:"cpu" json:"cpu"`
	Memory float64 `yaml:"memory" json:"memory"`
}

type TemplateSpec struct {
	BaseImage string `yaml:"baseImage" json:"baseImage"`
	VCPUs     int    `yaml:"vcpus" json:"vcpus"`
	MemoryMiB int    `yaml:"memoryMiB" json:"memoryMiB"`
	DiskGiB   int    `yaml:"diskGiB" json:"diskGiB"`
}

type SuitesSpec struct {
	ScheduleBurst     *ScheduleBurstSuiteSpec     `yaml:"scheduleBurst,omitempty" json:"scheduleBurst,omitempty"`
	SteadyChurn       *SteadyChurnSuiteSpec       `yaml:"steadyChurn,omitempty" json:"steadyChurn,omitempty"`
	PauseResume       *PauseResumeSuiteSpec       `yaml:"pauseResume,omitempty" json:"pauseResume,omitempty"`
	RootfsOnlyRestore *RootfsOnlyRestoreSuiteSpec `yaml:"rootfsOnlyRestore,omitempty" json:"rootfsOnlyRestore,omitempty"`
	WakeStorm         *WakeStormSuiteSpec         `yaml:"wakeStorm,omitempty" json:"wakeStorm,omitempty"`
	HighDensitySoak   *HighDensitySoakSuiteSpec   `yaml:"highDensitySoak,omitempty" json:"highDensitySoak,omitempty"`
	Custom            map[string]map[string]any   `yaml:"custom,omitempty" json:"custom,omitempty"`
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
	VCPUs     int `yaml:"vcpus" json:"vcpus"`
	MemoryMiB int `yaml:"memoryMiB" json:"memoryMiB"`
}

type RootfsOnlyRestoreSuiteSpec struct {
	DirtyUpperMiB       []int                  `yaml:"dirtyUpperMiB" json:"dirtyUpperMiB"`
	FanoutConcurrencies []int                  `yaml:"fanoutConcurrencies" json:"fanoutConcurrencies"`
	CrossNode           bool                   `yaml:"crossNode" json:"crossNode"`
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
	TraceSamplerArg        *float64  `yaml:"traceSamplerArg,omitempty" json:"traceSamplerArg,omitempty"`
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
	fileName := fmt.Sprintf("profiles/%s.yaml", strings.TrimSuffix(name, ".yaml"))
	data, err := embeddedProfiles.ReadFile(fileName)
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
		if p.Stack.FastSandbox.Pool.Mode != PoolModeResourceAware {
			errs = append(errs, fmt.Errorf("Phase 1 requires stack.fastSandbox.pool.mode=%q, got %q", PoolModeResourceAware, p.Stack.FastSandbox.Pool.Mode))
		}
		if p.Stack.FastSandbox.Pool.MinFastlets <= 0 || p.Stack.FastSandbox.Pool.MaxFastlets < p.Stack.FastSandbox.Pool.MinFastlets {
			errs = append(errs, fmt.Errorf("invalid fastlet bounds: minFastlets=%d maxFastlets=%d", p.Stack.FastSandbox.Pool.MinFastlets, p.Stack.FastSandbox.Pool.MaxFastlets))
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
	}
	if p.Suites.WakeStorm != nil {
		validateSampler("wakeStorm", p.Suites.WakeStorm.TraceSamplerArg)
	}
	if p.Suites.HighDensitySoak != nil {
		validateSampler("highDensitySoak", p.Suites.HighDensitySoak.TraceSamplerArg)
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
	}
	return p.Environment.Tracing.DefaultSamplerArg
}
