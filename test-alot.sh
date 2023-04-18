#!/usr/bin/env bash
# set -euo pipefail

for ((i = 0 ; i < 5 ; i++)); do

    ARTIFACT=./artifacts/ go test --tags=integ ./tests/integration/pilot/ -run ^TestWorkloadEntry$ -p 1 --istio.test.kube.topology $(pwd)/prow/config/topology/multicluster.json --istio.test.pullpolicy IfNotPresent --istio.test.skipWorkloads="vm,proxyless,headless,tproxy,naked,statefulset,delta"

done

echo "done"