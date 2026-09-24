# botbox run on toy-widget dev: interrupted

botbox v0.0.0-test ran 2 of 3 runs from seed 7 on envtest, with `--launch-arg --bug=3`. It took 58s of the --deadline of 5m0s, and exited 130.

| run | seed | sequence | outcome | ops applied | faults applied | exits | took |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | 7 | drawn | passed | 3 of 3 | 1 of 1, to 2 requests | 1 | 21.5s |
| 2 | 8 | drawn | interrupted | 3 of 3 | 0 of 1 | 0 | 31s |
| 3 | 9 | drawn | not run | 0 of 3 | 0 of 1 | 0 |  |

## Run 1: passed

- the target exited during op 1 (fault) with exit status 2 after writing "panic: lost the lease"

## Run 2: interrupted

an interrupt stopped the run

`run-2/` holds the run's files.
