/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package adversarial

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// The suite boots one envtest API server, the same way the controller suite
// does, and every scenario works in its own namespace so rate limits and
// concurrency checks never see another scenario's objects.
var (
	testEnv   *envtest.Environment
	restCfg   *rest.Config
	k8sClient client.Client
	scheme    = runtime.NewScheme()
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "adding client-go scheme: %v\n", err)
		return 1
	}
	if err := dorguv1.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "adding dorgu scheme: %v\n", err)
		return 1
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: firstEnvTestBinaryDir(),
	}

	var err error
	restCfg, err = testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "starting envtest (run `make setup-envtest`): %v\n", err)
		return 1
	}
	defer func() {
		if stopErr := testEnv.Stop(); stopErr != nil {
			fmt.Fprintf(os.Stderr, "stopping envtest: %v\n", stopErr)
		}
	}()

	k8sClient, err = client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "building admin client: %v\n", err)
		return 1
	}

	code := m.Run()
	WriteSummary(os.Stdout)
	return code
}

// firstEnvTestBinaryDir finds envtest binaries under bin/k8s when
// KUBEBUILDER_ASSETS is not set, as the controller suite does.
func firstEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}

// testCtx returns a context cancelled when the test ends.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}
