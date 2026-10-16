package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const s3dfTestKey = "edu.stanford.slac.sdf.storage/sdf-data-lcls"

func TestS3DFAffinitySource(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		config     string
		node       *v1.Node
		key        string
		operator   v1.NodeSelectorOperator
		values     []string
		legacyKeys bool
	}{
		{
			name:   "shared ConfigMap key",
			config: `{"sharedFileSystemPath":"/sdf/data/lcls","nodeAffinityKey":"` + s3dfTestKey + `"}`,
			key:    s3dfTestKey, operator: v1.NodeSelectorOpIn, values: []string{SharedNodeAffinityValue},
		},
		{name: "shared without key", config: `{"sharedFileSystemPath":"/sdf/data/lcls"}`},
		{name: "shared empty key", config: `{"sharedFileSystemPath":"/sdf/data/lcls","nodeAffinityKey":""}`},
		{
			name: "StorageClass is not a source", config: `{"sharedFileSystemPath":"/sdf/data/lcls"}`,
			legacyKeys: true,
		},
		{
			name:   "ConfigMap wins over both obsolete interfaces",
			config: `{"sharedFileSystemPath":"/sdf/data/lcls","nodeAffinityKey":"` + s3dfTestKey + `"}`,
			key:    s3dfTestKey, operator: v1.NodeSelectorOpIn, values: []string{SharedNodeAffinityValue}, legacyKeys: true,
		},
		{
			name:   "per-class key",
			config: `{"nodeAffinityKey":"ignored.example/top-level","storageClassConfigs":{"sdf-test":{"sharedFileSystemPath":"/sdf/data/lcls","nodeAffinityKey":"` + s3dfTestKey + `"},"other":{"sharedFileSystemPath":"/other","nodeAffinityKey":"other.example/share"}}}`,
			key:    s3dfTestKey, operator: v1.NodeSelectorOpIn, values: []string{SharedNodeAffinityValue},
		},
		{
			name:   "per-class key does not inherit",
			config: `{"nodeAffinityKey":"ignored.example/top-level","storageClassConfigs":{"sdf-test":{"sharedFileSystemPath":"/sdf/data/lcls"}}}`,
		},
		{
			name:   "local configured key",
			config: `{"nodePathMap":[{"node":"DEFAULT_PATH_FOR_NON_LISTED_NODES","paths":["/local"]}],"nodeAffinityKey":"example.com/stable-id"}`,
			node:   &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Labels: map[string]string{"example.com/stable-id": "stable-worker"}}},
			key:    "example.com/stable-id", operator: v1.NodeSelectorOpIn, values: []string{"stable-worker"}, legacyKeys: true,
		},
		{
			name:   "local default hostname",
			config: `{"nodePathMap":[{"node":"DEFAULT_PATH_FOR_NON_LISTED_NODES","paths":["/local"]}]}`,
			node:   &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Labels: map[string]string{DefaultNodeAffinityKey: "hostname-1"}}},
			key:    DefaultNodeAffinityKey, operator: v1.NodeSelectorOpIn, values: []string{"hostname-1"}, legacyKeys: true,
		},
		{
			name:   "local missing hostname label",
			config: `{"nodePathMap":[{"node":"DEFAULT_PATH_FOR_NON_LISTED_NODES","paths":["/local"]}]}`,
			node:   &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}},
			key:    DefaultNodeAffinityKey, operator: v1.NodeSelectorOpIn, values: []string{"worker-1"},
		},
		{
			name:   "local missing custom label preserves fallback",
			config: `{"nodePathMap":[{"node":"DEFAULT_PATH_FOR_NON_LISTED_NODES","paths":["/local"]}],"nodeAffinityKey":"example.com/stable-id"}`,
			node:   &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}},
			key:    "example.com/stable-id", operator: v1.NodeSelectorOpIn, values: []string{"worker-1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, _ := newS3DFTestProvisioner(t, tt.config)
			opts := s3dfTestOptions()
			if tt.node != nil { // node-local cases
				opts = s3dfNodeLocalTestOptions()
			}
			opts.SelectedNode = tt.node
			if tt.legacyKeys {
				opts.StorageClass.Annotations = map[string]string{"local.path.provisioner/node-affinity-key": "ignored.example/annotation"}
				opts.StorageClass.Parameters = map[string]string{"nodeAffinityKey": "ignored.example/parameter"}
			}
			pv, _, err := p.Provision(t.Context(), opts)
			require.NoError(t, err)
			if tt.key == "" {
				require.Nil(t, pv.Spec.NodeAffinity)
				return
			}
			expected := &v1.VolumeNodeAffinity{Required: &v1.NodeSelector{
				NodeSelectorTerms: []v1.NodeSelectorTerm{{
					MatchExpressions: []v1.NodeSelectorRequirement{{
						Key: tt.key, Operator: tt.operator, Values: tt.values,
					}},
				}},
			}}
			require.Equal(t, expected, pv.Spec.NodeAffinity)
			if tt.node == nil { // shared cases
				expression, err := json.Marshal(pv.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions[0])
				require.NoError(t, err)
				require.JSONEq(t, fmt.Sprintf(`{"key":%q,"operator":"In","values":["true"]}`, tt.key), string(expression))
			}
		})
	}
}

func TestS3DFAffinityReload(t *testing.T) {
	t.Parallel()
	p, _ := newS3DFTestProvisioner(t, `{"sharedFileSystemPath":"/sdf/data/lcls","nodeAffinityKey":"example.com/old"}`)
	first, _, err := p.Provision(t.Context(), s3dfTestOptions())
	require.NoError(t, err)
	before := first.DeepCopy()
	p.configFile = `{"sharedFileSystemPath":"/sdf/data/lcls","nodeAffinityKey":"example.com/new"}`
	require.NoError(t, p.refreshConfig())
	opts := s3dfTestOptions()
	opts.PVName = "second-pv"
	second, _, err := p.Provision(t.Context(), opts)
	require.NoError(t, err)
	require.NotNil(t, first.Spec.NodeAffinity)
	require.NotNil(t, second.Spec.NodeAffinity)
	require.Equal(t, "example.com/old", first.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions[0].Key)
	require.Equal(t, "example.com/new", second.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions[0].Key)
	require.Equal(t, before, first, "reloading must not change an existing PV")
}

func TestS3DFAffinityKeyValidation(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"bad key", "/bad", "example.com/", "bad_prefix/key"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			data, err := loadConfigFile(fmt.Sprintf(`{"sharedFileSystemPath":"/sdf/data/lcls","nodeAffinityKey":%q}`, key))
			require.NoError(t, err)
			_, err = canonicalizeConfig(data)
			require.ErrorContains(t, err, "nodeAffinityKey")
		})
	}
}

func TestS3DFConcurrentConfigReload(t *testing.T) {
	t.Parallel()
	p, _ := newS3DFTestProvisioner(t, `{"sharedFileSystemPath":"/sdf/data/lcls","nodeAffinityKey":"example.com/share-0"}`)
	p.configFile = filepath.Join(t.TempDir(), "config.json")
	done := make(chan error, 1)
	go func() {
		for i := range 20 {
			data := fmt.Sprintf(`{"sharedFileSystemPath":"/sdf/data/lcls","nodeAffinityKey":"example.com/share-%d"}`, i)
			next := p.configFile + ".next"
			if err := os.WriteFile(next, []byte(data), 0600); err != nil {
				done <- err
				return
			}
			if err := os.Rename(next, p.configFile); err != nil {
				done <- err
				return
			}
			if err := p.refreshConfig(); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for i := range 20 {
		opts := s3dfTestOptions()
		opts.PVName = fmt.Sprintf("pv-%d", i)
		pv, _, err := p.Provision(t.Context(), opts)
		require.NoError(t, err)
		require.NotNil(t, pv.Spec.NodeAffinity)
		require.Equal(t, sharedMountRequirement(pv.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions[0].Key), pv.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions[0])
	}
	require.NoError(t, <-done)
}
