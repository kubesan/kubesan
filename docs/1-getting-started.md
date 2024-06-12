# Getting started

## Setting up the cluster

This guide assumes you already have a working cluster for Kubernetes;
if you still need to set that up, you might try [CRI-O on
Fedora](https://fedoramagazine.org/kubernetes-with-cri-o-on-fedora-linux-39/)
for bare-metal.

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

Additionally, before you deploy Subprovisioner, you need to make sure
every node in your cluster provides the global resources that
Subprovisioner will be using.

Subprovisioner depends on kernel modules for nbd and dm-thin-pool
being loaded on all nodes.  If your kernel does not already have these
built in, you may need to run this on each node:

```console
$ sudo cat <<EOF | sudo tee /etc/modules-load.d/subprovisioner.conf
nbd
dm-thin-pool
EOF
$ systemctl restart systemd-modules-load.service
```

Generally you should enable as many NBD devices on each node as the
maximum number of Subprovisioner volumes you may need to have mounted
on a single node at once.

## LVM configuration

Before installing Subprovisioner, each node in the cluster must have LVM and
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

Finally, subprovisioner assumes that you have shared storage visible
as a shared LVM Volume Group accessible via one or more block devices
shared to each node of the cluster, such as atop a LUN from a SAN.
This shared VG and lockspace can be created on any node with access to
the LUN, although you may find it easiest to do it on the control-plan
node; here is how to create a VG named `my-vg`:

```console
$ sudo vgcreate --shared my-vg /dev/my-san-lun
```

Subprovisioner will then ensure that all cluster nodes use `vgchange
--lock-start` as needed to access the VG.  Subprovisioner assumes that
it will be the sole owner of the shared volume group; you should not
assume that any pre-existing data will be preserved.

Other shared storage solutions, such as an NFS file mounted through
loopback, or even /dev/nbdX pointing to a common NBD server, will
likely work for hosting a shared VG, although they are less tested.
However, shared storage based on host-based mirroring or replication
is not likely to work correctly, since lvm documents that when
lvmlockd uses sanlock for maintaining shared VG consistency, it works
best when all io is ultimately directed to the same physical location.

## Installing Subprovisioner

If you are using OpenShift:

```console
$ kubectl create -k https://gitlab.com/subprovisioner/subprovisioner/deploy/openshift?ref=v0.2.1
```

Otherwise use the vanilla Kubernetes kustomization:

```console
$ kubectl create -k https://gitlab.com/subprovisioner/subprovisioner/deploy/kubernetes?ref=v0.2.1
```

If you wish to create snapshots of volumes, your Kubernetes cluster must have
the external-snapshotter sidecar and its CRDs defined. Some Kubernetes
distributions ship with them already available, while others (including plain
Kubernetes) do not.

If you need to create them, use these commands to do so:

```console
$ kubectl create -k "https://github.com/kubernetes-csi/external-snapshotter/client/config/crd?ref=v7.0.1"
$ kubectl create -k "https://github.com/kubernetes-csi/external-snapshotter/deploy/kubernetes/snapshot-controller?ref=v7.0.1"
```

Then create a `VolumeSnapshotClass` that uses the Subprovisioner CSI plugin:

```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshotClass
metadata:
  name: subprovisioner
  annotations:
    snapshot.storage.kubernetes.io/is-default-class: "true"
driver: subprovisioner.gitlab.io
deletionPolicy: Delete
```

Create a `StorageClass` that uses the Subprovisioner CSI plugin and
specifies the name of the shared volume group that you previously
created (here, `my-vg`):

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: my-san
provisioner: subprovisioner.gitlab.io
parameters:
  backingVolumeGroup: my-vg
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

You can have several Subprovisioner `StorageClass`es on the same cluster that
are backed by different shared volume groups.
