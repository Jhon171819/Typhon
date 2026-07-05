# Python `.pyc` Bridge Benchmark

- Modules: `requests urllib3 certifi charset_normalizer idna`
- Compiled files: `80`

| Scenario | Median ms |
|---|---:|
| Before `.pyc` precompile | 981.116 |
| After `.pyc` precompile | 1023.789 |

Delta: `+42.673 ms` (`+4.35%`).

Notes:
- `.pyc` can reduce Python module parse/compile time.
- It does not remove CPython startup, subprocess IPC, import execution, or Typhon/Python value marshalling.
- Results are noisy for short CLI runs; rerun with more samples for higher confidence.
