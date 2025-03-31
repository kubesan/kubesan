#!/bin/bash
# SPDX-License-Identifier: Apache-2.0

# kcli attributes
requires_local_deploy=1
requires_external_tool=1
requires_image_pull_policy_always=1
support_sandbox=1
support_snapshots=1

__kcli() {
    kcli "$@"
}
export -f __kcli

# Usage: __kcli_ssh <node> <command...>
__kcli_ssh() {
    __kcli ssh "$1" -- "
        set -o errexit -o pipefail -o nounset
        source .bashrc
        ${*:2}
        "
}
export -f __kcli_ssh

# Usage: __kcli_cluster_exists <suffix>
__kcli_cluster_exists() {
    local __exit_code=0
    __kcli list clusters -o jsoncompact | grep -q '"'$1'"' || __exit_code="$?"

    case "${__exit_code}" in
    0)
        return 0
        ;;
    1)
        return 1
        ;;
    *)
        >&2 echo "kcli failed with exit code ${__exit_code}"
        exit "${__exit_code}"
        ;;
    esac
}
export -f __kcli_cluster_exists

# Usage: __get_kcli_kubeconf <profile>
__get_kcli_kubeconf() {
    KUBECONFIG="$HOME/.kcli/clusters/${1}/auth/kubeconfig"
}
export -f __get_kcli_kubeconf

# Usage: __is_kcli_cluster_running <profile>
__is_kcli_cluster_running() {
    export KUBECONFIG=""
    local kstatus=""

    __get_kcli_kubeconf "$1"
    if [[ -f $KUBECONFIG ]]; then
        ALLNODES=()
        for node in $(kubectl get node --output=name); do
            ALLNODES+=( "${node#node/}" )
        done
        if [ "${#ALLNODES[@]}" -lt "${num_nodes}" ]; then
            return 1
        fi
        for node in "${ALLNODES[@]}"; do
            kstatus=$(kubectl get nodes/$node --output=jsonpath='{.status.conditions[?(@.reason == "KubeletReady")].type}')
            if [[ "${kstatus}" != "Ready" ]]; then
                return 1
            fi
        done
        if ! nc -z 192.168.122.253 5000; then
            return 1
        fi
        if [[ -n "$(kubectl get pods -A -o=jsonpath='{range .items[*]}{.status.phase}{"\n"}{end}' | grep -vE '^(Running|Succeeded)$')" ]]; then
            return 1
        fi
        return 0
    fi
    return 1
}
export -f __is_kcli_cluster_running

# Usage: __wait_kcli_cluster <profile>
__wait_kcli_cluster() {
    local timeout=600

    __log_cyan "Waiting for cluster to be fully operational..."
    while [[ "$timeout" -gt "0" ]]; do
        if ! __is_kcli_cluster_running "$1"; then
            if [[ $(( $timeout % 10 )) -eq 0 ]]; then
                __log_cyan "Still waiting for cluster to be fully operational..."
            fi
            timeout=$(( timeout - 1))
            sleep 1
        else
            __log_green "Cluster is fully operational..."
            return 0
        fi
    done
    __log_red "Timeout waiting for cluster to be operational"
    exit 1
}
export -f __wait_kcli_cluster

# Usage: __start_kcli_cluster <profile> [<extra_kcli_opts...>]
__start_kcli_cluster() {
    export KUBECONFIG=""
    local created=0
    local controllers=1
    local workers=1
    local extregparam=""
    local defaultimg=fedora40

    if ! __kcli_cluster_exists "$1"; then
        if ! __kcli list images -o name |grep -q "/fedora40$"; then
            __kcli download image $defaultimg
        fi

        if [[ "${num_nodes}" -ge "5" ]]; then
            controllers=3
        fi
        workers=$(( num_nodes - controllers ))
        __log_cyan "kcli will deploy $controllers control-plane node(s) and $workers worker(s)"

        if [[ -n "$extregistry" ]]; then
             __log_cyan "Deployment will use registry: $extregistry"
             extregparam="--param disconnected_url=$extregistry"
        fi

        __kcli create plan \
                --inputfile "$(dirname $0)/deployers/kcli-plan.yml" \
                --threaded \
                --param image=$defaultimg \
                --param ctlplanes=$controllers \
                --param workers=$workers \
                $extregparam \
                "$1"

        created=1
    else
        if (( use_cache )); then
            __log_cyan "restore from snapshot"
            __kcli revert plan-snapshot --plan "$1" "$1"-snap
            __kcli create plan-snapshot --plan "$1" "$1"-snap
        fi
    fi

    __kcli start plan "$1"

    __wait_kcli_cluster "$1"
}
export -f __start_kcli_cluster

# Usage: __stop_kcli_cluster [<extra_kcli_opts...>]
__stop_kcli_cluster() {
    __kcli stop plan "$@"
}
export -f __stop_kcli_cluster

# Usage: __restart_kcli_cluster <profile> [<extra_kcli_opts...>]
__restart_kcli_cluster() {
    __stop_kcli_cluster "$@"
    __start_kcli_cluster "$@"
}
export -f __restart_kcli_cluster

# Usage: __delete_kcli_cluster <profile>
__delete_kcli_cluster() {
    if  __kcli_cluster_exists "$1"; then
        __kcli start plan "$1"
        __kcli delete --yes plan "$1"
    fi
}
export -f __delete_kcli_cluster

# Usage: __snapshot_kcli_cluster <profile>
__snapshot_kcli_cluster() {
    __stop_${deploy_tool}_cluster --soft "$1"
    __kcli create plan-snapshot --plan "$1" "$1"-snap
}
export -f __snapshot_kcli_cluster

# Usage: __get_kcli_node_ip <profile> <node>
__get_kcli_node_ip() {
    __kcli show vm "$2" | grep "^ip:" | awk '{print $2}'
}

export -f __get_kcli_node_ip

# Usage: __kcli_image_upload <profile> <image>
__kcli_image_upload() {
    # use profile to detect api IP?
    podman push --tls-verify=false "kubesan/${2}:test" 192.168.122.253:5000/kubesan/${2}:test
}
export -f __kcli_image_upload

# Usage: __get_kcli_registry <profile>
__get_kcli_registry() {
    ksanregistry="192.168.122.253:5000"
}
export -f __get_kcli_registry

# Usage: ksan-kcli-ssh-into-node <node_name>|<node_index> [<command...>]
ksan-kcli-ssh-into-node() {
    if (( $# == 1 )); then
        # shellcheck disable=SC2154
        __kcli \
            ssh \
            -t \
            "$( __ksan-get-node-name "$1" )" \
            -- \
            bash -i
    else
        local __args="${*:2}"
        __kcli \
            ssh \
            -t \
            "$( __ksan-get-node-name "$1" )" \
            -- \
            bash -ic "${__args@Q}" bash
    fi
}
export -f ksan-kcli-ssh-into-node
