package main

import (
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestS3DFHelperUsesMountAffinity(t *testing.T) {
	t.Parallel()
	for _, node := range []*v1.Node{nil, {ObjectMeta: metav1.ObjectMeta{Name: "unlabeled-selected-node"}}} {
		name := "immediate"
		if node != nil {
			name = "selected node must not bypass scheduler"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, client := newS3DFTestProvisioner(t, `{"sharedFileSystemPath":"/sdf/data/lcls","nodeAffinityKey":"`+s3dfTestKey+`"}`)
			opts := s3dfTestOptions()
			opts.SelectedNode = node
			_, _, err := p.Provision(t.Context(), opts)
			require.NoError(t, err)
			helpers := s3dfCreatedHelpers(client)
			require.Len(t, helpers, 1)
			require.Empty(t, helpers[0].Spec.NodeName)
			require.NotNil(t, helpers[0].Spec.Affinity)
			require.Equal(t, &v1.NodeSelector{NodeSelectorTerms: []v1.NodeSelectorTerm{{
				MatchExpressions: []v1.NodeSelectorRequirement{{Key: s3dfTestKey, Operator: v1.NodeSelectorOpIn, Values: []string{SharedNodeAffinityValue}}},
			}}}, helpers[0].Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution)
		})
	}
}

func TestS3DFHelperAffinityIntersectsTemplate(t *testing.T) {
	t.Parallel()
	p, client := newS3DFTestProvisioner(t, `{"sharedFileSystemPath":"/sdf/data/lcls","nodeAffinityKey":"`+s3dfTestKey+`"}`)
	p.helperPod.Spec.NodeSelector = map[string]string{"example.com/zone": "west"}
	p.helperPod.Spec.Affinity = &v1.Affinity{NodeAffinity: &v1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{NodeSelectorTerms: []v1.NodeSelectorTerm{
			{MatchExpressions: []v1.NodeSelectorRequirement{{Key: "example.com/rack", Operator: v1.NodeSelectorOpIn, Values: []string{"a"}}}},
			{MatchFields: []v1.NodeSelectorRequirement{{Key: "metadata.name", Operator: v1.NodeSelectorOpIn, Values: []string{"worker-1"}}}},
			{},
		}},
		PreferredDuringSchedulingIgnoredDuringExecution: []v1.PreferredSchedulingTerm{{
			Weight: 1, Preference: v1.NodeSelectorTerm{
				MatchExpressions: []v1.NodeSelectorRequirement{{Key: "example.com/fast", Operator: v1.NodeSelectorOpExists}},
			},
		}},
	}}
	before := p.helperPod.DeepCopy()
	expected := before.Spec.Affinity.DeepCopy()
	for i := range 2 {
		term := &expected.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[i]
		term.MatchExpressions = append(term.MatchExpressions, v1.NodeSelectorRequirement{Key: s3dfTestKey, Operator: v1.NodeSelectorOpIn, Values: []string{SharedNodeAffinityValue}})
	}
	_, _, err := p.Provision(t.Context(), s3dfTestOptions())
	require.NoError(t, err)
	helpers := s3dfCreatedHelpers(client)
	require.Len(t, helpers, 1)
	require.Equal(t, before, p.helperPod, "the reusable template must not be mutated")
	require.Equal(t, expected, helpers[0].Spec.Affinity)
	require.Equal(t, before.Spec.NodeSelector, helpers[0].Spec.NodeSelector)
}

func TestS3DFHelperRejectsSchedulerBypass(t *testing.T) {
	t.Parallel()
	p, client := newS3DFTestProvisioner(t, `{"sharedFileSystemPath":"/sdf/data/lcls","nodeAffinityKey":"`+s3dfTestKey+`"}`)
	p.helperPod.Spec.NodeName = "unlabeled-node"
	_, _, err := p.Provision(t.Context(), s3dfTestOptions())
	require.ErrorContains(t, err, "nodeName")
	require.Zero(t, len(s3dfCreatedHelpers(client)))
}

func TestS3DFHelperDoesNotBroadenEmptyAffinity(t *testing.T) {
	t.Parallel()
	for _, terms := range [][]v1.NodeSelectorTerm{nil, {{}}} {
		p, client := newS3DFTestProvisioner(t, `{"sharedFileSystemPath":"/sdf/data/lcls","nodeAffinityKey":"`+s3dfTestKey+`"}`)
		p.helperPod.Spec.Affinity = &v1.Affinity{NodeAffinity: &v1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{NodeSelectorTerms: terms},
		}}
		_, _, err := p.Provision(t.Context(), s3dfTestOptions())
		require.ErrorContains(t, err, "node affinity")
		require.Zero(t, len(s3dfCreatedHelpers(client)))
	}
}
