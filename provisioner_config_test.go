package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type s3dfLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *s3dfLogBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(data)
}

func (b *s3dfLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestS3DFRetentionAuditLogs(t *testing.T) {
	logger := logrus.StandardLogger()
	output, level := logger.Out, logger.GetLevel()
	var logs s3dfLogBuffer
	logger.SetOutput(&logs)
	logger.SetLevel(logrus.InfoLevel)
	t.Cleanup(func() {
		logger.SetOutput(output)
		logger.SetLevel(level)
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p, err := NewProvisioner(ctx, fake.NewSimpleClientset(),
		`{"sharedFileSystemPath":"/sdf/data/lcls"}`, "lpp-test", "busybox", "lpp-test", "lpp-test",
		`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"helper-pod"},"spec":{"containers":[{"name":"helper-pod","image":"busybox"}]}}`, false)
	require.NoError(t, err)
	require.Contains(t, logs.String(), "filesystem deletion is permanently disabled")
	require.NoError(t, p.Delete(ctx, &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "audit-pv"},
		Spec: v1.PersistentVolumeSpec{PersistentVolumeSource: v1.PersistentVolumeSource{
			HostPath: &v1.HostPathVolumeSource{Path: s3dfTestPath},
		}},
	}))
	require.Contains(t, logs.String(), "Retained volume data")
	require.Contains(t, logs.String(), "audit-pv")
	require.Contains(t, logs.String(), s3dfTestPath)
	require.Contains(t, logs.String(), "level=info")
}

func TestS3DFInvalidConfigReloadKeepsLastValid(t *testing.T) {
	t.Parallel()
	p, _ := newS3DFTestProvisioner(t, `{"sharedFileSystemPath":"/sdf/data/lcls","nodeAffinityKey":"`+s3dfTestKey+`"}`)
	before := p.config
	p.configFile = `{"sharedFileSystemPath":"/sdf/data/lcls","nodeAffinityKey":"invalid key"}`
	require.ErrorContains(t, p.refreshConfig(), "nodeAffinityKey")
	require.Same(t, before, p.config)
	pv, _, err := p.Provision(t.Context(), s3dfTestOptions())
	require.NoError(t, err)
	require.Equal(t, s3dfTestKey, pv.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions[0].Key)
}

func TestS3DFInvalidHelperReloadKeepsLastValid(t *testing.T) {
	p, _ := newS3DFTestProvisioner(t, `{"sharedFileSystemPath":"/sdf/data/lcls","nodeAffinityKey":"`+s3dfTestKey+`"}`)
	before := p.helperPod.DeepCopy()
	dir := t.TempDir()
	t.Setenv(EnvConfigMountPath, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, DefaultHelperPodFile), []byte(
		`{"apiVersion":"v1","kind":"Pod","spec":{"containers":[{"name":"helper-pod","image":"busybox","securityContext":{"privileged":true}}]}}`), 0600))
	require.ErrorContains(t, p.refreshHelperPod(), "privileged")
	require.Equal(t, before, p.helperPod)
	_, _, err := p.Provision(t.Context(), s3dfTestOptions())
	require.NoError(t, err)
}

func TestS3DFHelperScriptFollowsSelectedCommand(t *testing.T) {
	t.Parallel()
	for _, custom := range []bool{false, true} {
		name := "script"
		if custom {
			name = "binary"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, client := newS3DFTestProvisioner(t, `{"sharedFileSystemPath":"/sdf/data/lcls"}`)
			cmd := []string{"/bin/sh", "/script/setup"}
			p.config.SetupCommand = "/new-config/setup"
			if custom {
				cmd = []string{"/old-config/setup"}
				p.config.SetupCommand = ""
			}
			err := p.createHelperPod(ActionTypeCreate, cmd, volumeOptions{
				Name: "pv-test", Path: s3dfTestPath, Mode: v1.PersistentVolumeFilesystem,
			}, &p.config.StorageClassConfig)
			require.NoError(t, err)
			helpers := s3dfCreatedHelpers(client)
			require.Len(t, helpers, 1)
			scriptMounted := false
			for _, volume := range helpers[0].Spec.Volumes {
				if volume.ConfigMap != nil {
					scriptMounted = true
					require.Equal(t, []v1.KeyToPath{{Key: "setup", Path: "setup"}}, volume.ConfigMap.Items)
				}
			}
			require.Equal(t, !custom, scriptMounted)
		})
	}
}

func TestS3DFNodeLocalOwnershipPatterns(t *testing.T) {
	t.Parallel()
	for _, annotation := range []bool{false, true} {
		p, client := newS3DFTestProvisioner(t, `{"nodePathMap":[{"node":"DEFAULT_PATH_FOR_NON_LISTED_NODES","paths":["/local"]}]}`)
		opts := s3dfTestOptions()
		opts.SelectedNode = &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}}
		opts.PVC.Labels = map[string]string{"uid": "1000", "gid": "2000"}
		patterns := map[string]string{"userPattern": "{{ .PVC.Labels.uid }}", "groupPattern": "{{ .PVC.Labels.gid }}", "permPattern": "0700"}
		if annotation {
			opts.StorageClass.Annotations = patterns
		} else {
			opts.StorageClass.Parameters = patterns
		}
		_, _, err := p.Provision(t.Context(), opts)
		require.NoError(t, err)
		helpers := s3dfCreatedHelpers(client)
		require.Len(t, helpers, 1)
		require.Contains(t, helpers[0].Spec.Containers[0].Env, v1.EnvVar{Name: envVolUser, Value: "1000"})
		require.Contains(t, helpers[0].Spec.Containers[0].Env, v1.EnvVar{Name: envVolGroup, Value: "2000"})
		require.Contains(t, helpers[0].Spec.Containers[0].Env, v1.EnvVar{Name: envVolPerm, Value: "0700"})
	}
}
