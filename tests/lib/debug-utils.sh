# SPDX-License-Identifier: Apache-2.0

ksan-retry() {
    # shellcheck disable=SC2154
    touch "${kubesan_retry_path}"
    exit 0
}
export -f ksan-retry

# shellcheck disable=SC2154
if (( ! sandbox )); then
    ksan-cancel() {
        # shellcheck disable=SC2154
        touch "${kubesan_cancel_path}"
        exit 0
    }
    export -f ksan-cancel
fi

# Usage: ksan-ssh-into-node <node_name>|<node_index> [<command...>]
# Only use for interactive debugging, do not assume node ssh access is
# available in tests because that would require the user to provide ssh
# connection details and that is inconvenient.
ksan-ssh-into-node() {
    if (( $# < 1 )); then
        >&2 echo "Usage: ksan-ssh-into-node <node_name>|<node_index> [<args...>]"
        return 2
    fi
    ksan-${deploy_tool}-ssh-into-node "$@"
}
export -f ksan-ssh-into-node

# Usage: ksan-reimage
# Only use for interactive debugging, after --pause-on-stage or
# --pause-on-failure has paused the test. Rebuilds the images and loads
# them into the cluster, useful for testing a quick compilation change.
# Unlikely to work if deploy/ changed (including any CRD API changes).
ksan-reimage() {
    local -; set -e
    __build_images
    __log_cyan "Importing KubeSAN images into ${deploy_tool} cluster '%s'..." "${current_cluster}"
    for image in kubesan test; do
        # copy locally built image to remote registry
        __${deploy_tool}_image_upload "${current_cluster}" "${image}"
    done
    # Tell the DaemonSet and Deployment to restart with the new image
    kubectl delete --namespace kubesan-system --selector app.kubernetes.io/name=kubesan pod --timeout=30s || echo "reimage may have failed"
}
export -f ksan-reimage
