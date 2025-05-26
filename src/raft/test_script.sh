#!/bin/bash

ITER=1
while true; do
  echo "=== Running TestFailNoAgree3B Iteration $ITER ==="
  VERBOSE=1 go test -run TestFailNoAgree3B -race > raft.log 2>&1

  if grep -q FAIL raft.log; then
    echo "!!! Test failed on iteration $ITER. Dumping logs with ./dslogs -c 3"
    ./dslogs -c 5 < raft.log
    break
  else
    echo "--- Test passed ---"
  fi

  ITER=$((ITER + 1))
done
