package kube

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

// twoContexts is a kubeconfig with two clusters. "blue" uses a token file, so
// client-go reads the token again when it is rotated.
const twoContexts = `apiVersion: v1
kind: Config
current-context: red
clusters:
- name: red
  cluster:
    server: https://red.example:6443
- name: blue
  cluster:
    server: https://blue.example:6443
users:
- name: red
  user:
    token: red-token
- name: blue
  user:
    tokenFile: TOKENFILE
contexts:
- name: red
  context: {cluster: red, user: red}
- name: blue
  context: {cluster: blue, user: blue}
`

// writeKubeconfig writes twoContexts and returns its path and the token file.
func writeKubeconfig(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("blue-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config")
	content := strings.ReplaceAll(twoContexts, "TOKENFILE", tokenFile)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, tokenFile
}

func envWith(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

// inPod is what rest.InClusterConfig returns inside a pod.
func inPod() (*rest.Config, error) { return &rest.Config{Host: "https://10.43.0.1:443"}, nil }

func notInPod() (*rest.Config, error) { return nil, rest.ErrNotInCluster }

func TestInClusterWithoutKubeconfig(t *testing.T) {
	cfg, source, err := loadRestConfig(envWith(nil), inPod, "")
	if err != nil {
		t.Fatalf("loadRestConfig: %v", err)
	}
	if cfg.Host != "https://10.43.0.1:443" || source != "in-cluster" {
		t.Errorf("got host %q, source %q, want the in-cluster config", cfg.Host, source)
	}
}

// An instance in one cluster that watches another: KUBECONFIG must win, or
// smokealarm would silently watch the cluster it runs in.
func TestKubeconfigWinsInsideAPod(t *testing.T) {
	path, _ := writeKubeconfig(t)
	cfg, source, err := loadRestConfig(envWith(map[string]string{"KUBECONFIG": path}), inPod, "")
	if err != nil {
		t.Fatalf("loadRestConfig: %v", err)
	}
	if cfg.Host != "https://red.example:6443" {
		t.Errorf("host = %q, want the current context of the kubeconfig", cfg.Host)
	}
	if source != `kubeconfig context "red"` {
		t.Errorf("source = %q", source)
	}
}

func TestContextChoosesTheCluster(t *testing.T) {
	path, tokenFile := writeKubeconfig(t)
	for _, inCluster := range []func() (*rest.Config, error){inPod, notInPod} {
		cfg, source, err := loadRestConfig(envWith(map[string]string{"KUBECONFIG": path}), inCluster, "blue")
		if err != nil {
			t.Fatalf("loadRestConfig: %v", err)
		}
		if cfg.Host != "https://blue.example:6443" || source != `kubeconfig context "blue"` {
			t.Errorf("got host %q, source %q, want the blue context", cfg.Host, source)
		}
		// The file, not its content: client-go reads it again when it changes.
		if cfg.BearerTokenFile != tokenFile {
			t.Errorf("BearerTokenFile = %q, want %q", cfg.BearerTokenFile, tokenFile)
		}
	}
}

// SMOKEALARM_KUBE_CONTEXT alone also means the kubeconfig: there is no
// context in the in-cluster config.
func TestContextWithoutKubeconfigVariableSkipsInCluster(t *testing.T) {
	called := false
	inCluster := func() (*rest.Config, error) { called = true; return inPod() }
	// ~/.kube/config of the test machine may or may not exist, so only the
	// path taken matters here, not the result.
	_, _, _ = loadRestConfig(envWith(nil), inCluster, "blue")
	if called {
		t.Error("the in-cluster config was asked although a context is set")
	}
}

func TestUnknownContextNamesTheVariable(t *testing.T) {
	path, _ := writeKubeconfig(t)
	_, _, err := loadRestConfig(envWith(map[string]string{"KUBECONFIG": path}), notInPod, "green")
	if err == nil || !strings.Contains(err.Error(), "SMOKEALARM_KUBE_CONTEXT") || !strings.Contains(err.Error(), `"green"`) {
		t.Errorf("err = %v, want one that names SMOKEALARM_KUBE_CONTEXT and the context", err)
	}
}

func TestMissingKubeconfigFileFails(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	_, _, err := loadRestConfig(envWith(map[string]string{"KUBECONFIG": missing}), inPod, "")
	if err == nil {
		t.Fatal("no error for a kubeconfig that does not exist")
	}
}

func TestBrokenInClusterConfigIsReported(t *testing.T) {
	broken := func() (*rest.Config, error) { return nil, errors.New("open token: permission denied") }
	_, _, err := loadRestConfig(envWith(nil), broken, "")
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("err = %v, want the in-cluster error", err)
	}
}
