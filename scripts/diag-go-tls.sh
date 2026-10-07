#!/bin/bash
# Temporary diagnostics for PR #187. Not for merge.
cd internal/edc
echo "THP enabled: $(cat /sys/kernel/mm/transparent_hugepage/enabled)"
echo "THP defrag: $(cat /sys/kernel/mm/transparent_hugepage/defrag)"
echo "khugepaged defrag: $(cat /sys/kernel/mm/transparent_hugepage/khugepaged/defrag)"
echo "numa_balancing: $(cat /proc/sys/kernel/numa_balancing 2>/dev/null) nodes: $(ls -d /sys/devices/system/node/node* | wc -l)"
nproc
counters='^(thp_collapse_alloc|thp_fault_alloc|pgmigrate_success|numa_hint_faults|compact_success|thp_split_pmd|pgmajfault) '
loop() {
  label=$1
  for i in $(seq 1 20); do
    before=$(grep -E "$counters" /proc/vmstat)
    sudo env "PATH=$PATH" EDC_TEST_GO="$(command -v go)" ../../edc.test -test.run 'TestTraceTLSGoCaptures/normal--0' -test.count=1 -test.v > run.log 2>&1
    after=$(grep -E "$counters" /proc/vmstat)
    delta=$(paste <(echo "$before") <(echo "$after") | awk '{d=$4-$2; if (d) printf "%s=+%d ", $1, d}')
    result=$(grep -oE -- '^    --- (PASS|FAIL)' run.log | head -1 | awk '{print $2}')
    lines=$(grep -E '^ZZLINE' run.log | tr '\n' ' ')
    echo "$label run $i: $result $lines| $delta"
  done
}
loop default
echo never | sudo tee /sys/kernel/mm/transparent_hugepage/enabled > /dev/null
echo never | sudo tee /sys/kernel/mm/transparent_hugepage/defrag > /dev/null
echo "THP enabled now: $(cat /sys/kernel/mm/transparent_hugepage/enabled)"
loop thp-never
