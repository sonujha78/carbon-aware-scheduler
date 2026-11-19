#!/usr/bin/env bash
# End-to-end check against a cluster that already has the CRD, mock API and controller deployed.
set -euo pipefail
NS=carbon-system

set_intensity() {
  kubectl -n "$NS" exec deploy/mock-carbon-api -- wget -qO- "http://localhost:8080/set?value=$1" > /dev/null
}

suspended() { kubectl get job "$1" -o jsonpath='{.spec.suspend}'; }

wait_suspend() { # job, wanted value, timeout seconds
  local job=$1 want=$2 timeout=${3:-90}
  for ((i = 0; i < timeout; i += 3)); do
    if [[ "$(suspended "$job")" == "$want" ]]; then
      echo "ok: $job suspend=$want"
      return 0
    fi
    sleep 3
  done
  echo "FAIL: $job did not reach suspend=$want"
  kubectl -n "$NS" logs deploy/carbon-controller --tail=40
  return 1
}

echo "== test 1: dirty grid keeps a deferrable job suspended"
set_intensity 400
kubectl apply -f examples/deferrable-job.yaml
sleep 25
[[ "$(suspended nightly-report)" == "true" ]] || { echo "FAIL: job should still be suspended"; exit 1; }
echo "ok: job stays suspended at 400 gCO2/kWh"

echo "== test 2: clean grid resumes the job"
set_intensity 150
wait_suspend nightly-report false 60

echo "== test 3: deadline forces a run on a dirty grid"
kubectl delete job nightly-report --ignore-not-found
set_intensity 400
sed 's/name: nightly-report/name: forced-report/' examples/deferrable-job.yaml | kubectl apply -f -
kubectl annotate job forced-report \
  "green.devops/deadline=$(date -u -d '+30 seconds' +%Y-%m-%dT%H:%M:%SZ)" --overwrite
wait_suspend forced-report false 60
kubectl -n "$NS" logs deploy/carbon-controller | grep -q "deadline forced" \
  || { echo "FAIL: no 'deadline forced' log line"; exit 1; }
echo "ok: deadline-forced run logged"

echo "ALL E2E TESTS PASSED"
