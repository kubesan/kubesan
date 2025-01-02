#!/bin/bash
# SPDX-License-Identifier: Apache-2.0

# minikube attributes
requires_local_deploy=1
requires_external_tool=1
requires_image_pull_policy_always=0
requires_nbd_storage=1
requires_snapshotter=1
support_sandbox=1
support_multiple_clusters=1
support_snapshots=0
support_set_kubectl_context=1


__minikube() {
    minikube --profile="${current_cluster}" "$@"
}
export -f __minikube

# Usage: __minikube_ssh <node> <command...>
__minikube_ssh() {
    __minikube ssh --node="$1" -- "
        set -o errexit -o pipefail -o nounset
        source .bashrc
        ${*:2}
        "
}
export -f __minikube_ssh

# Usage: __minikube_cluster_exists <suffix>
__minikube_cluster_exists() {
    local __exit_code=0
    minikube --profile "$1" status &>/dev/null || __exit_code="$?"

    case "${__exit_code}" in
    0|1|2|3|4|5|6|7)
        return 0
        ;;
    85)
        return 1
        ;;
    *)
        >&2 echo "minikube failed with exit code ${__exit_code}"
        exit "${__exit_code}"
        ;;
    esac
}
export -f __minikube_cluster_exists

# Usage: __get_minikube_kubeconf <profile>
__get_minikube_kubeconf() {
    kubectl config view --raw > "${temp_dir}/kubeconfig"
    KUBECONFIG="${temp_dir}/kubeconfig"
    kubectl config use-context "$1"
}
export -f __get_minikube_kubeconf

# Usage: __is_minikube_cluster_running
__is_minikube_cluster_running() {
    if [[ "$( __minikube status --format='{{.Host}}' )" != Running ]]; then
        return 1
    fi
    return 0
}
export -f __is_minikube_cluster_running

# Usage: __start_minikube_cluster <profile> [<extra_minikube_opts...>]
__start_minikube_cluster() {
    minikube start \
        --iso-url=https://gitlab.com/kubesan/minikube/-/package_files/124271634/download \
        --profile="$1" \
        --driver=kvm2 \
        --cpus=2 \
        --memory=2g \
        --disk-size=6g \
        --keep-context \
        --wait="all" \
        --nodes="${num_nodes}" \
        "${@:2}"

    __get_minikube_kubeconf "$1"
    kubectl label nodes --all node-role.kubernetes.io/worker=
}
export -f __start_minikube_cluster

# Usage: __stop_minikube_cluster [<extra_minikube_opts...>]
__stop_minikube_cluster() {
    __minikube stop "$@"
}
export -f __stop_minikube_cluster

# Usage: __restart_minikube_cluster <profile> [<extra_minikube_opts...>]
__restart_minikube_cluster() {
    __stop_minikube_cluster --keep-context-active --cancel-scheduled
    __start_minikube_cluster "$@"
}
export -f __restart_minikube_cluster

# Usage: __delete_minikube_cluster
__delete_minikube_cluster() {
    __minikube delete
}
export -f __delete_minikube_cluster

# Usage: __snapshot_minikube_cluster <profile>
# NOOP, minikube does not support snapshot
__snapshot_minikube_cluster() {
    return
}
export -f __snapshot_minikube_cluster

# Usage: __get_minikube_node_ip <profile> <node>
__get_minikube_node_ip() {
    __minikube ip --node="$2"
}
export -f __get_minikube_node_ip

# Usage: __minikube_image_upload <profile> <image>
__minikube_image_upload() {
    # Streaming the image over a pipe would be nicer but ${deploy_tool} image
    # load -` writes the image to /tmp and does not clean it up.
    image_file="${temp_dir}/${2}.tar"
    podman save --quiet --output "${image_file}" "kubesan/${2}:test"
    __minikube image load "${image_file}"
    rm -f "${image_file}" # also deleted by temp_dir trap handler on failure
}
export -f __minikube_image_upload

# Usage: __minikube_cp_bashrc <profile> <node>
__minikube_cp_bashrc() {
    __minikube cp \
        "${script_dir}/deployers/minikube-node-bash-profile.sh" \
        "${2}:/home/docker/.bashrc"
}
export -f __minikube_cp_bashrc

# Usage: __get_minikube_registry <profile>
__get_minikube_registry() {
    ksanregistry="docker.io/localhost"
}
export -f __get_minikube_registry

# Usage: __create_minikube_cluster_async <profile> [<extra_minikube_opts...>]
__create_minikube_cluster_async() {
    if ! (( sandbox )); then
        __log_cyan "Creating minikube cluster '%s' in the background to use later..." "$1"
        creating_cluster_in_background=1
        __start_minikube_cluster "$@" &>/dev/null &
    fi
}
export -f __create_minikube_cluster_async

__wait_until_background_minikube_cluster_is_ready() {
    if (( "${creating_cluster_in_background:-0}" == 1 )) && kill -0 "$!" &>/dev/null; then
        __log_cyan "Waiting for minikube cluster '%s' creation to finish before terminating..." \
            "${background_cluster}"
        wait || true
        __log_cyan "Done."
    else
        wait || true
    fi
}
export -f __wait_until_background_minikube_cluster_is_ready

# Usage: ksan-minikube-ssh-into-node <node_name>|<node_index> [<command...>]
ksan-minikube-ssh-into-node() {
    if (( $# == 1 )); then
        # shellcheck disable=SC2154
        minikube \
            --profile "${current_cluster}" \
            ssh \
            --node "$( __ksan-get-node-name "$1" )" \
            -- \
            bash -i
    else
        local __args="${*:2}"
        minikube \
            --profile "${current_cluster}" \
            ssh \
            --node "$( __ksan-get-node-name "$1" )" \
            -- \
            bash -ic "${__args@Q}" bash
    fi
}
export -f ksan-minikube-ssh-into-node
