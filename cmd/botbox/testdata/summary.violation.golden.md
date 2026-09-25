# botbox run on toy-widget dev: violation

botbox v0.0.0-test ran 2 of 3 runs from seed 7 on envtest, with `--launch-arg --bug=3 --launch-arg '--name=a b'`. It took 58s of the --deadline of 5m0s, and exited 1.

| run | seed | sequence | outcome | ops applied | faults applied | exits | took |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | 7 | drawn | passed | 3 of 3 | 1 of 1, to 2 requests | 1 | 21.5s |
| 2 | 8 | drawn | G3 | 3 of 3 | 0 of 1 | 0 | 31s |
| 3 | 9 | drawn | not run | 0 of 3 | 0 of 1 | 0 |  |

## Run 1: passed

- the target exited during op 1 (fault) with exit status 2 after writing "panic: lost the lease"

## Run 2: G3

the target deletes what it manages once the CR is deleted

the v1/ConfigMap widget-1 was still there 10s after the CR was deleted

`run-2/` holds the report and the evidence.

- the proxy applied the fault of op 1 to no request
- P1 "status.ready <= 3 && has(spec)" is not evaluated
