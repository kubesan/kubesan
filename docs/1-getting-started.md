# Getting started

## Setting up the cluster

This guide assumes you already have a working cluster for Kubernetes;
if you still need to set that up, you might try [CRI-O on
Fedora](https://fedoramagazine.org/kubernetes-with-cri-o-on-fedora-linux-39/)
for bare-metal.

In addition, by "every node," this guide refers to nodes sharing the SAN. This
includes worker nodes and potentially control-plane nodes.

[//]: # (Comment - this would be a good place to add information on
how to set up a Kubevirt or minikube setup)

Every node in the cluster must have lvmlockd and sanlock installed. Install the
packages on RHEL, CentOS Stream, and Fedora nodes with:

```console
$ sudo dnf install lvm2-lockd sanlock
```

Install the packages OpenShift RHCOS nodes with:
```console
# You may first need to configure a package repository in /etc/yum.repos.d/
$ sudo rpm-ostree install lvm2-lockd sanlock && sudo systemctl reboot
```

Additionally, before you deploy KubeSAN, you need to make sure
every node in your cluster provides the global resources that
KubeSAN will be using.

KubeSAN depends on kernel modules for nbd and dm-thin-pool
being loaded on all nodes.  If your kernel does not already have these
built in, you may need to run this on each node:

```console
$ sudo cat <<EOF | sudo tee /etc/modules-load.d/kubesan.conf
nbd
dm-thin-pool
EOF
$ systemctl restart systemd-modules-load.service
```

The NBD module requires Linux kernel 5.14 or later on all nodes of the
cluster; this is so that KubeSAN can utilize NBD netlink configuration
for as many NBD devices as needed, rather than having to worry about
setting the nbds_max module parameter.

## LVM configuration

Before installing KubeSAN, each node in the cluster must have LVM and
sanlock configured.  Use the following settings in /etc/lvm/lvm.conf:

```
global {
	use_lvmlockd = 1
}
```

Use the following settings in /etc/lvm/lvmlocal.conf:

```
local {
	# The lvmlockd sanlock host_id.
	# This must be unique among all hosts, and must be between 1 and 2000.
	host_id = ...
}
```

Use the following settings in /etc/sanlock/sanlock.conf:

```
# TODO enable watchdog and consider host reset scenarios
use_watchdog = 0
```

Enable and restart associated services as follows:

```
# systemctl enable sanlock lvmlockd
# systemctl restart sanlock lvmlockd
```

## Shared VG configuration

Finally, KubeSAN assumes that you have shared storage visible
as a shared LVM Volume Group accessible via one or more block devices
shared to each node of the cluster, such as atop a LUN from a SAN.
This shared VG and lockspace can be created on any node with access to
the LUN, although you may find it easiest to do it on the control-plane
node; here is how to create a VG named `my-vg`:

```console
$ sudo vgcreate --shared my-vg /dev/my-san-lun
```

KubeSAN assumes that it will be the sole owner of the shared volume group; you
should not assume that any pre-existing data will be preserved.

Other shared storage solutions, such as an NFS file mounted through
loopback, or even /dev/nbdX pointing to a common NBD server, will
likely work for hosting a shared VG, although they are less tested.
However, shared storage based on host-based mirroring or replication
is not likely to work correctly, since lvm documents that when
lvmlockd uses sanlock for maintaining shared VG consistency, it works
best when all io is ultimately directed to the same physical location.

Next, create a devices file with the same name as the LVM Volume Group on
every node in the cluster:

```console
$ sudo lvmdevices --devicesfile my-vg --adddev /dev/my-san-lun

# check if sanlock and lvmlockd are configured correctly
$ sudo vgchange --devicesfile my-vg --lock-start

# make sure the vg is visible
$ sudo vgs --devicesfile my-vg my-vg
```

Each node must have a devices file because KubeSAN restricts its LVM commands
to access only devices listed in this file, reducing the chance of interference
with other users of LVM.

Finally, ensure that locking has been started on every node that will use the
shared LVM Volume Group. This operation must be performed every time a node
boots, either manually or automatically, such as from a systemd unit:

```console
$ sudo vgchange --devicesfile my-vg --lock-start
```

## Installing KubeSAN

If you are using OpenShift:

```console
$ kubectl apply -k https://gitlab.com/kubesan/kubesan/deploy/openshift?ref=v0.8.0
```

Otherwise use the vanilla Kubernetes kustomization:

```console
$ kubectl apply -k https://gitlab.com/kubesan/kubesan/deploy/kubernetes?ref=v0.8.0
```

If you wish to create snapshots of volumes, your Kubernetes cluster must have
the external-snapshotter sidecar and its CRDs defined. Some Kubernetes
distributions ship with them already available, while others (including plain
Kubernetes) do not.

If you need to create them, use these commands to do so:

```console
$ kubectl apply -k "https://github.com/kubernetes-csi/external-snapshotter/client/config/crd?ref=v8.2.0"
$ kubectl apply -k "https://github.com/kubernetes-csi/external-snapshotter/deploy/kubernetes/snapshot-controller?ref=v8.2.0"
```

Then create a `VolumeSnapshotClass` that uses the KubeSAN CSI plugin:

```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshotClass
metadata:
  name: kubesan
  annotations:
    snapshot.storage.kubernetes.io/is-default-class: "true"
driver: kubesan.gitlab.io
deletionPolicy: Delete
```

Create a `StorageClass` that uses the KubeSAN CSI plugin and
specifies the name of the shared volume group that you previously
created (here, `my-vg`):

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: my-san
provisioner: kubesan.gitlab.io
parameters:
  lvmVolumeGroup: my-vg
```

If you are using OpenShift Virtualization, you must also patch the corresponding
`StorageProfile` as follows:

```console
$ kubectl patch storageprofile my-san --type=merge -p '{"spec": {"claimPropertySets": [{"accessModes": ["ReadWriteOnce", "ReadOnlyMany", "ReadWriteMany"], "volumeMode": "Block"}, {"accessModes": ["ReadWriteOnce"], "volumeMode": "Filesystem"}], "cloneStrategy": "copy"}}'
```

Now you can create volumes like so:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-pvc
spec:
  storageClassName: my-san
  volumeMode: Block
  resources:
    requests:
      storage: 1Ti
  accessModes:
    - ReadWriteOnce
```

When creating your StorageClass objects, KubeSAN understands the
following parameters:
- lvmVolumeGroup: Mandatory, must be the name of an LVM Volume Group
  already visible to all nodes in the cluster.  (In the future, we
  plan to add a way to inform KubeSAN about topological constraints,
  such as a volume group visible to only a subset of the cluster)
- mode: Optional. At present, this defaults to "Thin". Specifies the mode to
  use for each volume created by this storage class, can be:
  - "Thin": Volumes are backed by a thin pool LV, and can be sparse
    (unused portions of the volume do not consume storage from the
    VG).  Snapshots, cloning, and online resize are quick, however,
    only one node at a time has efficient access to the volume.  While
    it is possible to have shared access to the volume across nodes,
    such access works best if the window of time where more than one
    node is accessing the volume is short-lived (this is tuned for how
    KubeVirt does live-migration of storage between nodes).  In the
    current release, support for this mode is incomplete (snapshots
    and cloning are not yet implemented), but it will be completed
    before v1.0.0.
  - "Linear": Volumes are fully allocated by a linear LV, and can be
    shared across multiple nodes with no overhead.  It is not
    possible to take snapshots of these volumes, and therefore not
    possible to clone from; and volume expansion is only possible
    offline.

You can have several KubeSAN `StorageClass`es on the same cluster that
are backed by different shared volume groups, or even multiple classes
that target the same volume group but differ in the other parameters
affecting volume creation.  If you use more than one StorageClass, you
may want to [set the default
StorageClass](https://kubernetes.io/docs/tasks/administer-cluster/change-default-storage-class/)
for use by Persistent Volume Claims that don't explicitly request a
storageclass.

The following matrix documents setups that KubeSAN supports or plans to
support in a future release:

| Description         | RWO  | RWX  | ROX     | Expand  | Snapshots | Clone   |
| :------------------ | :--- | :--- | :------ | :------ | :-------- | :------ |
| LinearLV Block      | Yes  | Yes  | Planned | Planned | No        | No      |
| LinearLV Filesystem | Yes  | No   | Planned | Planned | No        | No      |
| ThinLV Block        | Yes  | Yes  | Planned | Planned | Planned   | Planned |
| ThinLV Filesystem   | Yes  | No   | Planned | Planned | Planned   | Planned |
