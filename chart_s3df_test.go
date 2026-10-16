//go:build helm

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func renderS3DFChart(t *testing.T, values string) ([]byte, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "helm", "template", "s3df",
		"./deploy/chart/local-path-provisioner", "--namespace", "lpp-test", "--values", "-")
	cmd.Stdin = strings.NewReader(values)
	return cmd.CombinedOutput()
}

func s3dfChartResources(t *testing.T, output []byte) (*v1.ConfigMap, map[string]*storagev1.StorageClass) {
	t.Helper()
	var cm *v1.ConfigMap
	classes := map[string]*storagev1.StorageClass{}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	for {
		var raw json.RawMessage
		err := decoder.Decode(&raw)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		var meta metav1.TypeMeta
		require.NoError(t, json.Unmarshal(raw, &meta))
		switch meta.Kind {
		case "ConfigMap":
			cm = &v1.ConfigMap{}
			require.NoError(t, json.Unmarshal(raw, cm))
		case "StorageClass":
			sc := &storagev1.StorageClass{}
			require.NoError(t, json.Unmarshal(raw, sc))
			classes[sc.Name] = sc
		}
	}
	require.NotNil(t, cm)
	return cm, classes
}

func TestS3DFChartConfigMapKey(t *testing.T) {
	t.Parallel()
	output, err := renderS3DFChart(t, `
nodePathMap: []
sharedFileSystemPath: /sdf/data/lcls
nodeAffinityKey: edu.stanford.slac.sdf.storage/sdf-data-lcls
storageClass:
  name: sdf-test
  annotations:
    example.com/owner: science
`)
	require.NoError(t, err, string(output))
	cm, classes := s3dfChartResources(t, output)
	require.Len(t, classes, 1)
	sc := classes["sdf-test"]
	require.NotNil(t, sc)
	require.NotContains(t, sc.Parameters, "nodeAffinityKey")
	require.NotContains(t, sc.Annotations, "local.path.provisioner/node-affinity-key")
	require.Equal(t, "science", sc.Annotations["example.com/owner"])
	p, _ := newS3DFTestProvisioner(t, cm.Data["config.json"])
	p.helperPod, err = loadHelperPodFile(cm.Data["helperPod.yaml"], false)
	require.NoError(t, err)
	opts := s3dfTestOptions()
	opts.StorageClass = sc
	pv, _, err := p.Provision(t.Context(), opts)
	require.NoError(t, err)
	require.Equal(t, s3dfTestPath, pv.Spec.HostPath.Path)
	require.Equal(t, v1.HostPathDirectory, *pv.Spec.HostPath.Type)
	require.Equal(t, v1.PersistentVolumeReclaimDelete, pv.Spec.PersistentVolumeReclaimPolicy)
	expression := pv.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions[0]
	require.Equal(t, v1.NodeSelectorRequirement{Key: s3dfTestKey, Operator: v1.NodeSelectorOpIn, Values: []string{SharedNodeAffinityValue}}, expression)
}

func TestS3DFChartPerClassKeys(t *testing.T) {
	t.Parallel()
	output, err := renderS3DFChart(t, `
nodePathMap: []
storageClassConfigs:
  sdf-a:
    sharedFileSystemPath: /facility/a
    nodeAffinityKey: example.com/share-a
    storageClass:
      create: true
      defaultClass: false
      defaultVolumeType: hostPath
      reclaimPolicy: Delete
      volumeBindingMode: WaitForFirstConsumer
  sdf-b:
    sharedFileSystemPath: /facility/b
    nodeAffinityKey: example.com/share-b
    storageClass:
      create: true
      defaultClass: false
      defaultVolumeType: hostPath
      reclaimPolicy: Delete
      volumeBindingMode: WaitForFirstConsumer
`)
	require.NoError(t, err, string(output))
	cm, classes := s3dfChartResources(t, output)
	require.Len(t, classes, 2)
	data, err := loadConfigFile(cm.Data["config.json"])
	require.NoError(t, err)
	cfg, err := canonicalizeConfig(data)
	require.NoError(t, err)
	require.Equal(t, "example.com/share-a", cfg.StorageClassConfigs["sdf-a"].NodeAffinityKey)
	require.Equal(t, "example.com/share-b", cfg.StorageClassConfigs["sdf-b"].NodeAffinityKey)
	for _, sc := range classes {
		require.NotContains(t, sc.Parameters, "nodeAffinityKey")
		require.NotContains(t, sc.Annotations, "local.path.provisioner/node-affinity-key")
	}
}

func TestS3DFChartNodeLocalParameters(t *testing.T) {
	t.Parallel()
	output, err := renderS3DFChart(t, `
nodeAffinityKey: example.com/stable-id
storageClass:
  nodePath: /local
  pathPattern: "{{ .PVC.Namespace }}/{{ .PVC.Name }}/cache"
  allowUnsafePathPattern: false
  userPattern: "1000"
  groupPattern: "2000"
  permPattern: "0700"
`)
	require.NoError(t, err, string(output))
	_, classes := s3dfChartResources(t, output)
	require.Equal(t, map[string]string{
		"nodePath": "/local", "pathPattern": "{{ .PVC.Namespace }}/{{ .PVC.Name }}/cache",
		"allowUnsafePathPattern": "false", "userPattern": "1000", "groupPattern": "2000", "permPattern": "0700",
	}, classes["local-path"].Parameters)
}

func TestS3DFChartRejectsObsoleteKeyValues(t *testing.T) {
	t.Parallel()
	for _, values := range []string{
		"storageClass:\n  nodeAffinityKey: example.com/old\n",
		"storageClass:\n  annotations:\n    local.path.provisioner/node-affinity-key: example.com/old\n",
	} {
		output, err := renderS3DFChart(t, values)
		require.Error(t, err)
		require.Contains(t, string(output), "nodeAffinityKey belongs")
	}
}

func TestS3DFDefaultSetupPreservesExistingRoot(t *testing.T) {
	t.Parallel()
	output, err := renderS3DFChart(t, "{}")
	require.NoError(t, err, string(output))
	cm, _ := s3dfChartResources(t, output)
	root := t.TempDir()
	require.NoError(t, os.Chmod(root, 0750))
	seed := filepath.Join(root, "existing-science-data")
	require.NoError(t, os.WriteFile(seed, []byte("pre-existing data\n"), 0640))
	before, err := os.Stat(root)
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", cm.Data["setup"])
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "VOL_DIR=" + root, "VOL_USER=", "VOL_GROUP=", "VOL_PERM="}
	result, err := cmd.CombinedOutput()
	require.NoError(t, err, string(result))
	after, err := os.Stat(root)
	require.NoError(t, err)
	require.Equal(t, before.Mode(), after.Mode())
	data, err := os.ReadFile(seed)
	require.NoError(t, err)
	require.Equal(t, "pre-existing data\n", string(data))
}
