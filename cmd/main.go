// Command backshots is the single binary of the operator: "manager" runs the controllers,
// "run" is executed inside mover pods, and "install" copies the binary into a pod volume.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	_ "time/tzdata" // the runtime image has no zoneinfo; policies may set spec.timeZone

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/willuhmjs/backshots/api/v1alpha1"
	"github.com/willuhmjs/backshots/internal/controller"
	"github.com/willuhmjs/backshots/internal/jobcfg"
	"github.com/willuhmjs/backshots/internal/runner"
)

const envImage = "BACKSHOTS_IMAGE"

func main() {
	cmd, args := "manager", os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "manager":
		exitOnError(runManager(args))
	case "run":
		os.Exit(runner.Run(ctrl.SetupSignalHandler()))
	case "install":
		exitOnError(runInstall(args))
	default:
		exitOnError(fmt.Errorf("unknown command %q (want manager, run or install)", cmd))
	}
}

func exitOnError(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	return scheme
}

func runManager(args []string) error {
	fs := flag.NewFlagSet("manager", flag.ExitOnError)
	metricsAddr := fs.String("metrics-bind-address", ":8443", "address the metrics endpoint binds to")
	metricsSecure := fs.Bool("metrics-secure", true, "serve metrics over TLS and require an authenticated, authorized caller (token review + subject access review)")
	probeAddr := fs.String("health-probe-bind-address", ":8081", "address the health probe endpoint binds to")
	leaderElect := fs.Bool("leader-elect", false, "enable leader election for the controller manager")
	zapOpts := zap.Options{}
	zapOpts.BindFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	image := os.Getenv(envImage)
	if image == "" {
		return fmt.Errorf("%s must be set to the runner image", envImage)
	}
	scheme := newScheme()
	moverSelector, err := moverLabelSelector()
	if err != nil {
		return err
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsOptions(*metricsAddr, *metricsSecure),
		HealthProbeBindAddress: *probeAddr,
		LeaderElection:         *leaderElect,
		LeaderElectionID:       "backshots.backshots.io",
		// Only objects created for policies are cached; workloads touched by restores are read directly.
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&batchv1.Job{}: {Label: moverSelector},
			&corev1.Pod{}:  {Label: moverSelector},
		}},
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}
	if err := (&controller.PolicyReconciler{Client: mgr.GetClient(), Scheme: scheme, Image: image, Recorder: mgr.GetEventRecorderFor("backshots-policy")}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up policy controller: %w", err)
	}
	restore := &controller.RestoreReconciler{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Scheme: scheme, Image: image, Recorder: mgr.GetEventRecorderFor("backshots-restore")}
	if err := restore.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up restore controller: %w", err)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	return mgr.Start(ctrl.SetupSignalHandler())
}

func moverLabelSelector() (labels.Selector, error) {
	req, err := labels.NewRequirement(jobcfg.LabelPolicy, selection.Exists, nil)
	if err != nil {
		return nil, err
	}
	return labels.NewSelector().Add(*req), nil
}

// runInstall copies this executable to the destination so mover containers running other images
// (e.g. postgres) can execute it from a shared volume.
func runInstall(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: backshots install <destination>")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	return copyExecutable(self, args[0])
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// metricsOptions serves the metrics endpoint. By default it is TLS with a self-signed certificate and every
// request is authenticated (TokenReview) and authorized (SubjectAccessReview against the /metrics URL), so another
// pod in the cluster cannot read other tenants' namespace and policy names from it.
func metricsOptions(addr string, secure bool) metricsserver.Options {
	opts := metricsserver.Options{BindAddress: addr}
	if secure {
		opts.SecureServing = true
		opts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}
	return opts
}
