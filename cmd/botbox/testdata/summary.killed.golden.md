# botbox run on toy-widget dev: unfinished

botbox v0.0.0-test ran 2 of 3 runs from seed 7 on envtest, with `--launch-arg --bug=3 --launch-arg '--name=a b'`. It did not finish.

| run | seed | sequence | outcome | ops applied | faults applied | exits | took |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | 7 | drawn | passed | 3 of 3 | 1 of 1, to 2 requests | 1 | 21.5s |
| 2 | 8 | drawn | unfinished | 0 of 3 | 0 of 1 | 0 |  |
| 3 | 9 | drawn | not run | 0 of 3 | 0 of 1 | 0 |  |

## Run 1: passed

- the target exited during op 1 (fault) with exit status 2 after writing "panic: lost the lease"
