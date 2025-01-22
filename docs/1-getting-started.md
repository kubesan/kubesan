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

Your Kubernetes cluster must have the external-snapshotter sidecar and
its CRDs defined. Some Kubernetes distributions ship with them already
available (including OpenShift), while others (including plain
Kubernetes) do not.

If you need to create them, use these commands to do so:

```console
$ kubectl apply -k "https://github.com/kubernetes-csi/external-snapshotter/client/config/crd?ref=v8.2.0"
$ kubectl apply -k "https://github.com/kubernetes-csi/external-snapshotter/deploy/kubernetes/snapshot-controller?ref=v8.2.0"
```

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
$ sudo vgcreate --devicesfile my-vg --shared my-vg /dev/my-san-lun
```

KubeSAN assumes that it will be the sole owner of the shared volume
group; you should not assume that any pre-existing data will be
preserved.  KubeSAN also insists that the volume group name be no
longer than 63 bytes.

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
# hide devices from LVM when invoked without --devicesfile my-vg
$ sudo mkdir -p /etc/lvm/devices
$ sudo touch /etc/lvm/devices/system.devices

# create a devices file for my-vg
# the "--lock-opt skipgl" works around a bug fixed in lvm 2.03.25
$ sudo vgimportdevices --lock-opt skipgl my-vg --devicesfile my-vg

# add devices so dmeventd sees them for automatic extension of thin-pools
$ sudo vgimportdevices --lock-opt skipgl my-vg --devicesfile dmeventd.devices

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

**CAUTION: While KubeSAN is in alpha (ie. a version string
  v0._minor_._patch_), there will be breaking changes between minor
  releases.  There are no guarantees that any PVCs or VolumeSnapshots
  you create under one minor version will still work if you upgrade a
  KubeSAN deployment in-place to another minor version.  The only safe
  approach to using a new minor version is to completely uninstall the
  old KubeSAN version, then create a fresh volume group before
  installing the new version.** However, patch-level releases (if any
  are made) should be safe to use with an in-place upgrade.  We do
  have plans in place to ensure that once KubeSAN v1.0.0 is released,
  all future updates will maintain the typical cross-version
  compatibility matrix recommended by Kubernetes.

If you are using OpenShift:

```console
$ kubectl apply -k https://gitlab.com/kubesan/kubesan/deploy/openshift?ref=v0.10.0
```

Otherwise use the vanilla Kubernetes kustomization:

```console
$ kubectl apply -k https://gitlab.com/kubesan/kubesan/deploy/kubernetes?ref=v0.10.0
```

Next, create a `StorageClass` that uses the KubeSAN CSI plugin and
specifies the name of the shared volume group that you previously
created (here, `my-vg`):

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  annotations:
    cdi.kubevirt.io/clone-strategy: csi-clone
  name: my-san
provisioner: kubesan.gitlab.io
parameters:
  lvmVolumeGroup: my-vg
allowVolumeExpansion: true
```

If you are setting the `mode` parameter to `Linear`, set the StorageClass
`cdi.kubevirt.io/clone-strategy` annotation to `copy`.

If you are using OpenShift Virtualization, you must also patch the corresponding
`StorageProfile` as follows:

```console
$ kubectl patch storageprofile my-san --type=merge -p '{"spec": {"claimPropertySets": [{"accessModes": ["ReadWriteOnce", "ReadOnlyMany", "ReadWriteMany"], "volumeMode": "Block"}, {"accessModes": ["ReadWriteOnce"], "volumeMode": "Filesystem"}], "cloneStrategy": "csi-clone"}}'
```

Now you can create volumes and snapshots like so:

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
      storage: 100Gi
  accessModes:
    - ReadWriteOnce
---
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: my-snapshot
spec:
  volumeSnapshotClassName: kubesan.gitlab.io
  source:
    persistentVolumeClaimName: my-pvc
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
- wipePolicy: Optional, can be 'Full' (default) or 'UnsafeFast'.  This
  parameter controls whether KubeSAN will guarantee that a new empty
  Linear volume, as well as any new bytes added when extending that
  volume, will already be wiped, or whether it is permitted to leave
  the prior contents of the volume group visible.  Setting this
  parameter to 'UnsafeFast' should only be done in a cluster where
  there is no security risk from a prior use of the storage being made
  visible to the new use.  Other modes may be added in the future.
  For Thin volumes and filesystem, new contents always read as zero
  without speed penalty, and regardless of the setting of this parameter.

You can have several KubeSAN `StorageClass`es on the same cluster that
are backed by different shared volume groups, or even multiple classes
that target the same volume group but differ in the other parameters
affecting volume creation.  If you use more than one StorageClass, you
may want to [set the default
StorageClass](https://kubernetes.io/docs/tasks/administer-cluster/change-default-storage-class/)
for use by Persistent Volume Claims that don't explicitly request a
storageclass.

Linear volumes are inherently limited by the size of the volume group,
but Thin volumes can be overcommitted.  You may wish to set up
[resource
quotas](https://kubernetes.io/docs/concepts/policy/resource-quotas/#viewing-and-setting-quotas)
on your cluster to ensure that persistent volume claims do not request
unreasonably large amounts of storage.

The following matrix documents setups that KubeSAN supports or plans to
support in a future release:

| Description         | RWO  | RWX  | ROX     | Online Expand  | Offline Expand | Snapshots/Clonable | Creation by Contents    |
| :------------------ | :--- | :--- | :------ | :------ | :------ | :--------- | :---------- |
| LinearLV Block      | Yes  | Yes  | Planned | No      | Yes     | No         | Planned     |
| LinearLV Filesystem | Yes  | No   | Planned | No      | Planned | No         | Planned     |
| ThinLV Block        | Yes  | Yes  | Planned | Yes     | Yes     | Yes        | Yes         |
| ThinLV Filesystem   | Yes  | No   | Planned | Planned | Planned | Planned    | Planned     |

There are two parts to volume cloning: Clonable represents whether a
volume can serve as the source of another volume at creation time,
while Creation by Contents represents whether a volume can be created
by specifying contents from a snapshot or a clonable volume.
