#!/bin/bash
# SPDX-License-Identifier: Apache-2.0

set -ex

# TODO: Must run wdmd as well.

host_hostname=$1

exec sanlock daemon -D -w 0 -U root -G root -e "$host_hostname"
