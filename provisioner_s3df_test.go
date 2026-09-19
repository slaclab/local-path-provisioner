package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	pvController "sigs.k8s.io/sig-storage-lib-external-provisioner/v11/controller"
)

const s3dfTestPath = "/sdf/data/lcls"

func newS3DFTestProvisioner(t *testing.T, configJSON string) (*LocalPathProvisioner, *fake.Clientset) {
	t.Helper()
	data, err := loadConfigFile(configJSON)
	require.NoError(t, err)
	cfg, err := canonicalizeConfig(data)
	require.NoError(t, err)
	client := fake.NewSimpleClientset(&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}})
	client.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pod := action.(clienttesting.CreateAction).GetObject().(*v1.Pod)
		pod.Status.Phase = v1.PodSucceeded
		return false, nil, nil
	})
	return &LocalPathProvisioner{
		ctx:                t.Context(),
		kubeClient:         client,
		namespace:          "lpp-test",
		helperImage:        "busybox",
		serviceAccountName: "lpp-test",
		configMapName:      "lpp-test",
		configFile:         configJSON,
		configData:         data,
		config:             cfg,
		configMutex:        &sync.RWMutex{},
		helperPod: &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "helper-pod"},
			Spec: v1.PodSpec{
				Containers: []v1.Container{{Name: "helper-pod", Image: "busybox"}},
			},
		},
	}, client
}

func s3dfTestOptions() pvController.ProvisionOptions {
	mode := v1.PersistentVolumeFilesystem
	reclaim := v1.PersistentVolumeReclaimDelete
	class := "sdf-test"
	return pvController.ProvisionOptions{
		PVName: "pv-test",
		StorageClass: &storagev1.StorageClass{
			ObjectMeta:    metav1.ObjectMeta{Name: class},
			Provisioner:   "s3df.test/local-path",
			ReclaimPolicy: &reclaim,
		},
		PVC: &v1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: "science"},
			Spec: v1.PersistentVolumeClaimSpec{
				StorageClassName: &class,
				VolumeMode:       &mode,
				AccessModes:      []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
				Resources: v1.VolumeResourceRequirements{
					Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		},
	}
}

func s3dfCreatedHelpers(client *fake.Clientset) []*v1.Pod {
	var pods []*v1.Pod
	for _, action := range client.Actions() {
		if action.Matches("create", "pods") {
			pods = append(pods, action.(clienttesting.CreateAction).GetObject().(*v1.Pod))
		}
	}
	return pods
}

func TestS3DFDeleteNeverCreatesHelpers(t *testing.T) {
	t.Parallel()
	configs := map[string]string{
		"shared":              `{"sharedFileSystemPath":"/sdf/data/lcls"}`,
		"shared teardown":     `{"sharedFileSystemPath":"/sdf/data/lcls","teardownCommand":"/custom/teardown"}`,
		"local":               `{"nodePathMap":[{"node":"DEFAULT_PATH_FOR_NON_LISTED_NODES","paths":["/local"]}]}`,
		"local teardown":      `{"nodePathMap":[{"node":"DEFAULT_PATH_FOR_NON_LISTED_NODES","paths":["/local"]}],"teardownCommand":"/custom/teardown"}`,
		"removed class":       `{"storageClassConfigs":{"different-class":{"sharedFileSystemPath":"/other"}}}`,
		"unconfigured":        `{}`,
		"obsolete flag false": `{"sharedFileSystemPath":"/sdf/data/lcls","disableDelete":false}`,
		"obsolete flag true":  `{"sharedFileSystemPath":"/sdf/data/lcls","disableDelete":true}`,
	}
	for name, configJSON := range configs {
		for _, volumeType := range []string{"hostPath", "local"} {
			t.Run(name+"/"+volumeType, func(t *testing.T) {
				t.Parallel()
				p, client := newS3DFTestProvisioner(t, configJSON)
				opts := s3dfTestOptions()
				source, err := createPersistentVolumeSource(volumeType, s3dfTestPath)
				require.NoError(t, err)
				pv := &v1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{Name: opts.PVName},
					Spec: v1.PersistentVolumeSpec{
						StorageClassName:              opts.StorageClass.Name,
						PersistentVolumeReclaimPolicy: v1.PersistentVolumeReclaimDelete,
						PersistentVolumeSource:        source,
						VolumeMode:                    opts.PVC.Spec.VolumeMode,
						NodeAffinity: &v1.VolumeNodeAffinity{Required: &v1.NodeSelector{
							NodeSelectorTerms: []v1.NodeSelectorTerm{{
								MatchExpressions: []v1.NodeSelectorRequirement{{
									Key: DefaultNodeAffinityKey, Operator: v1.NodeSelectorOpIn, Values: []string{"worker-1"},
								}},
							}},
						}},
					},
				}
				before := pv.DeepCopy()
				require.NoError(t, p.Delete(context.Background(), pv))
				require.Zero(t, len(s3dfCreatedHelpers(client)), "Delete must never create a helper pod")
				require.Empty(t, client.Actions(), "data retention must not depend on nodes or configuration")
				require.Equal(t, before, pv)
			})
		}
	}
}

func TestS3DFDeleteWithoutConfiguration(t *testing.T) {
	t.Parallel()
	p := &LocalPathProvisioner{}
	pv := &v1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "legacy-pv"}}
	require.NoError(t, p.Delete(t.Context(), pv))
}

func TestS3DFHelperRejectsDeleteAction(t *testing.T) {
	t.Parallel()
	p, client := newS3DFTestProvisioner(t, `{"sharedFileSystemPath":"/sdf/data/lcls"}`)
	err := p.createHelperPod("delete", []string{"/custom/teardown"}, volumeOptions{
		Name: "legacy-pv", Path: s3dfTestPath, Mode: v1.PersistentVolumeFilesystem,
	}, &p.config.StorageClassConfig)
	require.ErrorContains(t, err, "filesystem deletion is disabled")
	require.Empty(t, client.Actions())
}

func TestS3DFSetupDoesNotMountTeardown(t *testing.T) {
	t.Parallel()
	p, client := newS3DFTestProvisioner(t, `{"sharedFileSystemPath":"/sdf/data/lcls"}`)
	_, _, err := p.Provision(t.Context(), s3dfTestOptions())
	require.NoError(t, err)
	pods := s3dfCreatedHelpers(client)
	require.Len(t, pods, 1)
	for _, volume := range pods[0].Spec.Volumes {
		if volume.ConfigMap != nil {
			require.Equal(t, []v1.KeyToPath{{Key: "setup", Path: "setup"}}, volume.ConfigMap.Items)
		}
	}
}

func TestS3DFControllerReclaimsReleasedPV(t *testing.T) {
	t.Parallel()
	p, client := newS3DFTestProvisioner(t, `{"storageClassConfigs":{"other":{"sharedFileSystemPath":"/other"}}}`)
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "released-pv",
			Annotations: map[string]string{"pv.kubernetes.io/provisioned-by": "s3df.test/local-path"},
		},
		Spec: v1.PersistentVolumeSpec{
			StorageClassName:              "removed-class",
			PersistentVolumeReclaimPolicy: v1.PersistentVolumeReclaimDelete,
			PersistentVolumeSource: v1.PersistentVolumeSource{
				HostPath: &v1.HostPathVolumeSource{Path: s3dfTestPath},
			},
		},
		Status: v1.PersistentVolumeStatus{Phase: v1.VolumeReleased},
	}
	_, err := client.CoreV1().PersistentVolumes().Create(t.Context(), pv, metav1.CreateOptions{})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	controller := pvController.NewProvisionController(ctx, client, "s3df.test/local-path", p,
		pvController.LeaderElection(false), pvController.Threadiness(1))
	done := make(chan struct{})
	go func() {
		defer close(done)
		controller.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("provision controller did not stop")
		}
	})
	require.Eventually(t, func() bool {
		_, err := client.CoreV1().PersistentVolumes().Get(ctx, pv.Name, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	}, 5*time.Second, 10*time.Millisecond)
	require.Zero(t, len(s3dfCreatedHelpers(client)))
}

func TestS3DFReclaimPolicyPassthrough(t *testing.T) {
	t.Parallel()
	for _, policy := range []v1.PersistentVolumeReclaimPolicy{v1.PersistentVolumeReclaimDelete, v1.PersistentVolumeReclaimRetain} {
		t.Run(string(policy), func(t *testing.T) {
			t.Parallel()
			p, _ := newS3DFTestProvisioner(t, `{"sharedFileSystemPath":"/sdf/data/lcls","disableDelete":true}`)
			opts := s3dfTestOptions()
			opts.StorageClass.ReclaimPolicy = &policy
			pv, state, err := p.Provision(t.Context(), opts)
			require.NoError(t, err)
			require.Equal(t, pvController.ProvisioningFinished, state)
			require.Equal(t, policy, pv.Spec.PersistentVolumeReclaimPolicy)
		})
	}
}
