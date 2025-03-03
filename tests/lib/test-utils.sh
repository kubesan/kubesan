# SPDX-License-Identifier: Apache-2.0

# Usage: ksan-supported-modes [<mode1> ...]
ksan-supported-modes() {
    for allowed in "$@"; do
        # test does not require mode
        if [[ "${allowed,,}" == "any" ]]; then
            return
        fi
        if [[ "$mode" == "${allowed,,}" ]]; then
            return
        fi
    done

    echo "SKIP: test does not support StorageClass mode \"$mode\"" >&2
    exit 77
}

# Usage: ksan-stage <format> <args...>
ksan-stage() {
    (
        set -o errexit -o pipefail -o nounset +o xtrace

        # shellcheck disable=SC2059
        text="$( printf "$@" )"
        text_lower="${text,,}"

        # shellcheck disable=SC2154
        if (( pause_on_stage )); then
            __log_yellow "Pausing before ${text_lower::1}${text:1}"
            __shell 32 false
            if [[ -e "${temp_dir}/retry" || -e "${temp_dir}/cancel" ]]; then
                exit 1
            fi
        fi

        printf "\033[36m--- [%6.1f] %s\033[0m\n" "$( __elapsed )" "${text}"
    )
}

# Usage: ksan-create-volume <name> <size> <access>
ksan-create-volume() {
    [[ $# == 3 ]] || { echo "usage: $FUNCNAME name size access"; return 1; }

    name=$1
    size=$2
    access=$3

    kubectl create -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $name
spec:
  storageClassName: kubesan-$mode
  accessModes:
    - $access
  resources:
    requests:
      storage: $size
  volumeMode: Block
EOF

    ksan-wait-for-pvc-to-be-bound 60 "$name"
}
export -f ksan-create-volume

# Usage: ksan-create-rwo-volume <name> <size>
ksan-create-rwo-volume() {
    [[ $# == 2 ]] || { echo "usage: $FUNCNAME name size"; return 1; }
    ksan-create-volume "$@" ReadWriteOnce
}
export -f ksan-create-rwo-volume

# Usage: ksan-create-rwx-volume <name> <size>
ksan-create-rwx-volume() {
    [[ $# == 2 ]] || { echo "usage: $FUNCNAME name size"; return 1; }
    ksan-create-volume "$@" ReadWriteMany
}
export -f ksan-create-rwx-volume

# Usage: ksan-create-fs-volume <name> <size>
ksan-create-fs-volume() {
    [[ $# == 2 ]] || { echo "usage: $FUNCNAME name size"; return 1; }

    name=$1
    size=$2

    ksan-stage "Creating Filesystem volume \"$name\"..."

    kubectl create -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $name
spec:
  storageClassName: kubesan-$mode
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: $size
  volumeMode: Filesystem
EOF

    ksan-wait-for-pvc-to-be-bound 60 "$name"
}
export -f ksan-create-fs-volume

# Usage: ksan-fill-volume <name> <size_mb>
ksan-fill-volume() {
    [[ $# == 2 ]] || { echo "usage: $FUNCNAME name size_mb"; return 1; }

    name=$1
    size_mb=$2

    ksan-stage "Writing random data to volume \"$name\"..."

    kubectl create -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: test-pod
spec:
  restartPolicy: Never
  containers:
    - name: container
      image: $TEST_IMAGE
      command:
        - bash
        - -c
        - |
          set -o errexit -o pipefail -o nounset -o xtrace
          dd if=/dev/urandom of=/var/pvc conv=fsync bs=1M count=$size_mb
      volumeDevices:
        - { name: $name, devicePath: /var/pvc }
  volumes:
    - { name: $name, persistentVolumeClaim: { claimName: $name } }
EOF

    ksan-wait-for-pod-to-succeed 60 test-pod
    kubectl delete pod test-pod --timeout=60s
}
export -f ksan-fill-volume

# Usage: ksan-create-snapshot <volume> <snapshot>
ksan-create-snapshot() {
    [[ $# == 2 ]] || { echo "usage: $FUNCNAME volume snapshot"; return 1; }

    volume=$1
    snapshot=$2

    kubectl create -f - <<EOF
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: $snapshot
spec:
  volumeSnapshotClassName: kubesan.gitlab.io
  source:
    persistentVolumeClaimName: $volume
EOF

    ksan-wait-for-vs-to-be-bound 60 "$snapshot"
}
export -f ksan-create-snapshot

# Usage: ksan-delete-snapshot <snapshot>
ksan-delete-snapshot() {
    [[ $# == 1 ]] || { echo "usage: $FUNCNAME snapshot"; return 1; }
    snapshot=$1

    ksan-stage "Deleting snapshot \"$snapshot\"..."
    kubectl delete --cascade=foreground --timeout=60s volumesnapshot "$snapshot"
}
export -f ksan-delete-snapshot

# Usage: ksan-delete volume <volume> [<volume2> ...]
ksan-delete-volume() {
    [[ $# > 0 ]] || { echo "usage: $FUNCNAME volume..."; return 1; }

    ksan-stage "Deleting volumes..."

    # PVCs deletion is immediate but PVs only disappear after CSI DeleteVolume
    # completes successfully. Wait for PVs so that there is no more CSI
    # activity.
    pvs=$(kubectl get pvc "$@" --no-headers -o custom-columns=PV:.spec.volumeName | tr '\n' ' ')
    kubectl delete pvc "$@" --timeout=30s
    ksan-poll 1 30 "[[ -z \"\$(kubectl get --no-headers pv $pvs --ignore-not-found)\" ]]"
}
export -f ksan-delete-volume

# Usage: ksan-get-condition <kind> <name> <condition>
ksan-get-condition() {
    local kind="$1"
    local name="$2"
    local condition="$3"

    kubectl get --namespace kubesan-system "${kind}" "${name}" --output jsonpath="{.status.conditions[?(.type==\"${condition}\")].status}"
}
export -f ksan-get-condition
