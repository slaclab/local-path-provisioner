package main

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	pvController "sigs.k8s.io/sig-storage-lib-external-provisioner/v11/controller"
)

func TestS3DFSharedRoot(t *testing.T) {
	t.Parallel()
	for _, path := range []string{s3dfTestPath, s3dfTestPath + "/", "/facility"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			p, client := newS3DFTestProvisioner(t, fmt.Sprintf(`{"sharedFileSystemPath":%q}`, path))
			for i, mode := range []v1.PersistentVolumeAccessMode{v1.ReadOnlyMany, v1.ReadWriteMany} {
				opts := s3dfTestOptions()
				opts.PVName = fmt.Sprintf("pv-%d", i)
				opts.PVC.Name = fmt.Sprintf("claim-%d", i)
				opts.PVC.Spec.AccessModes = []v1.PersistentVolumeAccessMode{mode}
				pv, _, err := p.Provision(t.Context(), opts)
				require.NoError(t, err)
				require.NotNil(t, pv.Spec.HostPath)
				require.Equal(t, path, pv.Spec.HostPath.Path, "every claim must broker the same pre-existing share root")
				require.Equal(t, v1.HostPathDirectory, *pv.Spec.HostPath.Type)
				require.Equal(t, opts.PVC.Spec.AccessModes, pv.Spec.AccessModes)
				require.Equal(t, opts.PVC.Spec.Resources.Requests, pv.Spec.Capacity)
			}
			helpers := s3dfCreatedHelpers(client)
			require.Len(t, helpers, 2)
			for _, pod := range helpers {
				for _, volume := range pod.Spec.Volumes {
					if volume.Name == helperDataVolName {
						require.Equal(t, path, volume.HostPath.Path, "mount the share itself, not its parent")
						require.Equal(t, v1.HostPathDirectory, *volume.HostPath.Type)
					}
				}
				for _, mount := range pod.Spec.Containers[0].VolumeMounts {
					if mount.Name == helperDataVolName {
						require.Equal(t, path, mount.MountPath)
						require.True(t, mount.ReadOnly, "setup must not modify facility data")
					}
				}
				require.Contains(t, pod.Spec.Containers[0].Env, v1.EnvVar{Name: envVolDir, Value: path})
				require.Equal(t, []string{"-p", path}, pod.Spec.Containers[0].Args[:2])
			}
		})
	}
}

func TestS3DFNodeLocalSubdirectories(t *testing.T) {
	t.Parallel()
	for _, volumeType := range []string{"hostPath", "local"} {
		for _, pattern := range []string{"", "{{ .PVC.Namespace }}/{{ .PVC.Name }}/cache"} {
			t.Run(volumeType+"/"+pattern, func(t *testing.T) {
				t.Parallel()
				p, client := newS3DFTestProvisioner(t, `{"nodePathMap":[{"node":"DEFAULT_PATH_FOR_NON_LISTED_NODES","paths":["/local"]}]}`)
				opts := s3dfNodeLocalTestOptions()
				opts.SelectedNode = &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}}
				opts.StorageClass.Annotations = map[string]string{"defaultVolumeType": volumeType}
				want := "/local/pv-test_science_claim"
				if pattern != "" {
					opts.StorageClass.Parameters = map[string]string{"pathPattern": pattern}
					want = "/local/science/claim/cache"
				}
				pv, _, err := p.Provision(t.Context(), opts)
				require.NoError(t, err)
				if volumeType == "hostPath" {
					require.Equal(t, want, pv.Spec.HostPath.Path)
					require.Equal(t, v1.HostPathDirectoryOrCreate, *pv.Spec.HostPath.Type)
				} else {
					require.Equal(t, want, pv.Spec.Local.Path)
				}
				helpers := s3dfCreatedHelpers(client)
				require.Len(t, helpers, 1)
				require.Equal(t, "worker-1", helpers[0].Spec.NodeName)
				for _, volume := range helpers[0].Spec.Volumes {
					if volume.HostPath != nil {
						require.Equal(t, filepath.Dir(want)+"/", volume.HostPath.Path)
						require.Equal(t, v1.HostPathDirectoryOrCreate, *volume.HostPath.Type)
					}
				}
			})
		}
	}
}

func TestS3DFSharedPathOptionsRejected(t *testing.T) {
	t.Parallel()
	options := map[string]string{
		"pathPattern":            "{{ .PVC.Namespace }}/{{ .PVC.Name }}/cache",
		"allowUnsafePathPattern": "false",
		"userPattern":            "1000",
		"groupPattern":           "1000",
		"permPattern":            "0777",
	}
	for key, value := range options {
		for _, annotation := range []bool{false, true} {
			for _, value := range []string{value, ""} {
				t.Run(fmt.Sprintf("%s/annotation=%t/value=%s", key, annotation, value), func(t *testing.T) {
					t.Parallel()
					p, client := newS3DFTestProvisioner(t, `{"sharedFileSystemPath":"/sdf/data/lcls"}`)
					opts := s3dfTestOptions()
					if annotation {
						opts.StorageClass.Annotations = map[string]string{key: value}
					} else {
						opts.StorageClass.Parameters = map[string]string{key: value}
					}
					pv, _, err := p.Provision(t.Context(), opts)
					require.ErrorContains(t, err, key)
					require.Nil(t, pv)
					require.Zero(t, len(s3dfCreatedHelpers(client)))
				})
			}
		}
	}
}

func TestS3DFInvalidSharedRootRejected(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"relative/path", "/", "//", "/sdf/.."} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			data, err := loadConfigFile(fmt.Sprintf(`{"sharedFileSystemPath":%q}`, path))
			require.NoError(t, err)
			_, err = canonicalizeConfig(data)
			require.ErrorContains(t, err, "sharedFileSystemPath")
		})
	}
}

func TestS3DFSharedHelperMountOverrideRejected(t *testing.T) {
	t.Parallel()
	for _, mount := range []v1.VolumeMount{
		{Name: helperDataVolName, MountPath: "/redirect"},
		{Name: helperDataVolName, MountPath: s3dfTestPath, SubPath: "subdir"},
		{Name: helperDataVolName, MountPath: s3dfTestPath, SubPathExpr: "$(SUBDIR)"},
	} {
		t.Run(fmt.Sprintf("%+v", mount), func(t *testing.T) {
			t.Parallel()
			p, client := newS3DFTestProvisioner(t, `{"sharedFileSystemPath":"/sdf/data/lcls"}`)
			p.helperPod.Spec.Containers[0].VolumeMounts = []v1.VolumeMount{mount}
			_, _, err := p.Provision(t.Context(), s3dfTestOptions())
			require.ErrorContains(t, err, "shared filesystem")
			require.Zero(t, len(s3dfCreatedHelpers(client)))
		})
	}
}

func TestS3DFSharedAccessModesRejected(t *testing.T) {
	t.Parallel()
	const config = `{"sharedFileSystemPath":"/sdf/data/lcls"}`
	rejected := map[string]struct {
		modes []v1.PersistentVolumeAccessMode
		want  string
	}{
		"RWO":      {[]v1.PersistentVolumeAccessMode{v1.ReadWriteOnce}, "ReadWriteOnce is not supported"},
		"RWOP":     {[]v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod}, "ReadWriteOncePod is not supported"},
		"RWX+RWO":  {[]v1.PersistentVolumeAccessMode{v1.ReadWriteMany, v1.ReadWriteOnce}, "ReadWriteOnce is not supported"},
		"ROX+RWOP": {[]v1.PersistentVolumeAccessMode{v1.ReadOnlyMany, v1.ReadWriteOncePod}, "ReadWriteOncePod is not supported"},
		"empty":    {nil, "no access mode requested"},
	}
	for name, tt := range rejected {
		t.Run("rejected/"+name, func(t *testing.T) {
			t.Parallel()
			p, client := newS3DFTestProvisioner(t, config)
			opts := s3dfTestOptions()
			opts.PVC.Spec.AccessModes = tt.modes
			pv, state, err := p.Provision(t.Context(), opts)
			require.ErrorContains(t, err, "sharedFileSystemPath only supports ReadWriteMany and ReadOnlyMany access modes")
			require.ErrorContains(t, err, tt.want)
			require.Nil(t, pv)
			require.Equal(t, pvController.ProvisioningFinished, state)
			require.Empty(t, s3dfCreatedHelpers(client), "a rejected claim must not touch the share")
		})
	}
	accepted := map[string][]v1.PersistentVolumeAccessMode{
		"RWX":     {v1.ReadWriteMany},
		"ROX":     {v1.ReadOnlyMany},
		"RWX+ROX": {v1.ReadWriteMany, v1.ReadOnlyMany},
	}
	for name, modes := range accepted {
		t.Run("accepted/"+name, func(t *testing.T) {
			t.Parallel()
			p, client := newS3DFTestProvisioner(t, config)
			opts := s3dfTestOptions()
			opts.PVC.Spec.AccessModes = modes
			pv, state, err := p.Provision(t.Context(), opts)
			require.NoError(t, err)
			require.Equal(t, pvController.ProvisioningFinished, state)
			require.Equal(t, modes, pv.Spec.AccessModes)
			require.Equal(t, s3dfTestPath, pv.Spec.HostPath.Path)
			require.Len(t, s3dfCreatedHelpers(client), 1)
		})
	}
}
