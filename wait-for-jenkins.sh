#!/bin/bash

set -e

# Dump the environment, but escaped into one line per variable
env -0 | sed -z 's/\\/\\\\/g; s/\n/\\n/g' | tr '\0' '\n' | sort

# for both push events and MR within the same repository
# CI_COMMIT_SHA contains the commit that needs to be monitored
# and will be built by Jenkins.
commit="$CI_COMMIT_SHA"

# CI_MERGE_REQUEST_SOURCE_BRANCH_SHA is set only when an MR is
# created or updated from a fork of the main repository and
# jenkins pipeline is triggered on the original repo.
if [ -n "$CI_MERGE_REQUEST_SOURCE_BRANCH_SHA" ]; then
	commit="$CI_MERGE_REQUEST_SOURCE_BRANCH_SHA"
fi

if [ -z "$commit" ]; then
	echo "Gitlab commit not detected!"
	exit 1
fi

# 20 hours (72000 seconds)
timeout=72000
# 5 minutes
sleeptime=300

echo "[$(date)] Waiting for Jenkins to start building commit $commit (timeout: $timeout seconds)"

while ! wget -q -O /tmp/res https://ci.kronosnet.org/.kubesan/$commit; do
	# check every 5 minutes
	sleep $sleeptime
	timeout=$(( timeout - $sleeptime ))
	if [ "$timeout" -le "0" ]; then
		echo "[$(date)] Timeout waiting for Jenkins to start building!"
		exit 1
	fi
	echo "[$(date)] Waiting for Jenkins to start building ($timeout seconds remaining)"
done

# this relies on https://github.com/kronosnet/ci-tools/blob/main/ci-gitlab-jenkins-gw
# generating proper shell code that can be sourced by the gitlab runner.
. /tmp/res

# 3 hours to complete the build
timeout=10800

echo "[$(date)] Jenkins build progress: $jenkins_build_url ($timeout seconds remaining)"

while [ "$jenkins_stage" != "completed" ]; do
	sleep $sleeptime
	timeout=$(( timeout - $sleeptime ))
	if [ "$timeout" -le "0" ]; then
		echo "[$(date)] Timeout waiting for Jenkins to complete building!"
		exit 1
	fi
	echo "[$(date)] Waiting for Jenkins to complete the building ($timeout seconds remaining)"
	wget -q -O /tmp/res https://ci.kronosnet.org/.kubesan/$commit
	. /tmp/res
done

echo "[$(date)] Jenkins build completed: $jenkins_ret"

ret=0

case $jenkins_ret in
	SUCCESS)
		ret=0
	;;
	FAILED)
		ret=1
	;;
	*)
		echo "Unknown Jenkins return value $jenkins_ret!"
		exit 1
	;;
esac

exit $ret
