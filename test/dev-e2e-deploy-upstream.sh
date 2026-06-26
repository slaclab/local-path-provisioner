#!/usr/bin/env bash
###############################################################################
# Deploy upstream provisioner to dev cluster with real S3DF storage
#
# This script deploys rancher/local-path-provisioner:v0.0.35 to a dev
# cluster that has /sdf/data mounted, ready for E2E testing.
#
# Prerequisites:
#   - kubectl context points to dev cluster (NOT production)
#   - dev cluster nodes have /sdf/data mounted
#   - Optional: NFD installed and node labels like sdf.slac/data-mounted
#
# Usage:
#   ./test/dev-e2e-deploy-upstream.sh [options]
#
# Options:
#   --namespace NAME           namespace to deploy to (default: local-path-upstream)
#   --provisioner-name NAME    provisioner name (default: rancher.io/local-path-upstream)
#   --storage-class NAME       StorageClass name (default: sdf-shared-upstream)
#   --sdf-path PATH            path to shared filesystem (default: /sdf/data)
#   --reclaim-policy POLICY    reclaimPolicy for StorageClass (default: Retain)
#   --node-affinity-key KEY    nodeAffinityKey parameter (default: sdf.slac/data-mounted)
#   --skip-rbac                skip RBAC creation (if already exists)
#   --image IMAGE              override image (default: rancher/local-path-provisioner:v0.0.35)
###############################################################################

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Defaults
NAMESPACE="local-path-upstream"
PROVISIONER_NAME="rancher.io/local-path-upstream"
STORAGE_CLASS="sdf-shared-upstream"
SDF_PATH="/sdf/data"
RECLAIM_POLICY="Retain"
NODE_AFFINITY_KEY="sdf.slac/data-mounted"
SKIP_RBAC=false
IMAGE="rancher/local-path-provisioner:v0.0.35"

# Parse args
while [[ $# -gt 0 ]]; do
  case "$1" in
    --namespace) NAMESPACE="$2"; shift 2 ;;
    --provisioner-name) PROVISIONER_NAME="$2"; shift 2 ;;
    --storage-class) STORAGE_CLASS="$2"; shift 2 ;;
    --sdf-path) SDF_PATH="$2"; shift 2 ;;
    --reclaim-policy) RECLAIM_POLICY="$2"; shift 2 ;;
    --node-affinity-key) NODE_AFFINITY_KEY="$2"; shift 2 ;;
    --skip-rbac) SKIP_RBAC=true; shift ;;
    --image) IMAGE="$2"; shift 2 ;;
    *) echo "Unknown option: $1"; exit 1 ;;
  esac
done

echo "=== Deploying upstream provisioner to dev cluster ==="
echo "Namespace: $NAMESPACE"
echo "Provisioner: $PROVISIONER_NAME"
echo "StorageClass: $STORAGE_CLASS"
echo "SDF path: $SDF_PATH"
echo "Reclaim policy: $RECLAIM_POLICY"
echo "Node affinity key: $NODE_AFFINITY_KEY"
echo "Image: $IMAGE"
echo ""

# Safety check
ctx=$(kubectl config current-context 2>/dev/null || echo "")
if echo "$ctx" | grep -qiE 'prod|sdf-monitoring|sdf-k8s01'; then
  echo "ERROR: kubectl context is pointing at production ($ctx)"
  echo "This script is for dev/staging only. Aborting."
  exit 1
fi
echo "Using context: $ctx"
echo ""

# Deploy
kubectl apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: $NAMESPACE
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: local-path-provisioner-service-account
  namespace: $NAMESPACE
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: local-path-provisioner-role
  namespace: $NAMESPACE
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "watch", "create", "patch", "update", "delete"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: local-path-provisioner-role-$NAMESPACE
rules:
  - apiGroups: [""]
    resources: ["nodes", "persistentvolumeclaims", "configmaps", "pods", "pods/log"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["persistentvolumes"]
    verbs: ["get", "list", "watch", "create", "patch", "update", "delete"]
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["create", "patch"]
  - apiGroups: ["storage.k8s.io"]
    resources: ["storageclasses"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: local-path-provisioner-bind
  namespace: $NAMESPACE
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: local-path-provisioner-role
subjects:
  - kind: ServiceAccount
    name: local-path-provisioner-service-account
    namespace: $NAMESPACE
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: local-path-provisioner-bind-$NAMESPACE
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: local-path-provisioner-role-$NAMESPACE
subjects:
  - kind: ServiceAccount
    name: local-path-provisioner-service-account
    namespace: $NAMESPACE
---
kind: ConfigMap
apiVersion: v1
metadata:
  name: local-path-config
  namespace: $NAMESPACE
data:
  config.json: |-
    { "sharedFileSystemPath": "$SDF_PATH", "nodePathMap": [] }
  setup: |-
    #!/bin/sh
    set -eu
    mkdir -m 0777 -p "\$VOL_DIR"
  teardown: |-
    #!/bin/sh
    set -eu
    rm -rf "\$VOL_DIR"
  helperPod.yaml: |-
    apiVersion: v1
    kind: Pod
    metadata:
      name: helper-pod
    spec:
      priorityClassName: system-node-critical
      tolerations:
        - key: node.kubernetes.io/disk-pressure
          operator: Exists
          effect: NoSchedule
      containers:
      - name: helper-pod
        image: busybox
        imagePullPolicy: IfNotPresent
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: local-path-provisioner
  namespace: $NAMESPACE
spec:
  replicas: 1
  selector:
    matchLabels:
      app: local-path-provisioner
  template:
    metadata:
      labels:
        app: local-path-provisioner
    spec:
      serviceAccountName: local-path-provisioner-service-account
      containers:
        - name: local-path-provisioner
          image: $IMAGE
          imagePullPolicy: IfNotPresent
          command:
            - local-path-provisioner
            - --debug
            - start
            - --provisioner-name
            - $PROVISIONER_NAME
            - --config
            - /etc/config/config.json
          volumeMounts:
            - name: config-volume
              mountPath: /etc/config/
          env:
            - name: POD_NAMESPACE
              valueFrom:
                fieldRef:
                  fieldPath: metadata.namespace
            - name: CONFIG_MOUNT_PATH
              value: /etc/config/
      volumes:
        - name: config-volume
          configMap:
            name: local-path-config
---
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: $STORAGE_CLASS
provisioner: $PROVISIONER_NAME
parameters:
  nodeAffinityKey: $NODE_AFFINITY_KEY
volumeBindingMode: WaitForFirstConsumer
reclaimPolicy: $RECLAIM_POLICY
EOF

echo "=== Waiting for provisioner to start ==="
kubectl -n "$NAMESPACE" rollout status deployment/local-path-provisioner --timeout=120s

echo ""
echo "=== Deployment complete ==="
echo "Provisioner: $PROVISIONER_NAME running in ns/$NAMESPACE"
echo "StorageClass: $STORAGE_CLASS (reclaimPolicy: $RECLAIM_POLICY)"
echo ""
echo "Verify:"
echo "  kubectl -n $NAMESPACE get pods"
echo "  kubectl -n $NAMESPACE logs -l app=local-path-provisioner"
echo "  kubectl get sc $STORAGE_CLASS -o wide"
