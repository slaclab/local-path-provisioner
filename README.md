# Local Path Provisioner
[![Go Report Card](https://goreportcard.com/badge/github.com/rancher/local-path-provisioner)](https://goreportcard.com/report/github.com/rancher/local-path-provisioner)

> **S3DF image:** this fork permanently retains backing data, brokers pre-existing
> shared roots, and reads node affinity keys from its ConfigMap. See
> [S3DF_IMAGE.md](S3DF_IMAGE.md) for the image contract and manual build instructions.
> The stock Rancher images in the upstream installation examples below do not
> implement this contract; use the reviewed S3DF image for facility storage.

## Overview

Local Path Provisioner provides a way for the Kubernetes users to utilize the local storage in each node. Based on the user configuration, the Local Path Provisioner will create either `hostPath` or `local` based persistent volume on the node automatically. It utilizes the features introduced by Kubernetes [Local Persistent Volume feature](https://kubernetes.io/blog/2018/04/13/local-persistent-volumes-beta/), but makes it a simpler solution than the built-in `local` volume feature in Kubernetes.

## Compare to built-in Local Persistent Volume feature in Kubernetes

### Pros
Dynamic provisioning the volume using [hostPath](https://kubernetes.io/docs/concepts/storage/volumes/#hostpath) or [local](https://kubernetes.io/docs/concepts/storage/volumes/#local).
* Currently the Kubernetes [Local Volume provisioner](https://github.com/kubernetes-sigs/sig-storage-local-static-provisioner) cannot do dynamic provisioning for the local volumes.
* Local based persistent volumes are an experimental feature ([example usage](examples/pvc-with-local-volume/pvc.yaml)).

### Cons
1. No support for the volume capacity limit currently.
    1. The capacity limit will be ignored for now.

## Requirement
Kubernetes v1.12+.

## Deployment

### Installation

In this setup, the directory `/opt/local-path-provisioner` will be used across all the nodes as the path for provisioning (a.k.a, store the persistent volume data). The provisioner will be installed in `local-path-storage` namespace by default.

- Stable
```
kubectl apply -f https://raw.githubusercontent.com/rancher/local-path-provisioner/v0.0.37/deploy/local-path-storage.yaml
```

- Development
```
kubectl apply -f https://raw.githubusercontent.com/rancher/local-path-provisioner/master/deploy/local-path-storage.yaml
```

Or, use `kustomize` to deploy.
- Stable
```
kustomize build "github.com/rancher/local-path-provisioner/deploy?ref=v0.0.37" | kubectl apply -f -
```

- Development
```
kustomize build "github.com/rancher/local-path-provisioner/deploy?ref=master" | kubectl apply -f -
```

After installation, you should see something like the following:
```
$ kubectl -n local-path-storage get pod
NAME                                     READY     STATUS    RESTARTS   AGE
local-path-provisioner-d744ccf98-xfcbk   1/1       Running   0          7m
```

Check and follow the provisioner log using:
```
kubectl -n local-path-storage logs -f -l app=local-path-provisioner
```

## Usage

Create a `hostPath` backend Persistent Volume and a pod uses it:
```
kubectl create -f https://raw.githubusercontent.com/rancher/local-path-provisioner/master/examples/pvc/pvc.yaml
kubectl create -f https://raw.githubusercontent.com/rancher/local-path-provisioner/master/examples/pod/pod.yaml
```

Or, use `kustomize` to deploy them.
```
kustomize build "github.com/rancher/local-path-provisioner/examples/pod?ref=master" | kubectl apply -f -
```

You should see the PV has been created:
```
$ kubectl get pv
NAME                                       CAPACITY   ACCESS MODES   RECLAIM POLICY   STATUS    CLAIM                    STORAGECLASS   REASON    AGE
pvc-bc3117d9-c6d3-11e8-b36d-7a42907dda78   2Gi        RWO            Delete           Bound     default/local-path-pvc   local-path               4s
```

The PVC has been bound:
```
$ kubectl get pvc
NAME             STATUS    VOLUME                                     CAPACITY   ACCESS MODES   STORAGECLASS   AGE
local-path-pvc   Bound     pvc-bc3117d9-c6d3-11e8-b36d-7a42907dda78   2Gi        RWO            local-path     16s
```

And the Pod started running:
```
$ kubectl get pod
NAME          READY     STATUS    RESTARTS   AGE
volume-test   1/1       Running   0          3s
```

Write something into the pod
```
kubectl exec volume-test -- sh -c "echo local-path-test > /data/test"
```

Now delete the pod using
```
kubectl delete -f https://raw.githubusercontent.com/rancher/local-path-provisioner/master/examples/pod/pod.yaml
```

After confirm that the pod is gone, recreated the pod using
```
kubectl create -f https://raw.githubusercontent.com/rancher/local-path-provisioner/master/examples/pod/pod.yaml
```

Check the volume content:
```
$ kubectl exec volume-test -- sh -c "cat /data/test"
local-path-test
```

Delete the pod and pvc
```
kubectl delete -f https://raw.githubusercontent.com/rancher/local-path-provisioner/master/examples/pod/pod.yaml
kubectl delete -f https://raw.githubusercontent.com/rancher/local-path-provisioner/master/examples/pvc/pvc.yaml
```

Or, use `kustomize` to delete them.
```
kustomize build "github.com/rancher/local-path-provisioner/examples/pod?ref=master" | kubectl delete -f -
```

With the S3DF image, the PV API object is reclaimed when its policy is `Delete`,
but the volume's contents are never cleaned up. Retention is logged for each PV.

Now you've verified that the provisioner works as expected.

## Configuration

### Customize the ConfigMap

The configuration of the provisioner is a json file `config.json`, a Pod template `helperPod.yaml` and two bash scripts `setup` and `teardown`, stored in a config map, e.g.:
```
kind: ConfigMap
apiVersion: v1
metadata:
  name: local-path-config
  namespace: local-path-storage
data:
  config.json: |-
        {
                "nodePathMap":[
                {
                        "node":"DEFAULT_PATH_FOR_NON_LISTED_NODES",
                        "paths":["/opt/local-path-provisioner"]
                },
                {
                        "node":"yasker-lp-dev1",
                        "paths":["/opt/local-path-provisioner", "/data1"]
                },
                {
                        "node":"yasker-lp-dev3",
                        "paths":[]
                }
                ]
        }
  setup: |-
        #!/bin/sh
        set -eu
        mkdir -m 0777 -p "$VOL_DIR"
  teardown: |-
        #!/bin/sh
        set -eu
        echo "S3DF retains volume data; teardown is never invoked"
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

```

The helperPod can run setup on nodes experiencing disk pressure. The S3DF image
never creates a cleanup helper and does not free filesystem space on PVC deletion.

#### `config.json`

##### Definition
`nodePathMap` is the place user can customize where to store the data on each node.
1. If one node is not listed on the `nodePathMap`, and Kubernetes wants to create volume on it, the paths specified in `DEFAULT_PATH_FOR_NON_LISTED_NODES` will be used for provisioning.
2. If one node is listed on the `nodePathMap`, the specified paths in `paths` will be used for provisioning.
    1. If one node is listed but with `paths` set to `[]`, the provisioner will refuse to provision on this node.
    2. If more than one path was specified, the path would be chosen randomly when provisioning.

`sharedFileSystemPath` is the pre-existing facility root used verbatim by every PV
from that configuration, without a per-PVC subdirectory. `nodeAffinityKey` in the
same ConfigMap identifies the node label advertising mount availability.
In this case only `ReadWriteMany` and `ReadOnlyMany` claims are supported; `ReadWriteOnce` and `ReadWriteOncePod` are rejected.

`storageClassConfigs` is a map from storage class names to objects containing `nodePathMap` or `sharedFilesystemPath`, as described above.

In addition `volumeBindingMode: Immediate` can be used in  StorageClass definition.

Please note that `nodePathMap`, `sharedFileSystemPath`, and `storageClassConfigs` are mutually exclusive. If `sharedFileSystemPath` or `storageClassConfigs` are used, then `nodePathMap` must be set to `[]`.

`setupCommand` selects a setup binary instead of the setup script, for example
with a distroless helper. `teardownCommand` is accepted for configuration
compatibility but is never invoked. No configuration can re-enable data deletion.
A setup binary can take the following parameters:
| Parameter | Description |
| -------------------- | ----------- |
| -p | Pre-existing shared root, or node-local directory to prepare. |
| -m | The PersistentVolume mode (`Block` or `Filesystem`). |
| -s | Requested volume size in bytes; advisory for shared roots. |
| -a | Always `create`; delete helpers are disabled. |

`setupCommand` has higher priority than the `setup` script from the ConfigMap.

##### Rules
The configuration must obey following rules:
1. `config.json` must be a valid json file.
2. A path must start with `/`, a.k.a an absolute path.
2. Root directory(`/`) is prohibited.
3. No duplicate paths allowed for one node.
4. No duplicate node allowed.

#### Scripts `setup` and `teardown` and the `helperPod.yaml` template

* The `setup` script runs before a PV is created. Shared roots must already exist.
* The `teardown` script is neither mounted into helpers nor executed.
* `helperPod.yaml` supplies the setup Pod template. Shared setup mounts the root
  read-only with `hostPath.type: Directory`, and uses the configured mount-label
  affinity without bypassing the scheduler via `nodeName`.

By default, the helper pod template is validated before it is accepted. The template may customize the helper container image and regular pod settings such as `priorityClassName` and `tolerations`, but it must not define its own `securityContext`, `volumes`, `volumeMounts`, `initContainers`, `ephemeralContainers`, `envFrom`, `env.valueFrom`, `lifecycle`, `livenessProbe`, `readinessProbe`, `startupProbe`, host namespaces, `spec.nodeName`, or `spec.serviceAccountName`.

If you intentionally need a privileged helper pod template, you must opt in explicitly by setting `--allow-unsafe-helper-pod-template` or `ALLOW_UNSAFE_HELPER_POD_TEMPLATE=true` on the provisioner deployment. This disables those safety checks and should only be used for trusted, administrator-managed configurations.

The scripts receive their input as environment variables:

| Environment variable | Description |
| -------------------- | ----------- |
| `VOL_DIR` | Shared facility root verbatim, or the per-PV directory in node-local mode. |
| `VOL_MODE` | The PersistentVolume mode (`Block` or `Filesystem`). |
| `VOL_SIZE_BYTES` | Requested volume size in bytes. |
| `VOL_USER` | User rendered from `userPattern`. |
| `VOL_GROUP` | Group rendered from `groupPattern`. |
| `VOL_PERM` | Permissions rendered from `permPattern`. |

#### Reloading

The provisioner supports automatic configuration reloading. Users can change the configuration using `kubectl apply` or `kubectl edit` with config map `local-path-config`. There is a delay between when the user updates the config map and the provisioner picking it up. In order for this to occur for updates made to the helper pod manifest, the following environment variable must be added to the provisioner container. If not, then the manifest used for the helper pod will be the same as what was in the config map when the provisioner was last restarted/deployed.

```yaml
- name: CONFIG_MOUNT_PATH
  value: /etc/config/
```

When the provisioner detects the configuration changes, it will try to load the new configuration. Users can observe it in the log
>time="2018-10-03T05:56:13Z" level=debug msg="Applied config: {\"nodePathMap\":[{\"node\":\"DEFAULT_PATH_FOR_NON_LISTED_NODES\",\"paths\":[\"/opt/local-path-provisioner\"]},{\"node\":\"yasker-lp-dev1\",\"paths\":[\"/opt\",\"/data1\"]},{\"node\":\"yasker-lp-dev3\"}]}"

If the reload fails, the provisioner will log the error and **continue using the last valid configuration for provisioning in the meantime**.
>time="2018-10-03T05:19:25Z" level=error msg="failed to load the new config file: fail to load config file /etc/config/config.json: invalid character '#' looking for beginning of object key string"

>time="2018-10-03T05:20:10Z" level=error msg="failed to load the new config file: config canonicalization failed: path must start with / for path opt on node yasker-lp-dev1"

>time="2018-10-03T05:23:35Z" level=error msg="failed to load the new config file: config canonicalization failed: duplicate path /data1 on node yasker-lp-dev1

>time="2018-10-03T06:39:28Z" level=error msg="failed to load the new config file: config canonicalization failed: duplicate node yasker-lp-dev3"

The same behavior applies to `helperPod.yaml`: if a reloaded template violates the helper pod safety checks, the provisioner logs the validation error and keeps using the last valid helper pod template.

### Volume Types

To specify the type of volume you want the provisioner to create, add either of the following annotations;

- PVC:
```yaml
annotations:
  volumeType: <local or hostPath>
```

- StorageClass:
```yaml
annotations:
  defaultVolumeType: <local or hostPath>
```

A few things to note; the annotation for the `StorageClass` will apply to all volumes using it and is superseded by the annotation on the PVC if one is provided. If neither of the annotations was provided then we default to `hostPath`.

### Storage classes

If more than one `paths` are specified in the `nodePathMap` the path is chosen randomly. To make the provisioner choose a specific path, use a `storageClass` defined with a parameter called `nodePath`. Note that this path should be defined in the `nodePathMap`.

In node-local mode, the volume subdirectory is named using
`{{ .PVName }}_{{ .PVC.Namespace }}_{{ .PVC.Name }}`. The `pathPattern` Go template
can change this name and has access to `PVName` and the PVC metadata as `PVC`.
Shared mode always mounts the configured facility root. It rejects `pathPattern`,
`allowUnsafePathPattern`, `userPattern`, `groupPattern`, and `permPattern` in either
StorageClass parameters or annotations rather than altering a facility root.

When `pathPattern` is set, the rendered path must start with `{{ .PVC.Namespace }}/{{ .PVC.Name }}/` and must not contain directory traversal (for example `../`).

In node-local mode, `userPattern`, `groupPattern` and `permPattern` (in `parameters`
or `metadata.annotations`) set `$VOL_USER`, `$VOL_GROUP` and `$VOL_PERM` for the
setup script using data from the PVC.

If you need to keep an existing `pathPattern` that does not follow the prefix requirement, you can opt out by setting `allowUnsafePathPattern: "true"` on the StorageClass (either in `parameters` or `metadata.annotations`). When enabled, the provisioner will skip these validations.
```
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: ssd-local-path
provisioner: rancher.io/local-path
parameters:
  nodePath: /data/ssd
  pathPattern: "{{ .PVC.Namespace }}/{{ .PVC.Name }}/"
volumeBindingMode: WaitForFirstConsumer
reclaimPolicy: Delete
```

Here the provisioner will use the path `/data/ssd` with a subdirectory per namespace and PVC when storage class `ssd-local-path` is used.

### Node Affinity Key

`nodeAffinityKey` is read only from the provisioner's `config.json`, alongside
`sharedFileSystemPath` or `nodePathMap`:

```json
{
  "sharedFileSystemPath": "/sdf/data/lcls",
  "nodePathMap": [],
  "nodeAffinityKey": "edu.stanford.slac.sdf.storage/sdf-data-lcls"
}
```

Shared mode emits exactly one `Exists` expression with the configured key and no
values. Setup helpers require the same key. An empty key leaves PV affinity unset
and logs a warning. Label values are irrelevant: remove the key, rather than set
it to `"false"`, when a mount is unhealthy.

Node-local mode emits `In` using the selected node's label value and defaults to
`kubernetes.io/hostname` when the key is unset. The key can also be supplied inside
an individual `storageClassConfigs` entry.

Neither `parameters.nodeAffinityKey` nor the old
`local.path.provisioner/node-affinity-key` StorageClass annotation is consumed.
The Helm value is top-level `nodeAffinityKey`, not `storageClass.nodeAffinityKey`.

**Upgrade note:** ConfigMap changes hot-reload on the normal approximately
30-second interval and affect only newly provisioned PVs. Existing PVs and running
pods are not reconciled or evicted. Existing-PV migration is a separate operation.

## Uninstall

Before uninstallation, make sure the PVs created by the provisioner have already been deleted. Use `kubectl get pv` and make sure no PV with StorageClass `local-path`.

To uninstall, execute:

- Stable
```
kubectl delete -f https://raw.githubusercontent.com/rancher/local-path-provisioner/v0.0.37/deploy/local-path-storage.yaml
```

- Development
```
kubectl delete -f https://raw.githubusercontent.com/rancher/local-path-provisioner/master/deploy/local-path-storage.yaml
```

## Debug
> it providers a out-of-cluster debug env for developers
### debug
```Bash
git clone https://github.com/rancher/local-path-provisioner.git
cd local-path-provisioner
go build
kubectl apply -f debug/config.yaml
./local-path-provisioner --debug start --service-account-name=default
```

### example
[Usage](#usage)

### clear
```
kubectl delete -f debug/config.yaml
```

## License

Copyright (c) 2014-2020  [Rancher Labs, Inc.](http://rancher.com/)

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the License. You may obtain a copy of the License at

[http://www.apache.org/licenses/LICENSE-2.0](http://www.apache.org/licenses/LICENSE-2.0)

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific language governing permissions and limitations under the License.
