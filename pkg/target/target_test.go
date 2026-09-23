package target_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse/botbox/pkg/target"
)

func TestWatchedKindsHoldThePrimaryOnce(t *testing.T) {
	widget := schema.GroupVersionKind{Group: "toy.botbox", Version: "v1", Kind: "Widget"}
	configMap := schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}

	watched := (&target.Target{
		Primary: widget,
		Manages: []schema.GroupVersionKind{configMap, widget},
	}).WatchedKinds()

	if want := []schema.GroupVersionKind{widget, configMap}; !slices.Equal(watched, want) {
		t.Errorf("botbox watches %v, want %v.", watched, want)
	}
}

func TestLaunchCheckFindsTheBinaryWhereBotboxRuns(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	for name, mode := range map[string]os.FileMode{"operator": 0o755, "notes": 0o644} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		binary string
		want   []string
	}{
		{binary: "./operator"},
		{binary: filepath.Join(dir, "operator")},
		{binary: "sh"},
		{binary: "bin/operator", want: []string{"launch.binary", "bin/operator", "working directory " + dir, "target.yaml"}},
		{binary: "./notes", want: []string{"launch.binary", "./notes", "permission denied"}},
		{binary: "no-such-program-on-path", want: []string{"launch.binary", "no-such-program-on-path", "PATH"}},
	} {
		t.Run(test.binary, func(t *testing.T) {
			err := target.LaunchSpec{Binary: test.binary}.Check()

			if len(test.want) == 0 {
				if err != nil {
					t.Errorf("Check refused %s: %v", test.binary, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Check accepted %s.", test.binary)
			}
			for _, said := range test.want {
				if !strings.Contains(err.Error(), said) {
					t.Errorf("Check returned %q, which does not say %q.", err, said)
				}
			}
		})
	}
}
