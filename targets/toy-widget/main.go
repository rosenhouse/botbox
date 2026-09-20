// Command toy-widget runs the Widget controller of the toy target (DESIGN.md §9).
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/rosenhouse/botbox/targets/toy-widget/controller"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintf(os.Stderr, "toy-widget: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("toy-widget", flag.ContinueOnError)
	flags.SetOutput(io.Discard) // main reports what Parse rejects; --help prints below.
	kubeconfig := flags.String("kubeconfig", "", "path to a kubeconfig file; defaults to $KUBECONFIG, then to the in-cluster configuration")
	bugID := flags.Int("bug", 0, fmt.Sprintf("seeded bug to run, 0 to %d; 0 is the correct controller (DESIGN.md §9.1)", controller.MaxBug))
	metricsAddress := flags.String("metrics-bind-address", "0", "address the metrics server binds to; 0 disables it")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(out)
			flags.Usage()
		}
		return err
	}
	bug, err := controller.ParseBug(*bugID)
	if err != nil {
		return err
	}

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	config, err := restConfig(*kubeconfig)
	if err != nil {
		return fmt.Errorf("loading the cluster configuration: %w", err)
	}
	scheme, err := controller.NewScheme()
	if err != nil {
		return err
	}
	manager, err := ctrl.NewManager(config, ctrl.Options{
		Scheme:         scheme,
		Metrics:        metricsserver.Options{BindAddress: *metricsAddress},
		LeaderElection: false,
	})
	if err != nil {
		return fmt.Errorf("creating the manager: %w", err)
	}

	reconciler := &controller.Reconciler{
		Client:    manager.GetClient(),
		APIReader: manager.GetAPIReader(),
		Scheme:    manager.GetScheme(),
		Bug:       bug,
	}
	if err := reconciler.SetupWithManager(manager); err != nil {
		return fmt.Errorf("setting up the controller: %w", err)
	}

	ctrl.Log.WithName("toy-widget").Info("starting", "bug", int(bug))
	return manager.Start(ctrl.SetupSignalHandler())
}

// restConfig prefers the --kubeconfig path, then $KUBECONFIG, then the
// in-cluster configuration.
func restConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return ctrl.GetConfig()
}
