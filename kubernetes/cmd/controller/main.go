// Copyright 2025 Alibaba Group Holding Ltd.
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

package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/controller"
	poolassign "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/controller/poolassign"
	cryptoutil "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/crypto"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/expectations"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/fieldindex"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/logging"
	// +kubebuilder:scaffold:imports
)

var (
	commitID  = "unknown"
	buildDate = "unknown"
)

const (
	defaultBatchSandboxConcurrency = 32
	defaultPoolConcurrency         = 16
)

type ConcurrencyConfig map[string]int

func (c *ConcurrencyConfig) String() string {
	if *c == nil {
		return ""
	}
	parts := make([]string, 0, len(*c))
	for k, v := range *c {
		parts = append(parts, fmt.Sprintf("%s=%d", k, v))
	}
	return strings.Join(parts, ";")
}

func (c *ConcurrencyConfig) Set(value string) error {
	if *c == nil {
		*c = make(ConcurrencyConfig)
	}
	if value == "" {
		return nil
	}
	pairs := strings.Split(value, ";")
	for _, pair := range pairs {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			return fmt.Errorf("invalid concurrency config format: %s, expected format: controller=value", pair)
		}
		name := strings.TrimSpace(kv[0])
		val, err := strconv.Atoi(strings.TrimSpace(kv[1]))
		if err != nil {
			return fmt.Errorf("invalid concurrency value for %s: %v", name, err)
		}
		if val <= 0 {
			return fmt.Errorf("concurrency value must be positive for %s: %d", name, val)
		}
		(*c)[name] = val
	}
	return nil
}

func (c *ConcurrencyConfig) Get(name string, defaultVal int) int {
	if *c != nil {
		if v, ok := (*c)[name]; ok {
			return v
		}
	}
	return defaultVal
}

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

// getKindFromType returns the Kind name for a given runtime.Object using the scheme.
// It panics if the object's kind cannot be determined.
func getKindFromType(obj runtime.Object) string {
	gvks, _, err := scheme.ObjectKinds(obj)
	if err != nil {
		panic(fmt.Sprintf("failed to get kind for object %T: %v", obj, err))
	}
	if len(gvks) == 0 {
		panic(fmt.Sprintf("no kind registered for object %T", obj))
	}
	return gvks[0].Kind
}

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(sandboxv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var allowWeakTLSKeyLengths bool
	var tlsOpts []func(*tls.Config)

	// Log file options
	var enableFileLog bool
	var logFilePath string
	var logMaxSize int
	var logMaxBackups int
	var logMaxAge int
	var logCompress bool

	// Kubernetes client rate limiter options
	var kubeClientQPS float64
	var kubeClientBurst int

	// Controller concurrency options
	var concurrencyConfig ConcurrencyConfig

	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.BoolVar(
		&allowWeakTLSKeyLengths,
		"allow-weak-tls-keylengths",
		false,
		"If set, allows TLS certificates below NIST 2030 minimum key/hash lengths (not recommended).",
	)

	// Log file flags
	flag.BoolVar(&enableFileLog, "enable-file-log", false, "Enable log output to file")
	flag.StringVar(&logFilePath, "log-file-path", "/var/log/sandbox-controller/controller.log", "Path to the log file")
	flag.IntVar(&logMaxSize, "log-max-size", 100, "Maximum size in megabytes of the log file before it gets rotated")
	flag.IntVar(&logMaxBackups, "log-max-backups", 10, "Maximum number of old log files to retain")
	flag.IntVar(&logMaxAge, "log-max-age", 30, "Maximum number of days to retain old log files")
	flag.BoolVar(&logCompress, "log-compress", true, "Compress determines if the rotated log files should be compressed using gzip")
	flag.Float64Var(&kubeClientQPS, "kube-client-qps", 100, "QPS for Kubernetes client rate limiter.")
	flag.IntVar(&kubeClientBurst, "kube-client-burst", 200, "Burst for Kubernetes client rate limiter.")
	flag.Var(&concurrencyConfig, "concurrency", "Controller concurrency settings in format: controller1=N;controller2=M. "+
		"Available controllers: batchsandbox, pool. "+
		"Example: --concurrency='batchsandbox=32;pool=128'")

	// Image committer
	var imageCommitterImage string
	flag.StringVar(&imageCommitterImage, "image-committer-image", "image-committer:dev", "The image used for commit operations (contains nerdctl tool).")

	var containerdSocketPath string
	flag.StringVar(&containerdSocketPath, "containerd-socket-path", controller.ContainerdSocketPath, "Containerd socket path")

	// Commit job timeout
	var commitJobTimeout time.Duration
	flag.DurationVar(&commitJobTimeout, "commit-job-timeout", 10*time.Minute, "The timeout duration for commit jobs.")

	var snapshotRegistry string
	flag.StringVar(&snapshotRegistry, "snapshot-registry", "", "OCI registry for snapshot images (e.g., registry.example.com/snapshots).")

	var snapshotRegistryInsecure bool
	flag.BoolVar(&snapshotRegistryInsecure, "snapshot-registry-insecure", false, "Use insecure registry mode when pushing snapshot images.")

	var snapshotPushSecret string
	flag.StringVar(&snapshotPushSecret, "snapshot-push-secret", "", "K8s Secret name for pushing snapshots to registry.")

	var resumePullSecret string
	flag.StringVar(&resumePullSecret, "resume-pull-secret", "", "K8s Secret name for pulling snapshot images during resume.")

	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)

	flag.Parse()

	// Setup logger with file rotation support
	logOpts := logging.Options{
		Development:      opts.Development,
		EnableFileOutput: enableFileLog,
		LogFilePath:      logFilePath,
		MaxSize:          logMaxSize,
		MaxBackups:       logMaxBackups,
		MaxAge:           logMaxAge,
		Compress:         logCompress,
		ZapOptions:       opts,
	}

	logger := logging.NewLoggerWithZapOptions(logOpts)
	ctrl.SetLogger(logger)

	setupLog.Info("Starting controller", "commitID", commitID, "buildDate", buildDate)

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	tlsOpts = append(tlsOpts, func(c *tls.Config) {
		c.MinVersion = tls.VersionTLS12
	})

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Create watchers for metrics and webhooks certificates
	var metricsCertWatcher, webhookCertWatcher *certwatcher.CertWatcher

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts

	if len(webhookCertPath) > 0 {
		webhookCertFile := filepath.Join(webhookCertPath, webhookCertName)
		webhookKeyFile := filepath.Join(webhookCertPath, webhookCertKey)
		if !allowWeakTLSKeyLengths {
			if err := cryptoutil.ValidateCertificateKeyPair(webhookCertFile, webhookKeyFile); err != nil {
				setupLog.Error(err, "Webhook certificate does not meet NIST minimum key/hash requirements",
					"webhook-cert-file", webhookCertFile, "webhook-key-file", webhookKeyFile)
				os.Exit(1)
			}
		}

		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		var err error
		webhookCertWatcher, err = certwatcher.New(
			webhookCertFile,
			webhookKeyFile,
		)
		if err != nil {
			setupLog.Error(err, "Failed to initialize webhook certificate watcher")
			os.Exit(1)
		}

		webhookTLSOpts = append(webhookTLSOpts, func(config *tls.Config) {
			config.GetCertificate = func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
				cert, err := webhookCertWatcher.GetCertificate(chi)
				if err != nil {
					return nil, err
				}
				if allowWeakTLSKeyLengths {
					return cert, nil
				}
				if err := cryptoutil.ValidateTLSCertificate(webhookCertFile, cert); err != nil {
					return nil, err
				}
				return cert, nil
			}
		})
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: webhookTLSOpts,
	})

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		metricsCertFile := filepath.Join(metricsCertPath, metricsCertName)
		metricsKeyFile := filepath.Join(metricsCertPath, metricsCertKey)
		if !allowWeakTLSKeyLengths && metricsAddr != "0" && secureMetrics {
			if err := cryptoutil.ValidateCertificateKeyPair(metricsCertFile, metricsKeyFile); err != nil {
				setupLog.Error(err, "Metrics certificate does not meet NIST minimum key/hash requirements",
					"metrics-cert-file", metricsCertFile, "metrics-key-file", metricsKeyFile)
				os.Exit(1)
			}
		}

		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		var err error
		metricsCertWatcher, err = certwatcher.New(
			metricsCertFile,
			metricsKeyFile,
		)
		if err != nil {
			setupLog.Error(err, "to initialize metrics certificate watcher", "error", err)
			os.Exit(1)
		}

		metricsServerOptions.TLSOpts = append(metricsServerOptions.TLSOpts, func(config *tls.Config) {
			config.GetCertificate = func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
				cert, err := metricsCertWatcher.GetCertificate(chi)
				if err != nil {
					return nil, err
				}
				if allowWeakTLSKeyLengths {
					return cert, nil
				}
				if err := cryptoutil.ValidateTLSCertificate(metricsCertFile, cert); err != nil {
					return nil, err
				}
				return cert, nil
			}
		})
	}

	config := ctrl.GetConfigOrDie()
	config.UserAgent = "sandbox-k8s-controller/1.0"
	// Set client rate limiter if specified
	if kubeClientQPS != 0 {
		config.QPS = float32(kubeClientQPS)
	}
	if kubeClientBurst != 0 {
		config.Burst = kubeClientBurst
	}

	mgr, err := ctrl.NewManager(config, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "2fa1c467.opensandbox.io",
		// LeaderElectionReleaseOnCancel causes the leader to voluntarily release the lease
		// when the Manager is stopped, allowing a new leader to acquire it without waiting
		// for the full LeaseDuration. This is safe because main() exits immediately after
		// mgr.Start() returns and performs no post-stop cleanup.
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}
	setupLog.Info("register field index")
	if err := fieldindex.RegisterFieldIndexes(mgr.GetCache()); err != nil {
		setupLog.Error(err, "failed to register field index")
		os.Exit(1)
	}

	var (
		batchSandboxKindName = strings.ToLower(getKindFromType(&sandboxv1alpha1.BatchSandbox{}))
		poolKindName         = strings.ToLower(getKindFromType(&sandboxv1alpha1.Pool{}))
	)
	batchSandboxConcurrency := concurrencyConfig.Get(batchSandboxKindName, defaultBatchSandboxConcurrency)
	poolConcurrency := concurrencyConfig.Get(poolKindName, defaultPoolConcurrency)
	setupLog.Info("controller concurrency configured", batchSandboxKindName, batchSandboxConcurrency, poolKindName, poolConcurrency)

	profileStore := poolassign.NewProfileStore()
	_ = profileStore.LoadDefault()
	if err := profileStore.SetupWithManager(mgr, os.Getenv("POD_NAMESPACE")); err != nil {
		setupLog.Error(err, "failed to setup pool assign profiles ConfigMap watch")
		os.Exit(1)
	}

	if err := (&controller.BatchSandboxReconciler{
		Client:              mgr.GetClient(),
		Scheme:              mgr.GetScheme(),
		Recorder:            mgr.GetEventRecorderFor("batchsandbox-controller"),
		ResumePullSecret:    resumePullSecret,
		ProfileStore:        profileStore,
		StatusRVExpectation: expectations.NewResourceVersionExpectation(),
	}).SetupWithManager(mgr, batchSandboxConcurrency); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "BatchSandbox")
		os.Exit(1)
	}
	if err := (&controller.PoolReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		Recorder:   mgr.GetEventRecorderFor("pool-controller"),
		Allocator:  controller.NewDefaultAllocator(mgr.GetClient()),
		RestConfig: mgr.GetConfig(),
	}).SetupWithManager(mgr, poolConcurrency); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Pool")
		os.Exit(1)
	}
	if err := (&controller.SandboxSnapshotReconciler{
		Client:                   mgr.GetClient(),
		Scheme:                   mgr.GetScheme(),
		Recorder:                 mgr.GetEventRecorderFor("sandboxsnapshot-controller"),
		ImageCommitterImage:      imageCommitterImage,
		ContainerdSocketPath:     containerdSocketPath,
		CommitJobTimeout:         commitJobTimeout,
		SnapshotRegistry:         snapshotRegistry,
		SnapshotRegistryInsecure: snapshotRegistryInsecure,
		SnapshotPushSecret:       snapshotPushSecret,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "SandboxSnapshot")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if metricsCertWatcher != nil {
		setupLog.Info("Adding metrics certificate watcher to manager")
		if err := mgr.Add(metricsCertWatcher); err != nil {
			setupLog.Error(err, "unable to add metrics certificate watcher to manager")
			os.Exit(1)
		}
	}

	if webhookCertWatcher != nil {
		setupLog.Info("Adding webhook certificate watcher to manager")
		if err := mgr.Add(webhookCertWatcher); err != nil {
			setupLog.Error(err, "unable to add webhook certificate watcher to manager")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
