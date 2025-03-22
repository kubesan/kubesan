#!/bin/bash
# SPDX-License-Identifier: Apache-2.0

export LC_ALL=C

set -o errexit -o pipefail -o nounset

if [[ -n "${kubesan_tests_run_sh_path:-}" ]]; then
    >&2 echo "You're already running $0"
    exit 1
fi

start_time="$( date +%s%N )" # Does not overflow 63 bits until 2262
script_dir="$( dirname "$(realpath -e "$0")")"
repo_root="$( realpath -e "${script_dir}/.." )"
initial_working_dir="${PWD}"

# parse usage

deploy_tool=kcli
ksanregistry=""
extregistry=""
fail_fast=0
mode=Thin
num_nodes=2
repeat=1
pause_on_failure=0
pause_on_stage=0
use_cache=0
refresh_cache=0
tests_arg=()

while (( $# > 0 )); do
    case "$1" in
        --fail-fast)
            fail_fast=1
            ;;
        --mode)
            shift
            mode=$1
            ;;
        --nodes)
            shift
            num_nodes=$1
            ;;
        --repeat)
            shift
            repeat=$1
            ;;
        --pause-on-failure)
            pause_on_failure=1
            ;;
        --pause-on-stage)
            # shellcheck disable=SC2034
            pause_on_stage=1
            ;;
        --use)
            shift
            deploy_tool=$1
            ;;
        --use-cache)
            use_cache=1
            ;;
        --refresh-cache)
            refresh_cache=1
            use_cache=1
            ;;
        --external-registry)
            shift
            extregistry=$1
            ;;
        *)
            tests_arg+=( "$1" )
            ;;
    esac
    shift
done

case $mode in
    [tT]hin) export mode=thin ;;
    [lL]inear) export mode=linear ;;
    *) echo "Invalid mode \"$mode\", must be \"Thin\" or \"Linear\""
       exit 1;;
esac

if [[ -f "${script_dir}/deployers/${deploy_tool}_iface.sh" ]]; then
    source "${script_dir}/deployers/${deploy_tool}_iface.sh"
    if (( requires_external_tool )); then
        # quick sanity check
        ( ${deploy_tool} --help ) >/dev/null 2>&1 ||
            { echo "skipping: ${deploy_tool} not installed" >&2; exit 77; }
    fi
else
    echo "Unknown deployment tool / method"
    exit 1
fi

if (( "${#tests_arg[@]}" == 0 )); then
    >&2 echo -n "\
Usage: $0 [<options...>] <tests...>
       $0 [<options...>] all
       $0 [<options...>] create-cache
       $0 [<options...>] delete-cache
       $0 [<options...>] sandbox
       $0 [<options...>] sandbox-no-install

Run each given test against a temporary cluster.

If invoked with a single \`all\` argument, all .sh files under t/ are run as
tests.

If invoked with a single \`sandbox\` argument, no tests are actually run but a
cluster is set up and an interactive shell is launched so you can play around
with it.

Options:
   --fail-fast             Cancel remaining tests after a test fails.
   --mode Thin|Linear      Select LVM LV type via StorageClass \"mode\" parameter (default: Thin).
   --nodes <n>             Number of nodes in the cluster (default: 2).
   --repeat <n>            Run each test n times (default: 1).
   --pause-on-failure      Launch an interactive shell after a test fails.
   --pause-on-stage        Launch an interactive shell before each stage in a test.
   --use <x>               Backend provider for k8s/openshift deployment (kcli|..., default: kcli).
   --use-cache             Use local cache when available (only supported for kcli).
   --refresh-cache         Refresh/create local cache when running tests directly (only supported for kcli).
   --external-registry     Use a dedicated registry to deploy kcli clusters (only supported for kcli).

"
    exit 2
fi

create_cache=0
delete_cache=0
sandbox=0
install_kubesan=0
uninstall_kubesan=0

if (( "${#tests_arg[@]}" == 1 )) && [[ "${tests_arg[0]}" = create-cache ]]; then
    use_cache=1
    create_cache=1
elif (( "${#tests_arg[@]}" == 1 )) && [[ "${tests_arg[0]}" = delete-cache ]]; then
    use_cache=1
    delete_cache=1
elif (( "${#tests_arg[@]}" == 1 )) && [[ "${tests_arg[0]}" = sandbox ]]; then
    sandbox=1
    install_kubesan=1
elif (( "${#tests_arg[@]}" == 1 )) && [[ "${tests_arg[0]}" = sandbox-no-install ]]; then
    sandbox=1
else
    install_kubesan=1
    uninstall_kubesan=1

    if (( "${#tests_arg[@]}" == 1 )) && [[ "${tests_arg[0]}" = all ]]; then
        tests_arg=()
        for f in "${script_dir}"/t/*.sh; do
            tests_arg+=( "$f" )
        done
    fi

    for test in "${tests_arg[@]}"; do
        if [[ ! -e "${test}" ]]; then
            >&2 echo "Test file does not exist: ${test}"
            exit 1
        fi
    done
fi

# sandbox support might not be available for all providers
if (( sandbox )) && ! (( support_sandbox )); then
    >&2 echo "${deploy_tool} does not support sandbox mode!"
    exit 1
fi

# cache support might not be available for all providers
if (( use_cache )) && ! (( requires_local_deploy && support_snapshots )); then
    >&2 echo "${deploy_tool} does not support cache mode!"
    exit 1
fi

tests=()
for test in "${tests_arg[@]}"; do
    for (( i = 0; i < repeat; ++i )); do
        tests+=( "$test" )
    done
done

# definitions shared with test scripts
export repo_root
export current_cluster
export deploy_tool

# source all helpers
for f in ${script_dir}/lib/*.sh; do
    # shellcheck disable=SC1090
    source "$f"
done

# this is only really required by CI to separate
# creation time vs testing time. does not support
# multiple clusters yet and pretty much works only
# with kcli

if (( use_cache )); then
    if (( create_cache )) || (( delete_cache )) || (( refresh_cache )); then
        __log_cyan "Deleting ${deploy_tool} cluster '%s'..." "${cluster_base_name}"
        __delete_${deploy_tool}_cluster "${cluster_base_name}"
        if (( delete_cache )); then
            exit 0
        fi
        __get_a_current_cluster
        __snapshot_${deploy_tool}_cluster "${cluster_base_name}"
        if ! (( refresh_cache )); then
            exit 0
        fi
    fi
fi

__build_images

# create temporary directory

temp_dir="$( mktemp -d )"
export temp_dir
trap 'rm -fr "${temp_dir}"' EXIT

# run tests

num_succeeded=0
num_failed=0
num_skipped=0

canceled=0
trap 'canceled=1' SIGINT

__run() {

    if [[ -e "${temp_dir}/retry" ]]; then
        rm -f "${temp_dir}/retry"
        __build_images
    fi

    # for external clusters, this function needs to be
    # ${deploy_tool} specific, and needs to set
    # current_cluster env var.
    #
    # for locally supported deployments, __get_a_current_cluster
    # will take care of wrapping call to deploy_tool to create
    # manage etc the cluster.
    #
    # from now on, each call to __<something> should understand
    # <current_cluster> to determine the target of operations.
    if (( requires_local_deploy )); then
        __get_a_current_cluster
    else
        __get_${deploy_tool}_current_cluster
    fi

    export NODES=()
    export KUBECONFIG=""

    # set KUBECONFIG=...
    __get_${deploy_tool}_kubeconf "${current_cluster}"
    # set NODES=()
    export NODES=()
    for node in $(kubectl get node -l node-role.kubernetes.io/worker --output=name); do
        NODES+=( "${node#node/}" )
    done
    export NODE_INDICES=( "${!NODES[@]}" )

    # set current_cluster registry required to install kubesan and test image
    __get_${deploy_tool}_registry "${current_cluster}"

    export TEST_IMAGE=${ksanregistry}/kubesan/test:test

    __log_cyan "Importing KubeSAN images into ${deploy_tool} cluster '%s'..." "${current_cluster}"
    for image in kubesan test; do
        # copy locally built image to remote registry
        __${deploy_tool}_image_upload "${current_cluster}" "${image}"
    done

    set +o errexit
    (
        set -o errexit -o pipefail -o nounset +o xtrace

        if (( install_kubesan )); then
            __log_cyan "Installing KubeSAN..."
            cat >${temp_dir}/kustomization.yaml <<EOF
resources:
  - $(realpath --relative-to=${temp_dir} ${repo_root})/deploy/kubernetes
images:
  - name: quay.io/kubesan/kubesan
    newName: ${ksanregistry}/kubesan/kubesan
    newTag: test
patches:
  # Slow down liveness probe to once a minute, instead of every 2 seconds
  - target:
      kind: Deployment
      name: csi-controller-plugin
    patch: |-
      - op: replace
        path: /spec/template/spec/containers/0/livenessProbe/periodSeconds
        value: 60
  - target:
      kind: DaemonSet
      name: csi-node-plugin
    patch: |-
      - op: replace
        path: /spec/template/spec/containers/0/livenessProbe/periodSeconds
        value: 60
EOF
            if (( requires_image_pull_policy_always )); then
                cat >>${temp_dir}/kustomization.yaml <<EOF
  - target:
      kind: Deployment
    patch: |-
      - op: add
        path: /spec/template/spec/containers/0/imagePullPolicy
        value: Always
  - target:
      kind: DaemonSet
    patch: |-
      - op: add
        path: /spec/template/spec/containers/0/imagePullPolicy
        value: Always
EOF
            fi
            kubectl apply -k ${temp_dir}
            kubectl apply -f "${script_dir}/t-data/storage-class.yaml"
        fi

        # If pods aren't healthy quickly, dump some logs before failing to
        # aid in debugging CI
        __log_cyan "Waiting for KubeSAN pods to be running..."
        if ksan-poll 1 240 '[[ -z "$(kubectl get --namespace kubesan-system pod --field-selector status.phase!=Running --no-headers --ignore-not-found)" ]]'; then
            :
        else
            __log_red "KubeSAN pods not healthy!"
            if (( ! sandbox )); then
                kubectl describe --namespace kubesan-system pod
                exit 1
            fi
        fi

        if (( sandbox )); then
            __log_cyan "Entering sandbox..."
            __shell 32 true
        else
            __log_cyan "Starting test $( basename "${test_resolved}" )..."
            set -o xtrace
            cd "$( dirname "${test_resolved}" )"
            # shellcheck disable=SC1090
            source "${test_resolved}"
            ksan-stage 'Finishing test...'
        fi
    )
    exit_code="$?"
    set -o errexit

    if [[ -e "${temp_dir}/retry" || -e "${temp_dir}/cancel" ]]; then

        # ksan-retry/ksan-cancel was run from a --pause-on-stage debug shell
        true

    elif (( exit_code == 0 )); then

        if (( uninstall_kubesan )); then
            __log_cyan "Waiting for KubeSAN to quiesce..."
            if [[ -n "$(kubectl get --namespace kubesan-system volume --ignore-not-found)" ]]; then
                __log_red 'Volumes leaked after test:'
                kubectl describe --namespace kubesan-system volume
		exit 1
            fi
            if [[ -n "$(kubectl get --namespace kubesan-system snapshot --ignore-not-found)" ]]; then
                __log_red 'Snapshots leaked after test:'
                kubectl describe --namespace kubesan-system snapshot
		exit 1
            fi
            if [[ -n "$(kubectl get --namespace kubesan-system nbdexport --ignore-not-found)" ]]; then
                __log_red 'NbdExports leaked after test:'
                kubectl describe --namespace kubesan-system nbdexport
		exit 1
            fi
            if ksan-poll 1 30 '[[ -z "$(kubectl get --namespace kubesan-system thinpoollv --ignore-not-found)" ]]'; then
                :
            else
                __log_red "ThinPoolLv leaked after Volume and Snapshot deletion"
                kubectl describe --namespace kubesan-system thinpoollv
                exit 1
            fi

            # __clean_cluster after test TBD for all deployers
            __log_cyan "Uninstalling KubeSAN..."
            kubectl delete --ignore-not-found --timeout=120s \
                -k "${repo_root}/deploy/kubernetes" \
                || exit_code="$?"

            if (( exit_code != 0 )); then
                __failure 'Failed to uninstall KubeSAN.'
            fi
        fi

    else

        if (( canceled )); then
            echo
            __canceled 'Test %s was canceled.' "${test_name}"
        else
            __failure 'Test %s failed.' "${test_name}"
        fi

    fi

    if (( requires_local_deploy )); then
        if (( use_cache )); then
            __log_cyan "Stopping ${deploy_tool} cluster '%s'..." "${current_cluster}"
            __stop_${deploy_tool}_cluster "${current_cluster}"
        else
            __log_cyan "Deleting ${deploy_tool} cluster '%s'..." "${current_cluster}"
            __delete_${deploy_tool}_cluster "${current_cluster}"
        fi
    fi
}

if (( sandbox )); then
    __big_log 33 'Starting sandbox cluster...'
    __run
    while [[ -e "${temp_dir}/retry" ]]; do
        __run
    done
else
    for (( test_i = 0; test_i < ${#tests[@]}; ++test_i )); do

        test="${tests[test_i]}"
        test_name="$( realpath --relative-to=. "${test}" )"
        test_resolved="$( realpath -e "${test}" )"

        __big_log 33 'Running test %s (%d of %d)...' \
            "${test_name}" "$(( test_i+1 ))" "${#tests[@]}"

        # hack to skip tests that don´t support current mode.
        testmodes="$(grep ^ksan-supported-modes ${test_resolved})"
        $testmodes
        exit_code=$?

        if (( exit_code == 0 )); then
            __run
        fi

        if [[ -e "${temp_dir}/retry" ]]; then
            canceled=0
            : $(( --test_i ))
        elif (( canceled )) || [[ -e "${temp_dir}/cancel" ]]; then
            break
        elif (( exit_code == 0 )); then
            : $(( num_succeeded++ ))
        elif (( exit_code == 77 )); then
            : $(( num_skipped++ ))
        else
            : $(( num_failed++ ))
            if (( fail_fast )); then
                break
            fi
        fi

    done

    # print summary

    num_canceled="$(( ${#tests[@]} - num_succeeded - num_failed - num_skipped ))"

    if (( num_failed > 0 )); then
        color=31 # red
    elif (( num_canceled > 0 )); then
        color=33 # yellow
    elif (( num_skipped > 0 )); then
        color=34 # blue
    else
        color=32 # green
    fi

    __big_log "${color}" '%d succeeded, %d failed, %d skipped, %d canceled' \
        "${num_succeeded}" "${num_failed}" "${num_skipped}" "${num_canceled}"
fi

(( sandbox || ( num_succeeded + num_skipped ) == ${#tests[@]} ))
