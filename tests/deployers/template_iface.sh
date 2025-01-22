#!/bin/bash
# SPDX-License-Identifier: Apache-2.0

# template attributes

# set to 1 if this deployer will also create/delete/manage clusters locally.
# set to 0 if cluster is already deployed (for example an external OCP deploy)
requires_local_deploy=1

# set to 1 if a tool that usually  is not available on the system needs
# to be available. kcli and minikube are not available in RHEL or Fedora
# and needs extra repos and such. Set to 0 otherwise
# It is assume kubectl is always installed.
requires_external_tool=1

# set to 1 if the kubesan deployment should be patched to add in
# imagePullPolicy: Always, to guarantee that a restarted kubesan pod
# will get an updated image from the cluster's repository.  Set to 0 if
# the default imagePullPolicy of IfNotPresent will still pick up new
# images from the image_upload command.
requires_image_pull_policy_always=1

# set to 1 if the target cluster does not have shared storage and nbd
# needs to be setup. This will also configure shared vgs and such.
# set to 0 otherwise
requires_nbd_storage=1

# set to 1 if a snapshot provider needs to be installed on the target
# cluster. 0 otherwise.
requires_snapshotter=1

# set to 1 if this target support sandbox, 0 otherwise
support_sandbox=1

# useful only when requires_local_deploy=1, it indicates if this
# deployer can handle multiple clusters at once.
support_multiple_clusters=0

# useful only when requires_local_deploy=1, it indicates if this
# deployer can handle snapshots of a deployed cluster.
support_snapshots=1

# some deployers automatically add information to the current
# user kubeconfig. Set to 1 to load the context from kubeconfig
# or 0 otherwise.
support_set_kubectl_context=0


# Usage: __template()
# simple wrapper call to the real tool. Useful to add permanent options.
__template() {
    template "$@"
}
export -f __template

#### the following functions are __required__ for externally provisioned clusters ####

# Usage: __get_template_current_cluster()
# set current_cluster variable as a name/string that other functions
# can use to reference a specific deployment.
__get_template_current_cluster() {
    # code
    # code
    current_cluster="foobarbaz"
}
export -f __get_template_current_cluster

# Usage: __get_template_kubeconf <profile>
# set KUBECOFIG env var to access profile specified by "${current_cluster}"
__get_template_kubeconf() {
    # code
    # code
    KUBECONFIG="$HOME/.template/${1}/auth/kubeconfig"
}
export -f __get_template_kubeconf

# Usage: __get_template_node_names <profile>
# set NODE=() array to cluster deployment node names. this is
# required to schedule some tests on specific nodes.
__get_template_nodes() {
    __get_template_node_names "$1"
    for node in $templatenodes; do
        NODES+=( "$node" )
    done
}
export -f __get_template_nodes

# Usage: __get_template_registry <profile>
# set ksanregistry to point to the registry of the cluster deployment.
# this is used to upload the locally built kubesan image and install it
# on the deployment.
__get_template_registry() {
    ksanregistry="example.com:5000"
}
export -f __get_template_registry

# Usage: __template_image_upload <profile> <image>
# command used to copy the locally built images to the deployment registry.
# NOTE that <image> has to match the registry path/name as it´s used during
# tests.
__template_image_upload() {
    # use profile to detect api IP?
    podman-template-example push --tls-verify=false "kubesan/${2}:test" example.com:5000/kubesan/${2}:test
}
export -f __template_image_upload

#### the following functions are __required__ for locally managed clusters ####

# Usage: __template_ssh <node> <command...>
# execute command on a remote node via ssh
__template_ssh() {
    __template ssh "$1" -- "
        set -o errexit -o pipefail -o nounset
        source .bashrc
        ${*:2}
        "
}
export -f __template_ssh

# Usage: __template_cluster_exists <suffix>
# check if a given deployment exists
__template_cluster_exists() {
    local __exit_code=0
    __template list clusters -o json | jq '."'$1'" // halt_error(1)' &>/dev/null || __exit_code="$?"

    case "${__exit_code}" in
    0)
        return 0
        ;;
    1)
        return 1
        ;;
    *)
        >&2 echo "template failed with exit code ${__exit_code}"
        exit "${__exit_code}"
        ;;
    esac
}
export -f __template_cluster_exists

# Usage: __is_template_cluster_running <profile>
# check status of a cluster and return 0 if running and ready to operate
# return 1 otherwise
__is_template_cluster_running() {
    local KUBECONFIG=""
    local kstatus=""

    if [[ -f $HOME/.template/clusters/$1/auth/kubeconfig ]] && \
       [[ "$(echo $templatenodes | wc -w)" == "${num_nodes}" ]]; then
        if [[ -z "$KUBECONFIG" ]]; then
            export KUBECONFIG=$HOME/.template/clusters/$1/auth/kubeconfig
        fi
        for node in $templatenodes; do
            if [[ $(__template info plan $1 -o json -q | jq -r '.[] | select(.name == "'$node'") | .status') != up ]]; then
                return 1
            fi
            kstatus=$(kubectl get nodes/$node --output=json 2>/dev/null | \
                      jq -r '.status.conditions[] | select(.reason == "KubeletReady") | .type')
            if [[ "${kstatus}" != "Ready" ]]; then
                return 1
            fi
        done
        if ! nc -z 192.168.122.253 5000; then
            return 1
        fi
        if [[ -n "$(kubectl get pods -A -o=json | jq -r '[.items][][].status | select(.phase != "Running") | select (.phase != "Succeeded")')" ]]; then
            return 1
        fi
        return 0
    fi
    return 1
}
export -f __is_template_cluster_running

# Usage: __start_template_cluster <profile> [<extra_template_opts...>]
# create/deploy a local cluster. it is wise to return once the cluster
# is fully operational and ready for use.
__start_template_cluster() {
    __template create cluster .... "$1"
}
export -f __start_template_cluster

# Usage: __stop_template_cluster [<extra_template_opts...>]
__stop_template_cluster() {
    __template stop plan "$@"
}
export -f __stop_template_cluster

# Usage: __restart_template_cluster <profile> [<extra_template_opts...>]
__restart_template_cluster() {
    __stop_template_cluster "$@"
    __start_template_cluster "$@"
}
export -f __restart_template_cluster

# Usage: __delete_template_cluster <profile>
__delete_template_cluster() {
    __template delete -y cluster "$1"
}
export -f __delete_template_cluster

# Usage: __snapshot_template_cluster <profile>
__snapshot_template_cluster() {
    __template snapshot -y cluster "$1"
}
export -f __snapshot_template_cluster

# Usage: __get_template_node_ip <profile> <node>
# get deplomyment nodes IP addresses, this is required only
# by nbd deploment to setup nbd-server.
__get_template_node_ip() {
    __template show plan "$1" -q -o json | jq -r '.[] | select(.name == "'$2'") | .ip'
}

export -f __get_template_node_ip

# Usage: __template_cp_bashrc <profile> <node>
# Provide a bash-profile for the nodes. This is required by nbd
# setup to execute containarized commands on the deployment nodes.
__template_cp_bashrc() {
    __template scp \
        "${script_dir}/deployers/template-node-bash-profile.sh" \
        "${2}:/home/fedora/.bashrc"
}
export -f __template_cp_bashrc

# Usage: __create_template_cluster_async <profile> [<extra_template_opts...>]
# Call to create a cluster deployment in background. Useful only
# when the deployer allows multiple clusters on the same machine.
__create_template_cluster_async() {
    return
}
export -f __create_template_cluster_async

# Usage: __wait_until_background_template_cluster_is_ready
# wait for background cluster (created via cluster_async above to be fully operational.
__wait_until_background_template_cluster_is_ready() {
    return
}
export -f __wait_until_background_template_cluster_is_ready

# Usage: ksan-template-ssh-into-node <node_name>|<node_index> [<command...>]
# provides ksan-ssh-into-node functionality for interactive shell.
# see also sandbox and --pause-* options
ksan-template-ssh-into-node() {
    if (( $# == 1 )); then
        # shellcheck disable=SC2154
        __template \
            ssh \
            "$( __ksan-get-node-name "$1" )" \
            -- \
            bash -i
    else
        local __args="${*:2}"
        __template \
            ssh \
            "$( __ksan-get-node-name "$1" )" \
            -- \
            bash -ic "${__args@Q}" bash
    fi
}
export -f ksan-template-ssh-into-node
