# botbox run on toy-widget: error

botbox v0.0.0-test ran 1 of 3 runs from seed 7 on envtest, with `--launch-arg --bug=3 --launch-arg '--name=a b'`. It took 58s of a derived deadline of 5m0s, and exited 2.

botbox: the derived deadline of 5m0s stopped the invocation after 1 of 3 runs

| run | seed | sequence | outcome | ops applied | faults applied | exits | took |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | 7 | drawn | passed | 3 of 3 | 1 of 1, to 2 requests | 1 | 21.5s |
| 2 | 8 | `sequences/a\|b.json` | not run | 0 of 3 | 0 of 1 | 0 |  |
| 3 | 9 | drawn | not run | 0 of 3 | 0 of 1 | 0 |  |

## Run 1: passed

- the target exited during op 1 (fault) with exit status 2 after writing "panic: lost the lease"
